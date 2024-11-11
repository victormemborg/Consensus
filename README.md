# Consensus

## Description
Consensus is an application that allows multiple nodes to agree on the state of our critical resource. It uses the Raft consensus algorithm to achieve this. Key features of the application include:
- Peer to peer Communication: The nodes communicate with each other to agree on a leader and the state of the critical resource.
- Leader election: The nodes elect a leader to manage the critical resource.
- Fault tolerance: The application can handle node failures and still maintain the state of the critical resource.


## To get started
1. Clone the repository
2. Configure amount of nodes in Consensus/node/main.go
    ```go
    nNodes := 5
    ```
    The default value is 5. You can change this value to simulate more or less nodes.
3. Run the application
    ```bash
    cd Consensus/node
    go run .
    ```
4. The application will start and output the results in the logs.log file under the node directory.

## Requirements
- The application is built using go version 1.23.1