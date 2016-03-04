package state

import (
	"io/ioutil"
	"log"
	"net"
	"testing"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"

	"github.com/coreos/etcd/raft"
	"github.com/docker/swarm-v2/api"
	"github.com/stretchr/testify/assert"
)

var (
	raftLogger = &raft.DefaultLogger{Logger: log.New(ioutil.Discard, "", 0)}
)

func init() {
	grpclog.SetLogger(log.New(ioutil.Discard, "", log.LstdFlags))
}

func newInitNode(t *testing.T, id uint64) *Node {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoError(t, err, "can't bind to raft service port")
	s := grpc.NewServer()

	cfg := DefaultNodeConfig()
	cfg.Logger = raftLogger

	n, err := NewNode(context.Background(), id, l.Addr().String(), cfg, nil)
	assert.NoError(t, err, "can't create raft node")
	n.Listener = l
	n.Server = s

	err = n.Campaign(n.Ctx)
	assert.NoError(t, err, "can't campaign to be the leader")
	n.Start()

	Register(s, n)

	done := make(chan error)
	go func() {
		done <- s.Serve(l)
	}()
	go func() {
		// After stopping, we should receive an error from Serve
		assert.Error(t, <-done)
	}()

	time.Sleep(1 * time.Second)
	return n
}

func newJoinNode(t *testing.T, id uint64, join string) *Node {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoError(t, err, "can't bind to raft service port")
	s := grpc.NewServer()

	cfg := DefaultNodeConfig()
	cfg.Logger = raftLogger

	n, err := NewNode(context.Background(), id, l.Addr().String(), cfg, nil)
	assert.NoError(t, err, "can't create raft node")
	n.Listener = l
	n.Server = s

	n.Start()

	c, err := GetRaftClient(join, 100*time.Millisecond)
	assert.NoError(t, err, "can't initiate connection with existing raft")

	resp, err := c.Join(n.Ctx, &api.JoinRequest{
		&api.RaftNode{ID: id, Addr: l.Addr().String()},
	})
	assert.NoError(t, err, "can't join existing Raft")

	err = n.RegisterNodes(resp.Members)
	assert.NoError(t, err, "can't add nodes to the local cluster list")

	Register(s, n)

	done := make(chan error)
	go func() {
		done <- s.Serve(l)
	}()
	go func() {
		// After stopping, we should receive an error from Serve
		assert.Error(t, <-done)
	}()

	time.Sleep(1 * time.Second)
	return n
}

func newRaftCluster(t *testing.T) map[int]*Node {
	nodes := make(map[int]*Node, 0)

	nodes[1] = newInitNode(t, 1)
	nodes[2] = newJoinNode(t, 2, nodes[1].Listener.Addr().String())
	nodes[3] = newJoinNode(t, 3, nodes[1].Listener.Addr().String())

	return nodes
}

func addRaftNode(t *testing.T, nodes map[int]*Node) {
	n := len(nodes) + 1
	nodes[n] = newJoinNode(t, uint64(n), nodes[1].Listener.Addr().String())
}

func teardownCluster(t *testing.T, nodes map[int]*Node) {
	for _, node := range nodes {
		shutdownNode(node)
	}
	nodes = nil

	// FIXME We have to wait a little bit for the
	// connections to be cleaned up properly
	time.Sleep(2 * time.Second)
}

func removeNode(nodes map[string]*Node, node string) {
	shutdownNode(nodes[node])
	delete(nodes, node)
}

func shutdownNode(node *Node) {
	node.Server.Stop()
	node.Server.TestingCloseConns()
	_ = node.Listener.Close()
	node.Listener = nil
	node.Shutdown()
}

func TestRaftBootstrap(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	assert.Equal(t, len(nodes[1].Cluster.Peers()), 3)
	assert.Equal(t, len(nodes[2].Cluster.Peers()), 3)
	assert.Equal(t, len(nodes[3].Cluster.Peers()), 3)
}

func TestLeader(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	assert.True(t, nodes[1].IsLeader(), "error: node 1 is not the Leader")

	// nodes should all have the same leader
	assert.Equal(t, nodes[1].Leader(), nodes[1].ID)
	assert.Equal(t, nodes[2].Leader(), nodes[1].ID)
	assert.Equal(t, nodes[3].Leader(), nodes[1].ID)
}

func TestRaftLeaderDown(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	// Stop node 1
	nodes[1].Stop()

	// Wait for the re-election to occur
	time.Sleep(4 * time.Second)

	// Leader should not be 1
	assert.NotEqual(t, nodes[2].Leader(), nodes[1].ID)

	// Ensure that node 2 and node 3 have the same leader
	assert.Equal(t, nodes[3].Leader(), nodes[2].Leader())

	// Propose a value
	err = nodes[2].Propose(nodes[2].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// The value should be replicated on all remaining nodes
	assert.Equal(t, nodes[2].StoreLength(), 1)
	assert.Equal(t, nodes[2].Get(key), string(value))
	assert.Equal(t, len(nodes[2].Cluster.Peers()), 3)

	assert.Equal(t, nodes[3].StoreLength(), 1)
	assert.Equal(t, nodes[3].Get(key), string(value))
	assert.Equal(t, len(nodes[3].Cluster.Peers()), 3)
}

func TestRaftFollowerDown(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	// Stop node 3
	nodes[3].Stop()

	// Wait election tick
	time.Sleep(4 * time.Second)

	// Leader should still be 1
	assert.True(t, nodes[1].IsLeader(), "node 1 is not a leader anymore")
	assert.Equal(t, nodes[2].Leader(), nodes[1].ID)

	// Propose a value
	err = nodes[2].Propose(nodes[2].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// The value should be replicated on all remaining nodes
	assert.Equal(t, nodes[1].StoreLength(), 1)
	assert.Equal(t, nodes[1].Get(key), string(value))
	assert.Equal(t, len(nodes[1].Cluster.Peers()), 3)

	assert.Equal(t, nodes[2].StoreLength(), 1)
	assert.Equal(t, nodes[2].Get(key), string(value))
	assert.Equal(t, len(nodes[2].Cluster.Peers()), 3)
}

func TestRaftLogReplication(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	// Propose a value
	err = nodes[1].Propose(nodes[1].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// All nodes should have the value in the physical store
	assert.Equal(t, nodes[1].StoreLength(), 1)
	assert.Equal(t, nodes[1].Get(key), string(value))

	assert.Equal(t, nodes[2].StoreLength(), 1)
	assert.Equal(t, nodes[2].Get(key), string(value))

	assert.Equal(t, nodes[3].StoreLength(), 1)
	assert.Equal(t, nodes[3].Get(key), string(value))
}

func TestRaftLogReplicationWithoutLeader(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	// Stop the leader
	nodes[1].Stop()

	// Propose a value
	err = nodes[2].Propose(nodes[2].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// No value should be replicated in the store in the absence of the leader
	assert.Equal(t, nodes[2].StoreLength(), 0)
	assert.Equal(t, nodes[2].Get(key), "")

	assert.Equal(t, nodes[3].StoreLength(), 0)
	assert.Equal(t, nodes[3].Get(key), "")
}

func TestRaftQuorumFailure(t *testing.T) {
	// Bring up a 5 nodes cluster
	nodes := newRaftCluster(t)
	addRaftNode(t, nodes)
	addRaftNode(t, nodes)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	// Lose a majority
	nodes[3].Stop()
	nodes[4].Stop()
	nodes[5].Stop()

	// Propose a value
	err = nodes[1].Propose(nodes[1].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// The value should not be replicated, we have no majority
	assert.Equal(t, nodes[1].StoreLength(), 0)
	assert.Equal(t, nodes[1].Get(key), "")

	assert.Equal(t, nodes[2].StoreLength(), 0)
	assert.Equal(t, nodes[2].Get(key), "")
}

func TestRaftFollowerLeave(t *testing.T) {
	// Bring up a 5 nodes cluster
	nodes := newRaftCluster(t)
	addRaftNode(t, nodes)
	addRaftNode(t, nodes)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	resp, err := nodes[5].Leave(nodes[5].Ctx, &api.LeaveRequest{&api.RaftNode{ID: nodes[5].ID}})
	assert.NoError(t, err, "error sending message to leave the raft")
	assert.NotNil(t, resp, "leave response message is nil")

	// Propose a value
	err = nodes[1].Propose(nodes[1].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// Value should be replicated on every node
	assert.Equal(t, nodes[1].StoreLength(), 1)
	assert.Equal(t, nodes[1].Get(key), string(value))
	assert.Equal(t, len(nodes[1].Cluster.Peers()), 4)

	assert.Equal(t, nodes[2].StoreLength(), 1)
	assert.Equal(t, nodes[2].Get(key), string(value))
	assert.Equal(t, len(nodes[2].Cluster.Peers()), 4)

	assert.Equal(t, nodes[3].StoreLength(), 1)
	assert.Equal(t, nodes[3].Get(key), string(value))
	assert.Equal(t, len(nodes[3].Cluster.Peers()), 4)

	assert.Equal(t, nodes[4].StoreLength(), 1)
	assert.Equal(t, nodes[4].Get(key), string(value))
	assert.Equal(t, len(nodes[4].Cluster.Peers()), 4)
}

func TestRaftLeaderLeave(t *testing.T) {
	nodes := newRaftCluster(t)
	defer teardownCluster(t, nodes)

	key := "foo"
	value := []byte("bar")

	pair, err := EncodePair(key, value)
	assert.NoError(t, err, "can't encode key/value pair")

	// node 1 is the leader
	assert.Equal(t, nodes[1].Leader(), nodes[1].ID)

	// Try to leave the raft
	resp, err := nodes[1].Leave(nodes[1].Ctx, &api.LeaveRequest{&api.RaftNode{ID: nodes[1].ID}})
	assert.NoError(t, err, "error sending message to leave the raft")
	assert.NotNil(t, resp, "leave response message is nil")

	// Wait for election tick
	time.Sleep(4 * time.Second)

	// Leader should not be 1
	assert.NotEqual(t, nodes[2].Leader(), nodes[1].ID)
	assert.Equal(t, nodes[2].Leader(), nodes[3].Leader())

	// Propose a value
	err = nodes[2].Propose(nodes[2].Ctx, pair)
	assert.NoError(t, err, "can't propose value to cluster")

	// Wait heartbeat tick
	time.Sleep(1 * time.Second)

	// The value should be replicated on all remaining nodes
	assert.Equal(t, nodes[2].StoreLength(), 1)
	assert.Equal(t, nodes[2].Get(key), string(value))
	assert.Equal(t, len(nodes[2].Cluster.Peers()), 2)

	assert.Equal(t, nodes[3].StoreLength(), 1)
	assert.Equal(t, nodes[3].Get(key), string(value))
	assert.Equal(t, len(nodes[3].Cluster.Peers()), 2)
}

func TestRaftSnapshot(t *testing.T) {
	t.Skip()
}

func TestRaftRecoverSnapshot(t *testing.T) {
	t.Skip()
}