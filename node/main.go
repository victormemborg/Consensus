package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	pb "github.com/victormemborg/Consensus/grpc"
	"google.golang.org/grpc"
)

var (
	serviceRegistry = make(map[int32]string)
	registryMutex   = sync.Mutex{}
)

type Node struct {
	pb.NodeServer
	id      int32
	address string
	peers   map[int32]pb.NodeClient // id -> client
	role    string                  //leader, follower, candidate
	term    int32
}

func NewNode(id int32, address string) *Node {
	return &Node{
		id:      id,
		address: address,
		role:    "follower",
		term:    0,
		peers:   make(map[int32]pb.NodeClient),
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

	if grpcServer.Serve(listener) != nil {
		fmt.Printf("failed to serve: %v\n", err)
	}
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

func (n *Node) broadcast() {
	message := &pb.Request{Sender: n.id}

	for _, peer := range n.peers {
		fmt.Println()
		_, err := peer.ElectLeader(context.Background(), message)
		if err != nil {
			fmt.Printf("failed to elect %v\n", err)
		}
	}
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

	n1.broadcast()
	n2.broadcast()
	n3.broadcast()

	for k, v := range serviceRegistry {
		fmt.Printf("%d : %s\n", k, v)
	}

	wg.Wait()
}

func (n *Node) InformArrival(_ context.Context, in *pb.NodeInfo) (*pb.Empty, error) {
	n.addPeer(in.Id, in.Address)
	return &pb.Empty{}, nil
}

func (n *Node) ElectLeader(_ context.Context, in *pb.Request) (*pb.Reply, error) {
	fmt.Printf("%d modtog en besked fra %d", n.id, in.Sender)
	return &pb.Reply{Granted: true}, nil
}
