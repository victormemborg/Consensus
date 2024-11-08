package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"

	pb "github.com/victormemborg/Consensus/grpc"
	"google.golang.org/grpc"
)

type Queue struct {
	items []int32
}

func NewQueue() *Queue {
	return &Queue{
		items: make([]int32, 0),
	}
}

func (q *Queue) Enqueue(id int32) {
	q.items = append(q.items, id)
}

func (q *Queue) Dequeue() int32 {
	if len(q.items) == 0 {
		return -1 // empty queue
	}
	value := q.items[0]
	q.items = q.items[1:]
	return value
}

var (
	serviceReg         = NewServiceRegistry()   // service registry for discovery
	heartbeatTimeout   = 4 * time.Second        // max allowed interval between heartbeats
	heartbeatInterval  = 1 * time.Second        // cooldown between heartbeats
	rpcTimeout         = 200 * time.Millisecond // timeouts for rpc-calls using context.withTimeout()
	randDeviationBound = 150                    // upper bound for random time deviation used in nowWithDeviation() (ms)
	accessDuration     = 5 * time.Second        // time to hold the critical resource (s) NOTE: Must be lower than accessTimeout
	accessTimeout      = 7 * time.Second        // if no response from holder, assume resource is free
)

type ServiceRegistry struct {
	mu       sync.Mutex
	services map[int32]string // id -> address
}

func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[int32]string),
	}
}

type Node struct {
	pb.NodeServer
	mu            sync.Mutex
	id            int32
	address       string
	peers         map[int32]pb.NodeClient // id -> client
	leader        int32
	term          int32
	lastHeartbeat time.Time
	csQueue       *Queue
	csIsUsedBy    int32
	isWaiting     bool
	lastCSAccess  time.Time
}

func NewNode(id int32, address string) *Node {
	return &Node{
		id:            id,
		address:       address,
		peers:         make(map[int32]pb.NodeClient),
		leader:        -1, // no leader when starting
		term:          0,
		lastHeartbeat: nowWithDeviation(),
		csQueue:       NewQueue(),
		csIsUsedBy:    -1,
		isWaiting:     false,
		lastCSAccess:  time.Now(),
	}
}

func nowWithDeviation() time.Time {
	nanoSeconds := rand.IntN(randDeviationBound) * 1000000
	return time.Now().Add(time.Duration(nanoSeconds))
}

///////////////////////////////////////////////////////
//                    Networking                     //
///////////////////////////////////////////////////////

func (n *Node) start() {
	grpcServer := grpc.NewServer()
	listener, err := net.Listen("tcp", n.address)
	if err != nil {
		fmt.Printf("Unable to start connection to server: %v\n", err)
	}
	fmt.Printf("server listening at %v\n", listener.Addr())

	pb.RegisterNodeServer(grpcServer, n)
	if err := n.registerService(); err != nil {
		fmt.Printf("Node %d failed to register\n", n.id)
	}

	go n.nodeLogic(grpcServer)

	if grpcServer.Serve(listener) != nil {
		fmt.Printf("failed to serve: %v\n", err)
	}
}

func (n *Node) registerService() error {
	serviceReg.mu.Lock()
	defer serviceReg.mu.Unlock()

	for k, v := range serviceReg.services {
		n.addPeer(k, v)
	}

	info := &pb.NodeInfo{Id: n.id, Address: n.address}

	for _, peer := range n.peers {
		_, err := peer.InformArrival(context.Background(), info)
		if err != nil {
			fmt.Printf("failed to inform of arrival %v\n", err)
		}
	}

	serviceReg.services[n.id] = n.address
	return nil
}

func (n *Node) addPeer(id int32, address string) {
	conn, err := grpc.NewClient(address, grpc.WithInsecure())
	if err != nil {
		fmt.Printf("Unable to connect to %s: %v\n", address, err)
	}

	n.peers[id] = pb.NewNodeClient(conn)
}

///////////////////////////////////////////////////////
//                       Logic                       //
///////////////////////////////////////////////////////

func (n *Node) nodeLogic(srv *grpc.Server) {
	for {
		n.mu.Lock()

		if !n.isLeader() {

			if !n.isWaiting && n.leader != -1 {
				n.requestResourceFromLeader()
			}

			if time.Since(n.lastHeartbeat) > heartbeatTimeout {
				n.callElection()
			}

			n.mu.Unlock()
			continue
		}

		if time.Since(n.lastCSAccess) > accessTimeout {
			fmt.Printf("%d: Resource access timed out\n", n.id)
			n.csIsUsedBy = -1
		}

		if n.csIsUsedBy == -1 {
			n.informAccessToNext()
		}

		if time.Since(n.lastHeartbeat) < heartbeatInterval {
			n.mu.Unlock()
			continue
		}

		// 1/10 chance of dying
		if rand.IntN(10) == 1 {
			fmt.Printf("%d died\n", n.id)
			srv.Stop()
			n.mu.Unlock()
			return
		}

		n.heartbeat()
		n.lastHeartbeat = nowWithDeviation()
		n.mu.Unlock()
	}
}

func (n *Node) isLeader() bool {
	return n.leader == n.id
}

func (n *Node) requestResourceFromLeader() {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	req := &pb.Request{Sender: n.id}
	defer cancel()

	reply, err := n.peers[n.leader].RequestResource(ctx, req)
	if err == nil && reply.Granted {
		n.isWaiting = true
	}
}

func (n *Node) informAccessToNext() {
	nextId := n.csQueue.Dequeue()

	if nextId == -1 || n.peers[nextId] == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	req := &pb.Empty{}
	defer cancel()

	_, err := n.peers[nextId].InformAccess(ctx, req)
	if err != nil {
		return
	}

	n.csIsUsedBy = nextId
	n.lastCSAccess = time.Now()
}

func (n *Node) heartbeat() {
	majority := len(n.peers)/2 + 1
	nSuccess := 1 // heartbeat to self always succesfull

	req := &pb.HeartBeat{Sender: n.id, Term: n.term, Queue: n.csQueue.items, CsIsUsedBy: n.csIsUsedBy}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	for _, peer := range n.peers {
		reply, err := peer.TransmitHeartbeat(ctx, req)
		if err != nil {
			continue
		}
		if !reply.Granted {
			n.leader = -1
			// Maybe increase own term
			return
		}
		nSuccess++
	}

	if nSuccess < majority {
		n.leader = -1
	}
}

func (n *Node) callElection() {
	fmt.Printf("%d is calling an election in term %d\n", n.id, n.term+1)

	majority := len(n.peers)/2 + 1
	nVotes := 1   // votes for self as leader
	n.leader = -1 // no longer has a leader
	n.term++

	req := &pb.Request{Sender: n.id, Term: n.term}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	for _, peer := range n.peers {
		reply, err := peer.RequestVote(ctx, req)
		if err == nil && reply.Granted {
			nVotes++
		}
	}

	if nVotes >= majority {
		n.leader = n.id
		fmt.Printf("Node %d has won its election in term %d\n", n.id, n.term)
	} else {
		fmt.Printf("Node %d has lost its election in term %d\n", n.id, n.term)
	}

	n.lastHeartbeat = nowWithDeviation()
}

func (n *Node) holdResource() {
	fmt.Printf("%d: Im using the critical resource!!\n", n.id)
	time.Sleep(accessDuration)
	fmt.Printf("%d: I have released the critical resource!!\n", n.id)

	for {
		n.mu.Lock()
		if n.leader == -1 || n.peers[n.leader] == nil {
			n.mu.Unlock()
			break
		}
		_, err := n.peers[n.leader].InformRelease(context.Background(), &pb.Request{Sender: n.id})
		if err == nil {
			n.mu.Unlock()
			break
		}
		fmt.Printf("%d: Failed to inform release, retrying...\n", n.id)
		n.mu.Unlock()

		time.Sleep(500 * time.Millisecond) // wait before retrying
	}
}

///////////////////////////////////////////////////////
//                rpc-implementations                //
///////////////////////////////////////////////////////

func (n *Node) InformArrival(_ context.Context, in *pb.NodeInfo) (*pb.Empty, error) {
	n.addPeer(in.Id, in.Address)
	return &pb.Empty{}, nil
}

func (n *Node) RequestVote(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.term > in.Term {
		fmt.Printf("%d: Request from %d is bad. Got term: %d, expected term: %d\n", n.id, in.Sender, in.Term, n.term)
		return &pb.Reply{Granted: false}, nil
	}
	if n.term == in.Term && n.leader != -1 {
		fmt.Printf("%d: Request from %d is bad. Already voted %d in this term %d\n", n.id, in.Sender, n.leader, n.term)
		return &pb.Reply{Granted: false}, nil
	}

	n.leader = in.Sender
	n.term = in.Term
	n.lastHeartbeat = nowWithDeviation()

	fmt.Printf("%d: has granted a vote to %d in term %d\n", n.id, in.Sender, n.term)

	return &pb.Reply{Granted: true}, nil
}

func (n *Node) TransmitHeartbeat(_ context.Context, in *pb.HeartBeat) (*pb.Reply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.term > in.Term {
		fmt.Printf("%d: The heartbeat was bad. Got term: %d, expected term: %d\n", n.id, in.Term, n.term)
		return &pb.Reply{Granted: false}, nil
	}

	n.leader = in.Sender
	n.term = in.Term
	n.csQueue.items = in.Queue
	n.csIsUsedBy = in.CsIsUsedBy
	n.lastHeartbeat = nowWithDeviation()

	//fmt.Printf("%d: has recieved heartbeat from %d in term %d\n", n.id, in.Sender, n.term)

	return &pb.Reply{Granted: true}, nil
}

func (n *Node) RequestResource(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.isLeader() {
		return &pb.Reply{Granted: false}, nil
	}

	n.csQueue.Enqueue(in.Sender)
	return &pb.Reply{Granted: true}, nil
}

func (n *Node) InformAccess(_ context.Context, in *pb.Empty) (*pb.Empty, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.isWaiting = false
	go n.holdResource()
	return &pb.Empty{}, nil
}

func (n *Node) InformRelease(_ context.Context, in *pb.Request) (*pb.Empty, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.csIsUsedBy == in.Sender {
		n.csIsUsedBy = -1
	}

	return &pb.Empty{}, nil
}

///////////////////////////////////////////////////////
//                       Main                        //
///////////////////////////////////////////////////////

func main() {
	nNodes := 5
	nodeArray := make([]*Node, nNodes)

	for i := 0; i < nNodes; i++ {
		port := 50051 + i
		nodeArray[i] = NewNode(int32(i), ":"+strconv.Itoa(port))
	}

	wg := sync.WaitGroup{}
	wg.Add(1)

	for i := 0; i < nNodes; i++ {
		go nodeArray[i].start()
	}

	time.Sleep(2 * time.Second) // we need to wait for the nodes to start
	wg.Wait()
}
