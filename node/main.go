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
	isLeader      bool                    //leader, follower, candidate
	leader        int32
	term          int32
	lastHeartbeat time.Time
	votedFor      int32 //used to keep track of who the node voted for
}

func NewNode(id int32, address string) *Node {
	return &Node{
		id:            id,
		address:       address,
		peers:         make(map[int32]pb.NodeClient),
		isLeader:      false,
		leader:        -1, // no leader when starting
		votedFor:      -1, // no vote when starting
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
		if !n.isLeader {
			if time.Since(n.lastHeartbeat) > heartbeatTimeout {
				fmt.Printf("%d is calling an election\n", n.id)
				n.leader = -1
				n.handleElection()
			}
		} else {
			if time.Since(n.lastHeartbeat) > heartbeatInterval {
				if rand.IntN(10) == 1 { // 1/10 chance of dying
					fmt.Printf("%d died\n", n.id)
					return
				}
				// Transmit heartbeat
				for _, peer := range n.peers {
					peer.SendHeartbeat(context.Background(), &pb.NodeInfo{Id: n.id, Address: n.address, Term: n.term})
				}
				n.lastHeartbeat = time.Now()
			}
		}
	}
}

func (n *Node) handleElection() {
	majority := len(n.peers)/2 + 1
	nVotes := 1 // votes for self as leader
	n.term++
	n.votedFor = n.id // vote for self

	for peerId, peer := range n.peers {
		if n.id > peerId {
			reply, err := peer.RequestVote(context.Background(), &pb.Request{Sender: n.id, Term: n.term})
			if reply.Granted && err == nil {
				nVotes++
			}
		}
	}
	if nVotes >= majority {
		n.isLeader = true
		n.leader = n.id
		fmt.Printf("Node %d is the new leader\n", n.id)
		// broadcast new leader
	}
	// randomize election timeout
	n.lastHeartbeat = time.Now().Add(time.Duration(rand.IntN(150)) * time.Millisecond)

}

func (n *Node) RequestVote(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	// if in.term < n.term -> false
	// if in.id < n.id -> false
	// if in.id < n.votedfor -> false
	// else -> true + n.votedfor = in.id

	if in.Term < n.term {
		return &pb.Reply{Granted: false, Message: "Vote denied: Old term"}, nil
	}
	if in.Sender < n.id {
		return &pb.Reply{Granted: false, Message: "Vote denied: Lower id"}, nil
	}
	if in.Sender < n.votedFor {
		return &pb.Reply{Granted: false, Message: "Vote denied: Lower id than other candidate"}, nil
	}

	n.term = in.Term
	n.votedFor = in.Sender

	return &pb.Reply{Granted: true, Message: "Vote granted"}, nil
}

func (n *Node) SendHeartbeat(_ context.Context, in *pb.NodeInfo) (*pb.Empty, error) {
	// if n.term > in.Term -> return
	// if n.leader > in.Id -> return
	// else -> n.leader = in.Id + n.term = in.Term

	fmt.Printf("%d recieved a heartbeat from %d\n", n.id, in.Id)

	if n.term > in.Term || n.leader > in.Id {
		// Bad leader. Ignore
		fmt.Printf("%d: The leader was bad. Got: %d, expected: %d\n", n.id, in.Id, n.leader)
		return &pb.Empty{}, nil
	}

	n.isLeader = false
	n.leader = in.Id
	n.term = in.Term
	n.lastHeartbeat = time.Now()

	return &pb.Empty{}, nil
}

func (n *Node) registerService() error {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	for k, v := range serviceRegistry {
		n.addPeer(k, v)
	}

	info := &pb.NodeInfo{Id: n.id, Address: n.address, Term: n.term}

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
