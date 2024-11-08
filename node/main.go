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

var (
	serviceReg         = NewServiceRegistry()   // service registry for discovery
	csMutex            = sync.Mutex{}           // mutex for critical section
	csRequest          = []int32{}              // queue of requests for critical section
	csUsed             = false                  // if critical section is in use
	heartbeatTimeout   = 4 * time.Second        // max allowed interval between heartbeats
	heartbeatInterval  = 1 * time.Second        // cooldown between heartbeats
	rpcTimeout         = 200 * time.Millisecond // timeouts for rpc-calls using context.withTimeout()
	randDeviationBound = 150                    // upper bound for random time deviation used in nowWithDeviation() (ms)
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
	isDead        bool // Gotta figure out how to actually stop the server
}

func NewNode(id int32, address string) *Node {
	return &Node{
		id:            id,
		address:       address,
		peers:         make(map[int32]pb.NodeClient),
		leader:        -1, // no leader when starting
		term:          0,
		lastHeartbeat: nowWithDeviation(),
		isDead:        false,
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

	go n.nodeLogic()

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

func (n *Node) nodeLogic() {
	for {
		if !n.isLeader() {
			if time.Since(n.lastHeartbeat) > heartbeatTimeout {
				n.callElection()
			}
			continue
		}

		if time.Since(n.lastHeartbeat) < heartbeatInterval {
			continue
		}

		// 1/10 chance of dying
		if rand.IntN(10) == 1 {
			n.isDead = true
			fmt.Printf("%d died\n", n.id)
			return
		}

		// Send heartbeat
		for _, peer := range n.peers {
			peer.TransmitHeartbeat(context.Background(), &pb.Request{Sender: n.id, Term: n.term})
		}

		n.lastHeartbeat = nowWithDeviation()
	}
}

func (n *Node) isLeader() bool {
	return n.leader == n.id
}

func (n *Node) callElection() {
	n.mu.Lock()
	defer n.mu.Unlock()

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

	if n.isDead {
		return &pb.Reply{Granted: false}, nil
	}

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

func (n *Node) TransmitHeartbeat(_ context.Context, in *pb.Request) (*pb.Empty, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.isDead {
		return &pb.Empty{}, nil
	}
	if n.term > in.Term {
		fmt.Printf("%d: The heartbeat was bad. Got term: %d, expected term: %d\n", n.id, in.Term, n.term)
		return &pb.Empty{}, nil
	}

	n.leader = in.Sender
	n.term = in.Term
	n.lastHeartbeat = nowWithDeviation()

	fmt.Printf("%d: has recieved heartbeat from %d in term %d\n", n.id, in.Sender, n.term)

	return &pb.Empty{}, nil
}

func (n *Node) RequestAccess(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	if !n.isLeader() {
		return &pb.Reply{Granted: false}, nil
	}
	if csUsed {
		csRequest = append(csRequest, in.Sender) // add node to queue
		return &pb.Reply{Granted: false}, nil
	}

	csMutex.Lock()
	csUsed = true
	fmt.Println("CS is now in use")
	csMutex.Unlock()
	return &pb.Reply{Granted: true}, nil
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
