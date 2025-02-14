package requestmanager

import (
	"context"
	"fmt"
	"math"

	"github.com/ipfs/go-block-format"
	ipldbridge "github.com/ipfs/go-graphsync/ipldbridge"
	gsmsg "github.com/ipfs/go-graphsync/message"
	"github.com/ipfs/go-graphsync/metadata"
	"github.com/ipfs/go-graphsync/requestmanager/loader"
	"github.com/ipfs/go-graphsync/requestmanager/types"
	logging "github.com/ipfs/go-log"
	"github.com/ipld/go-ipld-prime"
	peer "github.com/libp2p/go-libp2p-peer"
)

var log = logging.Logger("graphsync")

const (
	// maxPriority is the max priority as defined by the bitswap protocol
	maxPriority = gsmsg.GraphSyncPriority(math.MaxInt32)
)

type inProgressRequestStatus struct {
	ctx          context.Context
	cancelFn     func()
	p            peer.ID
	networkError chan error
}

// PeerHandler is an interface that can send requests to peers
type PeerHandler interface {
	SendRequest(p peer.ID, graphSyncRequest gsmsg.GraphSyncRequest)
}

// AsyncLoader is an interface for loading links asynchronously, returning
// results as new responses are processed
type AsyncLoader interface {
	StartRequest(requestID gsmsg.GraphSyncRequestID)
	ProcessResponse(responses map[gsmsg.GraphSyncRequestID]metadata.Metadata,
		blks []blocks.Block)
	AsyncLoad(requestID gsmsg.GraphSyncRequestID, link ipld.Link) <-chan types.AsyncLoadResult
	CompleteResponsesFor(requestID gsmsg.GraphSyncRequestID)
	CleanupRequest(requestID gsmsg.GraphSyncRequestID)
}

// RequestManager tracks outgoing requests and processes incoming reponses
// to them.
type RequestManager struct {
	ctx         context.Context
	cancel      func()
	messages    chan requestManagerMessage
	ipldBridge  ipldbridge.IPLDBridge
	peerHandler PeerHandler
	rc          *responseCollector
	asyncLoader AsyncLoader
	// dont touch out side of run loop
	nextRequestID             gsmsg.GraphSyncRequestID
	inProgressRequestStatuses map[gsmsg.GraphSyncRequestID]*inProgressRequestStatus
}

type requestManagerMessage interface {
	handle(rm *RequestManager)
}

// New generates a new request manager from a context, network, and selectorQuerier
func New(ctx context.Context, asyncLoader AsyncLoader, ipldBridge ipldbridge.IPLDBridge) *RequestManager {
	ctx, cancel := context.WithCancel(ctx)
	return &RequestManager{
		ctx:                       ctx,
		cancel:                    cancel,
		ipldBridge:                ipldBridge,
		asyncLoader:               asyncLoader,
		rc:                        newResponseCollector(ctx),
		messages:                  make(chan requestManagerMessage, 16),
		inProgressRequestStatuses: make(map[gsmsg.GraphSyncRequestID]*inProgressRequestStatus),
	}
}

// SetDelegate specifies who will send messages out to the internet.
func (rm *RequestManager) SetDelegate(peerHandler PeerHandler) {
	rm.peerHandler = peerHandler
}

type inProgressRequest struct {
	requestID     gsmsg.GraphSyncRequestID
	incoming      chan types.ResponseProgress
	incomingError chan error
}

type newRequestMessage struct {
	p                     peer.ID
	selector              ipld.Node
	inProgressRequestChan chan<- inProgressRequest
}

// SendRequest initiates a new GraphSync request to the given peer.
func (rm *RequestManager) SendRequest(ctx context.Context,
	p peer.ID,
	cidRootedSelector ipld.Node) (<-chan types.ResponseProgress, <-chan error) {
	if len(rm.ipldBridge.ValidateSelectorSpec(cidRootedSelector)) != 0 {
		return rm.singleErrorResponse(fmt.Errorf("Invalid Selector Spec"))
	}

	inProgressRequestChan := make(chan inProgressRequest)

	select {
	case rm.messages <- &newRequestMessage{p, cidRootedSelector, inProgressRequestChan}:
	case <-rm.ctx.Done():
		return rm.emptyResponse()
	case <-ctx.Done():
		return rm.emptyResponse()
	}
	var receivedInProgressRequest inProgressRequest
	select {
	case <-rm.ctx.Done():
		return rm.emptyResponse()
	case receivedInProgressRequest = <-inProgressRequestChan:
	}

	return rm.rc.collectResponses(ctx,
		receivedInProgressRequest.incoming,
		receivedInProgressRequest.incomingError,
		func() {
			rm.cancelRequest(receivedInProgressRequest.requestID,
				receivedInProgressRequest.incoming,
				receivedInProgressRequest.incomingError)
		})
}

func (rm *RequestManager) emptyResponse() (chan types.ResponseProgress, chan error) {
	ch := make(chan types.ResponseProgress)
	close(ch)
	errCh := make(chan error)
	close(errCh)
	return ch, errCh
}

func (rm *RequestManager) singleErrorResponse(err error) (chan types.ResponseProgress, chan error) {
	ch := make(chan types.ResponseProgress)
	close(ch)
	errCh := make(chan error, 1)
	errCh <- err
	close(errCh)
	return ch, errCh
}

type cancelRequestMessage struct {
	requestID gsmsg.GraphSyncRequestID
}

func (rm *RequestManager) cancelRequest(requestID gsmsg.GraphSyncRequestID,
	incomingResponses chan types.ResponseProgress,
	incomingErrors chan error) {
	cancelMessageChannel := rm.messages
	for cancelMessageChannel != nil || incomingResponses != nil || incomingErrors != nil {
		select {
		case cancelMessageChannel <- &cancelRequestMessage{requestID}:
			cancelMessageChannel = nil
		// clear out any remaining responses, in case and "incoming reponse"
		// messages get processed before our cancel message
		case _, ok := <-incomingResponses:
			if !ok {
				incomingResponses = nil
			}
		case _, ok := <-incomingErrors:
			if !ok {
				incomingErrors = nil
			}
		case <-rm.ctx.Done():
			return
		}
	}
}

type processResponseMessage struct {
	p         peer.ID
	responses []gsmsg.GraphSyncResponse
	blks      []blocks.Block
}

// ProcessResponses ingests the given responses from the network and
// and updates the in progress requests based on those responses.
func (rm *RequestManager) ProcessResponses(p peer.ID, responses []gsmsg.GraphSyncResponse,
	blks []blocks.Block) {
	select {
	case rm.messages <- &processResponseMessage{p, responses, blks}:
	case <-rm.ctx.Done():
	}
}

// Startup starts processing for the WantManager.
func (rm *RequestManager) Startup() {
	go rm.run()
}

// Shutdown ends processing for the want manager.
func (rm *RequestManager) Shutdown() {
	rm.cancel()
}

func (rm *RequestManager) run() {
	// NOTE: Do not open any streams or connections from anywhere in this
	// event loop. Really, just don't do anything likely to block.
	defer rm.cleanupInProcessRequests()

	for {
		select {
		case message := <-rm.messages:
			message.handle(rm)
		case <-rm.ctx.Done():
			return
		}
	}
}

func (rm *RequestManager) cleanupInProcessRequests() {
	for _, requestStatus := range rm.inProgressRequestStatuses {
		requestStatus.cancelFn()
	}
}

type terminateRequestMessage struct {
	requestID gsmsg.GraphSyncRequestID
}

func (nrm *newRequestMessage) handle(rm *RequestManager) {
	requestID := rm.nextRequestID
	rm.nextRequestID++

	inProgressChan, inProgressErr := rm.setupRequest(requestID, nrm.p, nrm.selector)

	select {
	case nrm.inProgressRequestChan <- inProgressRequest{
		requestID:     requestID,
		incoming:      inProgressChan,
		incomingError: inProgressErr,
	}:
	case <-rm.ctx.Done():
	}
}

func (trm *terminateRequestMessage) handle(rm *RequestManager) {
	delete(rm.inProgressRequestStatuses, trm.requestID)
	rm.asyncLoader.CleanupRequest(trm.requestID)
}

func (crm *cancelRequestMessage) handle(rm *RequestManager) {
	inProgressRequestStatus, ok := rm.inProgressRequestStatuses[crm.requestID]
	if !ok {
		return
	}

	rm.peerHandler.SendRequest(inProgressRequestStatus.p, gsmsg.CancelRequest(crm.requestID))
	delete(rm.inProgressRequestStatuses, crm.requestID)
	inProgressRequestStatus.cancelFn()
}

func (prm *processResponseMessage) handle(rm *RequestManager) {
	filteredResponses := rm.filterResponsesForPeer(prm.responses, prm.p)
	responseMetadata := metadataForResponses(filteredResponses, rm.ipldBridge)
	rm.asyncLoader.ProcessResponse(responseMetadata, prm.blks)
	rm.processTerminations(filteredResponses)
}

func (rm *RequestManager) filterResponsesForPeer(responses []gsmsg.GraphSyncResponse, p peer.ID) []gsmsg.GraphSyncResponse {
	responsesForPeer := make([]gsmsg.GraphSyncResponse, 0, len(responses))
	for _, response := range responses {
		requestStatus, ok := rm.inProgressRequestStatuses[response.RequestID()]
		if !ok || requestStatus.p != p {
			continue
		}
		responsesForPeer = append(responsesForPeer, response)
	}
	return responsesForPeer
}

func (rm *RequestManager) processTerminations(responses []gsmsg.GraphSyncResponse) {
	for _, response := range responses {
		if gsmsg.IsTerminalResponseCode(response.Status()) {
			if gsmsg.IsTerminalFailureCode(response.Status()) {
				requestStatus := rm.inProgressRequestStatuses[response.RequestID()]
				responseError := rm.generateResponseErrorFromStatus(response.Status())
				select {
				case requestStatus.networkError <- responseError:
				case <-requestStatus.ctx.Done():
				}
				requestStatus.cancelFn()
			}
			rm.asyncLoader.CompleteResponsesFor(response.RequestID())
			delete(rm.inProgressRequestStatuses, response.RequestID())
		}
	}
}

func (rm *RequestManager) generateResponseErrorFromStatus(status gsmsg.GraphSyncResponseStatusCode) error {
	switch status {
	case gsmsg.RequestFailedBusy:
		return fmt.Errorf("Request Failed - Peer Is Busy")
	case gsmsg.RequestFailedContentNotFound:
		return fmt.Errorf("Request Failed - Content Not Found")
	case gsmsg.RequestFailedLegal:
		return fmt.Errorf("Request Failed - For Legal Reasons")
	case gsmsg.RequestFailedUnknown:
		return fmt.Errorf("Request Failed - Unknown Reason")
	default:
		return fmt.Errorf("Unknown")
	}
}

func (rm *RequestManager) setupRequest(requestID gsmsg.GraphSyncRequestID, p peer.ID, selectorSpec ipld.Node) (chan types.ResponseProgress, chan error) {
	selectorBytes, err := rm.ipldBridge.EncodeNode(selectorSpec)
	if err != nil {
		return rm.singleErrorResponse(err)
	}
	root, selector, err := rm.ipldBridge.DecodeSelectorSpec(selectorSpec)
	if err != nil {
		return rm.singleErrorResponse(err)
	}
	networkErrorChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(rm.ctx)
	rm.inProgressRequestStatuses[requestID] = &inProgressRequestStatus{
		ctx, cancel, p, networkErrorChan,
	}
	rm.asyncLoader.StartRequest(requestID)
	rm.peerHandler.SendRequest(p, gsmsg.NewRequest(requestID, selectorBytes, maxPriority))
	return rm.executeTraversal(ctx, requestID, root, selector, networkErrorChan)
}

func (rm *RequestManager) executeTraversal(
	ctx context.Context,
	requestID gsmsg.GraphSyncRequestID,
	root ipld.Node,
	selector ipldbridge.Selector,
	networkErrorChan chan error,
) (chan types.ResponseProgress, chan error) {
	inProgressChan := make(chan types.ResponseProgress)
	inProgressErr := make(chan error)
	loaderFn := loader.WrapAsyncLoader(ctx, rm.asyncLoader.AsyncLoad, requestID, inProgressErr)
	visitor := visitToChannel(ctx, inProgressChan)
	go func() {
		rm.ipldBridge.Traverse(ctx, loaderFn, root, selector, visitor)
		select {
		case networkError := <-networkErrorChan:
			select {
			case <-rm.ctx.Done():
			case inProgressErr <- networkError:
			}
		default:
		}
		select {
		case <-ctx.Done():
		case rm.messages <- &terminateRequestMessage{requestID}:
		}
		close(inProgressChan)
		close(inProgressErr)
	}()
	return inProgressChan, inProgressErr
}
