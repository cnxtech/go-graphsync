package responsemanager

import (
	"context"
	"math"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"

	cid "github.com/ipfs/go-cid"
	gsmsg "github.com/ipfs/go-graphsync/message"
	"github.com/ipfs/go-graphsync/responsemanager/peerresponsemanager"
	"github.com/ipfs/go-peertaskqueue/peertask"
	"github.com/ipfs/go-graphsync/testbridge"
	"github.com/ipfs/go-graphsync/testutil"
	ipld "github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	peer "github.com/libp2p/go-libp2p-peer"
)

type fakeQueryQueue struct {
	popWait   sync.WaitGroup
	queriesLk sync.RWMutex
	queries   []*peertask.TaskBlock
}

func (fqq *fakeQueryQueue) PushBlock(to peer.ID, tasks ...peertask.Task) {
	fqq.queriesLk.Lock()
	fqq.queries = append(fqq.queries, &peertask.TaskBlock{
		Tasks:    tasks,
		Priority: tasks[0].Priority,
		Target:   to,
		Done:     func([]peertask.Task) {},
	})
	fqq.queriesLk.Unlock()
}

func (fqq *fakeQueryQueue) PopBlock() *peertask.TaskBlock {
	fqq.popWait.Wait()
	fqq.queriesLk.Lock()
	defer fqq.queriesLk.Unlock()
	if len(fqq.queries) == 0 {
		return nil
	}
	block := fqq.queries[0]
	fqq.queries = fqq.queries[1:]
	return block
}

func (fqq *fakeQueryQueue) Remove(identifier peertask.Identifier, p peer.ID) {
	fqq.queriesLk.Lock()
	defer fqq.queriesLk.Unlock()
	for i, query := range fqq.queries {
		if query.Target == p {
			for j, task := range query.Tasks {
				if task.Identifier == identifier {
					query.Tasks = append(query.Tasks[:j], query.Tasks[j+1:]...)
				}
			}
			if len(query.Tasks) == 0 {
				fqq.queries = append(fqq.queries[:i], fqq.queries[i+1:]...)
			}
		}
	}
}

func (fqq *fakeQueryQueue) ThawRound() {

}

type fakePeerManager struct {
	lastPeer           peer.ID
	peerResponseSender peerresponsemanager.PeerResponseSender
}

func (fpm *fakePeerManager) SenderForPeer(p peer.ID) peerresponsemanager.PeerResponseSender {
	fpm.lastPeer = p
	return fpm.peerResponseSender
}

type sentResponse struct {
	requestID gsmsg.GraphSyncRequestID
	link      ipld.Link
	data      []byte
}

type fakePeerResponseSender struct {
	sentResponses        chan sentResponse
	lastCompletedRequest chan gsmsg.GraphSyncRequestID
}

func (fprs *fakePeerResponseSender) Startup()  {}
func (fprs *fakePeerResponseSender) Shutdown() {}

func (fprs *fakePeerResponseSender) SendResponse(
	requestID gsmsg.GraphSyncRequestID,
	link ipld.Link,
	data []byte,
) {
	fprs.sentResponses <- sentResponse{requestID, link, data}
}

func (fprs *fakePeerResponseSender) FinishRequest(requestID gsmsg.GraphSyncRequestID) {
	fprs.lastCompletedRequest <- requestID
}

func (fprs *fakePeerResponseSender) FinishWithError(requestID gsmsg.GraphSyncRequestID, status gsmsg.GraphSyncResponseStatusCode) {
	fprs.lastCompletedRequest <- requestID
}

func TestIncomingQuery(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	blks := testutil.GenerateBlocksOfSize(5, 20)
	loader := testbridge.NewMockLoader(blks)
	ipldBridge := testbridge.NewMockIPLDBridge()
	requestIDChan := make(chan gsmsg.GraphSyncRequestID, 1)
	sentResponses := make(chan sentResponse, len(blks))
	fprs := &fakePeerResponseSender{lastCompletedRequest: requestIDChan, sentResponses: sentResponses}
	peerManager := &fakePeerManager{peerResponseSender: fprs}
	queryQueue := &fakeQueryQueue{}
	responseManager := New(ctx, loader, ipldBridge, peerManager, queryQueue)
	responseManager.Startup()

	cids := make([]cid.Cid, 0, 5)
	for _, block := range blks {
		cids = append(cids, block.Cid())
	}
	selectorSpec := testbridge.NewMockSelectorSpec(cids)
	selector, err := ipldBridge.EncodeNode(selectorSpec)
	if err != nil {
		t.Fatal("error encoding selector")
	}
	requestID := gsmsg.GraphSyncRequestID(rand.Int31())
	requests := []gsmsg.GraphSyncRequest{
		gsmsg.NewRequest(requestID, selector, gsmsg.GraphSyncPriority(math.MaxInt32)),
	}
	p := testutil.GeneratePeers(1)[0]
	responseManager.ProcessRequests(ctx, p, requests)
	select {
	case <-ctx.Done():
		t.Fatal("Should have completed request but didn't")
	case <-requestIDChan:
	}
	for i := 0; i < len(blks); i++ {
		select {
		case sentResponse := <-sentResponses:
			k := sentResponse.link.(cidlink.Link)
			blockIndex := testutil.IndexOf(blks, k.Cid)
			if blockIndex == -1 {
				t.Fatal("sent incorrect link")
			}
			if !reflect.DeepEqual(sentResponse.data, blks[blockIndex].RawData()) {
				t.Fatal("sent incorrect data")
			}
			if sentResponse.requestID != requestID {
				t.Fatal("incorrect response id")
			}
		case <-ctx.Done():
			t.Fatal("did not send enough responses")
		}
	}
}

func TestCancellationQueryInProgress(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	blks := testutil.GenerateBlocksOfSize(5, 20)
	loader := testbridge.NewMockLoader(blks)
	ipldBridge := testbridge.NewMockIPLDBridge()
	requestIDChan := make(chan gsmsg.GraphSyncRequestID)
	sentResponses := make(chan sentResponse)
	fprs := &fakePeerResponseSender{lastCompletedRequest: requestIDChan, sentResponses: sentResponses}
	peerManager := &fakePeerManager{peerResponseSender: fprs}
	queryQueue := &fakeQueryQueue{}
	responseManager := New(ctx, loader, ipldBridge, peerManager, queryQueue)
	responseManager.Startup()

	cids := make([]cid.Cid, 0, 5)
	for _, block := range blks {
		cids = append(cids, block.Cid())
	}
	selectorSpec := testbridge.NewMockSelectorSpec(cids)
	selector, err := ipldBridge.EncodeNode(selectorSpec)
	if err != nil {
		t.Fatal("error encoding selector")
	}
	requestID := gsmsg.GraphSyncRequestID(rand.Int31())
	requests := []gsmsg.GraphSyncRequest{
		gsmsg.NewRequest(requestID, selector, gsmsg.GraphSyncPriority(math.MaxInt32)),
	}
	p := testutil.GeneratePeers(1)[0]
	responseManager.ProcessRequests(ctx, p, requests)

	// read one block
	select {
	case sentResponse := <-sentResponses:
		k := sentResponse.link.(cidlink.Link)
		blockIndex := testutil.IndexOf(blks, k.Cid)
		if blockIndex == -1 {
			t.Fatal("sent incorrect link")
		}
		if !reflect.DeepEqual(sentResponse.data, blks[blockIndex].RawData()) {
			t.Fatal("sent incorrect data")
		}
		if sentResponse.requestID != requestID {
			t.Fatal("incorrect response id")
		}
	case <-ctx.Done():
		t.Fatal("did not send responses")
	}

	// send a cancellation
	requests = []gsmsg.GraphSyncRequest{
		gsmsg.CancelRequest(requestID),
	}
	responseManager.ProcessRequests(ctx, p, requests)

	responseManager.synchronize()

	// read one block -- to unblock processing
	select {
	case sentResponse := <-sentResponses:
		k := sentResponse.link.(cidlink.Link)
		blockIndex := testutil.IndexOf(blks, k.Cid)
		if blockIndex == -1 {
			t.Fatal("sent incorrect link")
		}
		if !reflect.DeepEqual(sentResponse.data, blks[blockIndex].RawData()) {
			t.Fatal("sent incorrect data")
		}
		if sentResponse.requestID != requestID {
			t.Fatal("incorrect response id")
		}
	case <-ctx.Done():
		t.Fatal("did not send responses")
	}

	// at this point traversal should abort and we should receive a completion
	select {
	case <-ctx.Done():
		t.Fatal("Should have completed request but didn't")
	case <-sentResponses:
		t.Fatal("should not send any more responses")
	case <-requestIDChan:
	}
}

func TestEarlyCancellation(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	blks := testutil.GenerateBlocksOfSize(5, 20)
	loader := testbridge.NewMockLoader(blks)
	ipldBridge := testbridge.NewMockIPLDBridge()
	requestIDChan := make(chan gsmsg.GraphSyncRequestID)
	sentResponses := make(chan sentResponse)
	fprs := &fakePeerResponseSender{lastCompletedRequest: requestIDChan, sentResponses: sentResponses}
	peerManager := &fakePeerManager{peerResponseSender: fprs}
	queryQueue := &fakeQueryQueue{}
	queryQueue.popWait.Add(1)
	responseManager := New(ctx, loader, ipldBridge, peerManager, queryQueue)
	responseManager.Startup()

	cids := make([]cid.Cid, 0, 5)
	for _, block := range blks {
		cids = append(cids, block.Cid())
	}
	selectorSpec := testbridge.NewMockSelectorSpec(cids)
	selector, err := ipldBridge.EncodeNode(selectorSpec)
	if err != nil {
		t.Fatal("error encoding selector")
	}
	requestID := gsmsg.GraphSyncRequestID(rand.Int31())
	requests := []gsmsg.GraphSyncRequest{
		gsmsg.NewRequest(requestID, selector, gsmsg.GraphSyncPriority(math.MaxInt32)),
	}
	p := testutil.GeneratePeers(1)[0]
	responseManager.ProcessRequests(ctx, p, requests)

	// send a cancellation
	requests = []gsmsg.GraphSyncRequest{
		gsmsg.CancelRequest(requestID),
	}
	responseManager.ProcessRequests(ctx, p, requests)

	responseManager.synchronize()

	// unblock popping from queue
	queryQueue.popWait.Done()

	// verify no responses processed
	select {
	case <-ctx.Done():
	case <-sentResponses:
		t.Fatal("should not send any more responses")
	case <-requestIDChan:
		t.Fatal("should not send have completed response")
	}
}
