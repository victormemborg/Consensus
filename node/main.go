package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	pb "github.com/victormemborg/Consensus/grpc"
	"google.golang.org/grpc"
)

var (
	serviceRegistry   = make(map[int32]string) // id -> address
	registryMutex     = sync.Mutex{}           // mutex for registry
	csMutex           = sync.Mutex{}           // mutex for critical section
	csRequest         = []int32{}              // queue of requests for critical section
	csUsed            = false                  // if critical section is in use
	heartbeatTimeout  = 10 * time.Second       // seconds between heartbeats
	heartbeatInterval = 1 * time.Second
)

type Node struct {
	pb.NodeServer
	id            int32
	address       string
	peers         map[int32]pb.NodeClient // id -> client
	leader        int32
	term          int32
	lastHeartbeat time.Time
}

func NewNode(id int32, address string) *Node {
	return &Node{
		id:            id,
		address:       address,
		peers:         make(map[int32]pb.NodeClient),
		leader:        -1, // no leader when starting
		term:          0,
		lastHeartbeat: time.Now(),
	}
}

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

func (n *Node) nodeLogic() {
	for {
		if !n.isLeader() {
			if time.Since(n.lastHeartbeat) < heartbeatTimeout {
				continue
			}

			fmt.Printf("%d is calling an election\n", n.id)
			n.leader = -1
			n.callElection()

		} else {
			if time.Since(n.lastHeartbeat) < heartbeatInterval {
				continue
			}

			// 1/10 chance of dying
			if rand.IntN(10) == 1 {
				fmt.Printf("%d died\n", n.id)
				return
			}

			n.TransmitHeartbeat()
		}
	}
}

func (n *Node) isLeader() bool {
	return n.leader == n.id
}

func (n *Node) TransmitHeartbeat() {
	for _, peer := range n.peers {
		reply, err := peer.RequestVote(context.Background(), &pb.Request{Sender: n.id, Term: n.term})

	}

	n.lastHeartbeat = time.Now()
}

func (n *Node) callElection() {
	majority := len(n.peers)/2 + 1
	nVotes := 1 // votes for self as leader
	n.term++

	for _, peer := range n.peers {
		reply, err := peer.RequestVote(context.Background(), &pb.Request{Sender: n.id, Term: n.term})
		if reply.Granted && err == nil {
			nVotes++
		}
	}

	if nVotes >= majority {
		n.leader = n.id
		fmt.Printf("Node %d has won its election\n", n.id)
	}

	// randomize
	n.lastHeartbeat = time.Now().Add(time.Duration(rand.IntN(150)) * time.Millisecond)

}

func (n *Node) RequestVote(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	if n.term > in.Term {
		fmt.Printf("%d: The request was bad. Got term: %d, expected term: %d\n", n.id, in.Term, n.term)
		return &pb.Reply{Granted: true}, nil
	}
	if n.term == in.Term && n.leader > in.Sender {
		fmt.Printf("%d: The request was bad. Got id: %d, expected id: %d\n", n.id, in.Sender, n.leader)
		return &pb.Reply{Granted: true}, nil
	}

	n.leader = in.Sender
	n.term = in.Term
	n.lastHeartbeat = time.Now()
	fmt.Printf("%d: current term: %d\n", n.id, n.term)

	return &pb.Reply{Granted: true}, nil
}

func (n *Node) SendHeartbeat(_ context.Context, in *pb.Request) (*pb.Empty, error) {
	fmt.Printf("%d recieved a heartbeat from %d\n", n.id, in.Sender)

	if n.term > in.Term {
		// Bad leader. Ignore
		fmt.Printf("%d: The leader was bad. Got term: %d, expected term: %d\n", n.id, in.Term, n.term)
		return &pb.Empty{}, nil
	}
	if n.term == in.Term && n.leader > in.Sender {
		// Bad leader. Ignore
		fmt.Printf("%d: The leader was bad. Got id: %d, expected id: %d\n", n.id, in.Sender, n.leader)
		return &pb.Empty{}, nil
	}

	n.isLeader = false
	n.leader = in.Sender
	n.term = in.Term
	n.lastHeartbeat = time.Now()
	fmt.Printf("%d: current term: %d\n", n.id, n.term)

	return &pb.Empty{}, nil
}

func (n *Node) registerService() error {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	for k, v := range serviceRegistry {
		n.addPeer(k, v)
	}

	info := &pb.NodeInfo{Id: n.id, Address: n.address}

	for _, peer := range n.peers {
		_, err := peer.InformArrival(context.Background(), info)
		if err != nil {
			fmt.Printf("failed to inform of arrival %v\n", err)
		}
	}

	serviceRegistry[n.id] = n.address
	return nil
}

func (n *Node) addPeer(id int32, address string) {
	conn, err := grpc.NewClient(address, grpc.WithInsecure())
	if err != nil {
		fmt.Printf("Unable to connect to %s: %v\n", address, err)
	}

	n.peers[id] = pb.NewNodeClient(conn)
}

func main() {
	n1 := NewNode(1, ":50051")
	n2 := NewNode(2, ":50052")
	n3 := NewNode(3, ":50053")

	wg := sync.WaitGroup{}
	wg.Add(1)

	go n1.start()
	go n2.start()
	go n3.start()

	time.Sleep(2 * time.Second) // we need to wait for the nodes to start
	wg.Wait()
}

func (n *Node) InformArrival(_ context.Context, in *pb.NodeInfo) (*pb.Empty, error) {
	n.addPeer(in.Id, in.Address)
	return &pb.Empty{}, nil
}

func (n *Node) ElectLeader(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	fmt.Printf("%d modtog en besked fra %d\n", n.id, in.Sender)
	return &pb.Reply{Granted: true, Message: ""}, nil
}

func (n *Node) RequestAccess(_ context.Context, in *pb.NodeInfo) (*pb.Reply, error) {
	if !n.isLeader {
		return &pb.Reply{Granted: false, Message: "Node is not the leader"}, nil
	}
	if csUsed {
		csRequest = append(csRequest, in.Id) // add node to queue
		return &pb.Reply{Granted: false, Message: "CS is already in use"}, nil
	}

	csMutex.Lock()
	csUsed = true
	fmt.Println("CS is now in use")
	csMutex.Unlock()
	return &pb.Reply{Granted: true, Message: "granted access"}, nil
}
