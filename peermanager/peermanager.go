package peermanager

import (
	"context"
	"sync"

	peer "github.com/libp2p/go-libp2p-peer"
)

// PeerProcess is any process that provides services for a peer
type PeerProcess interface {
	Startup()
	Shutdown()
}

// PeerProcessFactory provides a function that will create a PeerQueue.
type PeerProcessFactory func(ctx context.Context, p peer.ID) PeerProcess

type peerProcessInstance struct {
	refcnt  int
	process PeerProcess
}

// PeerManager manages a pool of peers and sends messages to peers in the pool.
type PeerManager struct {
	peerProcesses   map[peer.ID]*peerProcessInstance
	peerProcessesLk sync.RWMutex

	createPeerProcess PeerProcessFactory
	ctx               context.Context
}

// New creates a new PeerManager, given a context and a peerQueueFactory.
func New(ctx context.Context, createPeerQueue PeerProcessFactory) *PeerManager {
	return &PeerManager{
		peerProcesses:     make(map[peer.ID]*peerProcessInstance),
		createPeerProcess: createPeerQueue,
		ctx:               ctx,
	}
}

// ConnectedPeers returns a list of peers this PeerManager is managing.
func (pm *PeerManager) ConnectedPeers() []peer.ID {
	pm.peerProcessesLk.RLock()
	defer pm.peerProcessesLk.RUnlock()
	peers := make([]peer.ID, 0, len(pm.peerProcesses))
	for p := range pm.peerProcesses {
		peers = append(peers, p)
	}
	return peers
}

// Connected is called to add a new peer to the pool
func (pm *PeerManager) Connected(p peer.ID) {
	pm.peerProcessesLk.Lock()
	pq := pm.getOrCreate(p)
	pq.refcnt++
	pm.peerProcessesLk.Unlock()
}

// Disconnected is called to remove a peer from the pool.
func (pm *PeerManager) Disconnected(p peer.ID) {
	pm.peerProcessesLk.Lock()
	pq, ok := pm.peerProcesses[p]
	if !ok {
		pm.peerProcessesLk.Unlock()
		return
	}

	pq.refcnt--
	if pq.refcnt > 0 {
		pm.peerProcessesLk.Unlock()
		return
	}

	delete(pm.peerProcesses, p)
	pm.peerProcessesLk.Unlock()

	pq.process.Shutdown()

}

// GetProcess returns the process for the given peer
func (pm *PeerManager) GetProcess(
	p peer.ID) PeerProcess {
	pm.peerProcessesLk.Lock()
	pqi := pm.getOrCreate(p)
	pm.peerProcessesLk.Unlock()
	return pqi.process
}

func (pm *PeerManager) getOrCreate(p peer.ID) *peerProcessInstance {
	pqi, ok := pm.peerProcesses[p]
	if !ok {
		pq := pm.createPeerProcess(pm.ctx, p)
		pq.Startup()
		pqi = &peerProcessInstance{0, pq}
		pm.peerProcesses[p] = pqi
	}
	return pqi
}
