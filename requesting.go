package torrent

import (
	"context"
	"encoding/gob"
	"fmt"
	"math/rand"
	"reflect"
	"runtime/pprof"
	"time"
	"unsafe"

	"github.com/anacrolix/log"
	"github.com/anacrolix/multiless"
	"github.com/lispad/go-generics-tools/binheap"

	"github.com/anacrolix/torrent/request-strategy"
	"github.com/anacrolix/torrent/typed-roaring"
)

type (
	// Since we have to store all the requests in memory, we can't reasonably exceed what could be
	// indexed with the memory space available.
	maxRequests = int
)

func (t *Torrent) requestStrategyPieceOrderState(i int) request_strategy.PieceRequestOrderState {
	return request_strategy.PieceRequestOrderState{
		Priority:     t.piece(i).purePriority(),
		Partial:      t.piecePartiallyDownloaded(i),
		Availability: t.piece(i).availability(),
	}
}

func init() {
	gob.Register(peerId{})
}

type peerId struct {
	*Peer
	ptr uintptr
}

func (p peerId) Uintptr() uintptr {
	return p.ptr
}

func (p peerId) GobEncode() (b []byte, _ error) {
	*(*reflect.SliceHeader)(unsafe.Pointer(&b)) = reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&p.ptr)),
		Len:  int(unsafe.Sizeof(p.ptr)),
		Cap:  int(unsafe.Sizeof(p.ptr)),
	}
	return
}

func (p *peerId) GobDecode(b []byte) error {
	if uintptr(len(b)) != unsafe.Sizeof(p.ptr) {
		panic(len(b))
	}
	ptr := unsafe.Pointer(&b[0])
	p.ptr = *(*uintptr)(ptr)
	log.Printf("%p", ptr)
	dst := reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&p.Peer)),
		Len:  int(unsafe.Sizeof(p.Peer)),
		Cap:  int(unsafe.Sizeof(p.Peer)),
	}
	copy(*(*[]byte)(unsafe.Pointer(&dst)), b)
	return nil
}

type (
	RequestIndex   = request_strategy.RequestIndex
	chunkIndexType = request_strategy.ChunkIndex
)

type desiredPeerRequests struct {
	requestIndexes []RequestIndex
	peer           *Peer
	pieceStates    []request_strategy.PieceRequestOrderState
}

func (p *desiredPeerRequests) Len() int {
	return len(p.requestIndexes)
}

func (p *desiredPeerRequests) Less(i, j int) bool {
	return p.lessByValue(p.requestIndexes[i], p.requestIndexes[j])
}

func (p *desiredPeerRequests) lessByValue(leftRequest, rightRequest RequestIndex) bool {
	t := p.peer.t
	leftPieceIndex := t.pieceIndexOfRequestIndex(leftRequest)
	rightPieceIndex := t.pieceIndexOfRequestIndex(rightRequest)
	ml := multiless.New()
	// Push requests that can't be served right now to the end. But we don't throw them away unless
	// there's a better alternative. This is for when we're using the fast extension and get choked
	// but our requests could still be good when we get unchoked.
	if p.peer.peerChoking {
		ml = ml.Bool(
			!p.peer.peerAllowedFast.Contains(leftPieceIndex),
			!p.peer.peerAllowedFast.Contains(rightPieceIndex),
		)
	}
	leftPiece := &p.pieceStates[leftPieceIndex]
	rightPiece := &p.pieceStates[rightPieceIndex]
	// Putting this first means we can steal requests from lesser-performing peers for our first few
	// new requests.
	priority := func() piecePriority {
		// Technically we would be happy with the cached priority here, except we don't actually
		// cache it anymore, and Torrent.piecePriority just does another lookup of *Piece to resolve
		// the priority through Piece.purePriority, which is probably slower.
		leftPriority := leftPiece.Priority
		rightPriority := rightPiece.Priority
		ml = ml.Int(
			-int(leftPriority),
			-int(rightPriority),
		)
		if !ml.Ok() {
			if leftPriority != rightPriority {
				panic("expected equal")
			}
		}
		return leftPriority
	}()
	if ml.Ok() {
		return ml.MustLess()
	}
	leftRequestState := t.requestState[leftRequest]
	rightRequestState := t.requestState[rightRequest]
	leftPeer := leftRequestState.peer
	rightPeer := rightRequestState.peer
	// Prefer chunks already requested from this peer.
	ml = ml.Bool(rightPeer == p.peer, leftPeer == p.peer)
	// Prefer unrequested chunks.
	ml = ml.Bool(rightPeer == nil, leftPeer == nil)
	if ml.Ok() {
		return ml.MustLess()
	}
	if leftPeer != nil {
		// The right peer should also be set, or we'd have resolved the computation by now.
		ml = ml.Uint64(
			rightPeer.requestState.Requests.GetCardinality(),
			leftPeer.requestState.Requests.GetCardinality(),
		)
		// Could either of the lastRequested be Zero? That's what checking an existing peer is for.
		leftLast := leftRequestState.when
		rightLast := rightRequestState.when
		if leftLast.IsZero() || rightLast.IsZero() {
			panic("expected non-zero last requested times")
		}
		// We want the most-recently requested on the left. Clients like Transmission serve requests
		// in received order, so the most recently-requested is the one that has the longest until
		// it will be served and therefore is the best candidate to cancel.
		ml = ml.CmpInt64(rightLast.Sub(leftLast).Nanoseconds())
	}
	ml = ml.Int(
		leftPiece.Availability,
		rightPiece.Availability)
	if priority == PiecePriorityReadahead {
		// TODO: For readahead in particular, it would be even better to consider distance from the
		// reader position so that reads earlier in a torrent don't starve reads later in the
		// torrent. This would probably require reconsideration of how readahead priority works.
		ml = ml.Int(leftPieceIndex, rightPieceIndex)
	} else {
		ml = ml.Int(t.pieceRequestOrder[leftPieceIndex], t.pieceRequestOrder[rightPieceIndex])
	}
	return ml.Less()
}

func (p *desiredPeerRequests) Swap(i, j int) {
	p.requestIndexes[i], p.requestIndexes[j] = p.requestIndexes[j], p.requestIndexes[i]
}

func (p *desiredPeerRequests) Push(x interface{}) {
	p.requestIndexes = append(p.requestIndexes, x.(RequestIndex))
}

func (p *desiredPeerRequests) Pop() interface{} {
	last := len(p.requestIndexes) - 1
	x := p.requestIndexes[last]
	p.requestIndexes = p.requestIndexes[:last]
	return x
}

type desiredRequestState struct {
	Requests   desiredPeerRequests
	Interested bool
}

func (p *Peer) getDesiredRequestState() (desired desiredRequestState) {
	t := p.t
	if !t.haveInfo() {
		return
	}
	if t.closed.IsSet() {
		return
	}
	input := t.getRequestStrategyInput()
	requestHeap := desiredPeerRequests{
		peer:           p,
		pieceStates:    t.requestPieceStates,
		requestIndexes: t.requestIndexes,
	}
	// Caller-provided allocation for roaring bitmap iteration.
	var it typedRoaring.Iterator[RequestIndex]
	request_strategy.GetRequestablePieces(
		input,
		t.getPieceRequestOrder(), // decide the order of pieces to request
		func(ih InfoHash, pieceIndex int, pieceExtra request_strategy.PieceRequestOrderState) {
			if ih != t.infoHash {
				return
			}
			if !p.peerHasPiece(pieceIndex) {
				return
			}
			requestHeap.pieceStates[pieceIndex] = pieceExtra
			allowedFast := p.peerAllowedFast.Contains(pieceIndex)
			t.iterUndirtiedRequestIndexesInPiece(&it, pieceIndex, func(r request_strategy.RequestIndex) {
				if !allowedFast {
					// We must signal interest to request this. TODO: We could set interested if the
					// peers pieces (minus the allowed fast set) overlap with our missing pieces if
					// there are any readers, or any pending pieces.
					desired.Interested = true
					// We can make or will allow sustaining a request here if we're not choked, or
					// have made the request previously (presumably while unchoked), and haven't had
					// the peer respond yet (and the request was retained because we are using the
					// fast extension).
					if p.peerChoking && !p.requestState.Requests.Contains(r) {
						// We can't request this right now.
						return
					}
				}
				if p.requestState.Cancelled.Contains(r) {
					// Can't re-request while awaiting acknowledgement.
					return
				}
				// add the chunks of the piece
				requestHeap.requestIndexes = append(requestHeap.requestIndexes, r)
			})
		},
	)
	t.assertPendingRequests()
	desired.Requests = requestHeap
	return
}

func getMaxRarity(arr []RarityContentType) int {
	// 获取最大的稀缺值
	// 最大稀缺值+1即为分类数
	// 如果数组为空, 返回-1, 后续不需要判断数组是否为空
	maxRarity := -1
	for _, element := range arr {
		maxRarity = int(max(int64(maxRarity), int64(element)))
	}
	return maxRarity
}

func classifyByValueWithIndexRandomized(arr []RarityContentType, seed int64) []int {
	// 1) classify by rarity and record the arr index as value in inverted arrays which represent different rarity values
	maxRarity := getMaxRarity(arr)
	bucktes := make([][]int, maxRarity+1)
	for k, v := range arr {
		bucktes[v] = append(bucktes[v], k)
	}

	res := make([]int, 0, len(arr))
	rand.Seed(seed)
	// traverse in descending order, as the indexes of buckets represent rarity
	// bigger rarity, bigger priority
	for i := len(bucktes) - 1; i >= 0; i-- {
		indexes := bucktes[i]
		// 2) randomize the inverted arrays
		rand.Shuffle(
			len(indexes),
			// shuffle by swapping
			func(i, j int) {
				indexes[i], indexes[j] = indexes[j], indexes[i]
			},
		)
		// 3) concatenate the inverted arrays sorted by rarity values in descending order
		res = append(res, indexes...)
	}
	return res
}

func (p *Peer) getPieceRequestOrderByRarestFirst() []RequestIndex {
	t := p.t
	pc := p.t.cl.findPeerConnByTorrentAddr(t, p.RemoteAddr.String())
	if pc == nil {
		// this part of code may by executed before connecting to any peer
		// so, just throw a warning
		log.Fstr(
			"FindPeerConnByTorrentAddr return nil, target is %s", p.RemoteAddr.String(),
		).LogLevel(log.Warning, t.logger)
		for _, tmp := range p.t.cl.findPeerConnsByTorrent(p.t) {
			log.Fstr(
				"FindPeerConnByTorrentAddr what can be found is %s, present is %s",
				tmp.RemoteAddr.String(), p.RemoteAddr.String(),
			).LogLevel(log.Warning, t.logger)
		}
		return nil
	}

	// 找到处理该torrent的所有peer
	peers := p.t.cl.findPeerConnsByTorrent(p.t)
	// rarity数组
	rarity := make([]RarityContentType, p.t.numPieces())
	// 创建peers的int数组
	piecePossessionByPeers := make([][]RarityContentType, len(peers))
	for i := 0; i < len(peers); i++ {
		piecePossessionByPeer := peers[i].ToIndexArray()
		// if peer HaveAll, piecePossessionByPeer is all-zero at any time
		// else, piecePossessionByPeer is all-zero at the beginning
		if peers[i].peerSentHaveAll {
			for i := range piecePossessionByPeer {
				piecePossessionByPeer[i] = 1
			}
		}
		piecePossessionByPeers[i] = piecePossessionByPeer
		// 基于peers数组计算rarity
		for k, v := range piecePossessionByPeer {
			rarity[k] += (1 - v)
		}
		// print peer and piecePossession
		log.Fstr(
			"%s piecePossession is %v",
			peers[i].RemoteAddr.String(), piecePossessionByPeer,
		).LogLevel(log.Debug, t.logger)
	}

	// randomSeed := time.Now().Unix()
	// randomSeed := int64(p.remoteIpPort().Port)
	randomSeed := p.t.cl.config.RandomSeed
	// 根据rarity值对index进行排序(piece)
	sortedRarityIndex := classifyByValueWithIndexRandomized(rarity, randomSeed)
	log.Fstr(
		"%s sortedRarityIndex is %v, random seed is %d",
		p.RemoteAddr.String(), sortedRarityIndex, randomSeed,
	).LogLevel(log.Debug, t.logger)

	var presentPeerPiecePossession []RarityContentType
	presentPeerPiecePossession = pc.ToIndexArray()
	if pc.peerSentHaveAll {
		// if peer HaveAll, presentPeerPiecePossession should all be set to one
		for i := range presentPeerPiecePossession {
			presentPeerPiecePossession[i] = 1
		}
		// if peer HaveAll, _peerPieces is empty
		log.Fstr(
			"%s HaveAll, _peerPieces is %v %v",
			p.RemoteAddr.String(), pc._peerPieces.ToArray(), pc._peerPieces.IsEmpty(),
		).LogLevel(log.Debug, t.logger)
	}

	// to avoid frequent array expansion by append operation, set the maximum array size
	requests, numChunksUsed := make([]RequestIndex, t.numChunks()), 0            // 对该peer的chunk请求顺序
	requestedPieceIndex, numPiecesUsed := make([]RequestIndex, t.numPieces()), 0 // 对该peer请求的chunk属于哪些piece
	lastPieceIndex := t.numPieces() - 1
	for i := 0; i < len(sortedRarityIndex); i++ {
		presentPicesIndex := sortedRarityIndex[i]
		// check the piece
		if presentPeerPiecePossession[presentPicesIndex] != 1 {
			// this peer doesn't has this piece
			continue
		}
		// TODO: check the potential requests over again, which is done by adding more condition judgement
		if t.pieceComplete(presentPicesIndex) {
			// this piece has been completed
			continue
		}
		if t.hashingPiece(presentPicesIndex) || t.pieceQueuedForHash(presentPicesIndex) {
			// this piece is being or to be hashed
			continue
		}
		requestedPieceIndex[numPiecesUsed] = RequestIndex(presentPicesIndex)
		numPiecesUsed++

		var chunksNum int // 该piece中有多少chunk
		if presentPicesIndex != lastPieceIndex {
			chunksNum = int(t.chunksPerRegularPiece())
		} else {
			chunksNum = int(t.pieceNumChunks(lastPieceIndex))
		}
		allowedFast := p.peerAllowedFast.Contains(presentPicesIndex)
		for j := 0; j < chunksNum; j++ {
			chunkIndex := RequestIndex(presentPicesIndex)*t.chunksPerRegularPiece() + RequestIndex(j)
			// 当前peer已经请求该chunk
			if p.requestState.Requests.Contains(chunkIndex) {
				continue
			}
			// 当前client已完成该chunk
			// ppReq:=types.Request{
			// 	Index: presentPicesIndex*int(t.chunksPerRegularPiece()),
			// 	Begin:j*int(t.chunkSize),
			// 	Length:t.chunkSize,
			// }
			if t.haveChunk(t.requestIndexToRequest(chunkIndex)) {
				continue
			}
			// check the chunks
			if !allowedFast {
				if p.peerChoking && !p.requestState.Requests.Contains(chunkIndex) {
					// We can't request this right now.
					continue
				}
			}
			if p.requestState.Cancelled.Contains(chunkIndex) {
				// Can't re-request while awaiting acknowledgement.
				continue
			}

			requests[numChunksUsed] = chunkIndex
			numChunksUsed++
		}
	}
	requests = requests[:numChunksUsed]
	requestedPieceIndex = requestedPieceIndex[:numPiecesUsed]
	log.Fstr(
		"%s piece bitmap: %v, present peer: %v, requestedPieceIndex is %v, request chunk indexes: %v",
		p.RemoteAddr.String(), pc._peerPieces.ToArray(), t._completedPieces.ToArray(), requestedPieceIndex, requests,
	).LogLevel(log.Debug, t.logger)
	return requests
}

func (p *Peer) maybeUpdateActualRequestState() {
	if p.closed.IsSet() {
		return
	}
	if p.needRequestUpdate == "" {
		return
	}
	if p.needRequestUpdate == peerUpdateRequestsTimerReason {
		since := time.Since(p.lastRequestUpdate)
		if since < updateRequestsTimerDuration {
			panic(since)
		}
	}
	pprof.Do(
		context.Background(),
		pprof.Labels("update request", p.needRequestUpdate),
		func(_ context.Context) {
			if p.t.cl.config.PieceSelectionStrategy == request_strategy.RandomSelectionStrategy {
				next := p.getDesiredRequestState()
				p.applyRequestState(next)
				p.t.requestIndexes = next.Requests.requestIndexes[:0]
			} else if p.t.cl.config.PieceSelectionStrategy == request_strategy.RFSelectionStrategy {
				p.useRarityFirst()
			}
		},
	)
}

// Transmit/action the request state to the peer.
func (p *Peer) applyRequestState(next desiredRequestState) {
	current := &p.requestState
	if !p.setInterested(next.Interested) {
		panic("insufficient write buffer")
	}
	more := true
	requestHeap := binheap.FromSlice(next.Requests.requestIndexes, next.Requests.lessByValue)
	// use this to get the cost of calculating rarity
	// requestHexap := newArrayWrapper(p.getPieceRequestOrderByRarestFirst())
	t := p.t
	originalRequestCount := current.Requests.GetCardinality()
	// We're either here on a timer, or because we ran out of requests. Both are valid reasons to
	// alter peakRequests.
	if originalRequestCount != 0 && p.needRequestUpdate != peerUpdateRequestsTimerReason {
		panic(fmt.Sprintf(
			"expected zero existing requests (%v) for update reason %q",
			originalRequestCount, p.needRequestUpdate))
	}
	for requestHeap.Len() != 0 && maxRequests(current.Requests.GetCardinality()+current.Cancelled.GetCardinality()) < p.nominalMaxRequests() {
		req := requestHeap.Pop()
		existing := t.requestingPeer(req)
		if existing != nil && existing != p {
			// Don't steal from the poor.
			diff := int64(current.Requests.GetCardinality()) + 1 - (int64(existing.uncancelledRequests()) - 1)
			// Steal a request that leaves us with one more request than the existing peer
			// connection if the stealer more recently received a chunk.
			if diff > 1 || (diff == 1 && p.lastUsefulChunkReceived.Before(existing.lastUsefulChunkReceived)) {
				continue
			}
			// steal request from existing peer
			t.cancelRequest(req)
			log.Fstr("cancel request %s for chunk %d", existing.RemoteAddr.String(), req).LogLevel(log.Debug, t.logger)
		}
		// chunk is the minimum request unit
		more = p.mustRequest(req)
		if !more {
			break
		}
	}
	if !more {
		// This might fail if we incorrectly determine that we can fit up to the maximum allowed
		// requests into the available write buffer space. We don't want that to happen because it
		// makes our peak requests dependent on how much was already in the buffer.
		panic(fmt.Sprintf(
			"couldn't fill apply entire request state [newRequests=%v]",
			current.Requests.GetCardinality()-originalRequestCount))
	}
	newPeakRequests := maxRequests(current.Requests.GetCardinality() - originalRequestCount)
	// log.Printf(
	// 	"requests %v->%v (peak %v->%v) reason %q (peer %v)",
	// 	originalRequestCount, current.Requests.GetCardinality(), p.peakRequests, newPeakRequests, p.needRequestUpdate, p)
	p.peakRequests = newPeakRequests
	p.needRequestUpdate = ""
	p.lastRequestUpdate = time.Now()
	if enableUpdateRequestsTimer {
		p.updateRequestsTimer.Reset(updateRequestsTimerDuration)
	}
}

type arrayWrapper[T any] struct {
	data []T
}

func newArrayWrapper[T any](data []T) *arrayWrapper[T] {
	return &arrayWrapper[T]{data}
}

func (aw *arrayWrapper[T]) Len() int {
	return len(aw.data)
}

func (aw *arrayWrapper[T]) Pop() T {
	ret := aw.data[0]
	aw.data = aw.data[1:]
	return ret
}

func (p *Peer) useRarityFirst() {
	current := &p.requestState
	t := p.t
	more := true

	requestHeap := newArrayWrapper(p.getPieceRequestOrderByRarestFirst())
	originalRequestCount := current.Requests.GetCardinality()
	// We're either here on a timer, or because we ran out of requests. Both are valid reasons to
	// alter peakRequests.
	if originalRequestCount != 0 && p.needRequestUpdate != peerUpdateRequestsTimerReason {
		panic(fmt.Sprintf(
			"expected zero existing requests (%v) for update reason %q",
			originalRequestCount, p.needRequestUpdate))
	}

	for requestHeap.Len() != 0 && maxRequests(current.Requests.GetCardinality()+current.Cancelled.GetCardinality()) < p.nominalMaxRequests() {
		req := requestHeap.Pop()
		existing := t.requestingPeer(req)
		// like tcp, don't cancel any previous request
		// 已经向某个peer请求该chunk
		if existing != nil {
			log.Fstr("don't cancel request for chunk %d, present: %s, existing: %s", req, p.RemoteAddr.String(), existing.RemoteAddr.String()).LogLevel(log.Debug, t.logger)
			continue
		}
		// 当前client已完成该chunk
		if t.haveChunk(t.requestIndexToRequest(req)) {
			log.Fstr("req %d has been completed.", req).LogLevel(log.Debug, t.logger)
			continue
		}

		// chunk is the minimum request unit
		more = p.mustRequest(req)
		if !more {
			break
		}
	}
	// Check whether the error exists in log file
	if !more {
		// This might fail if we incorrectly determine that we can fit up to the maximum allowed
		// requests into the available write buffer space. We don't want that to happen because it
		// makes our peak requests dependent on how much was already in the buffer.
		panic(fmt.Sprintf(
			"couldn't fill apply entire request state [newRequests=%v]",
			current.Requests.GetCardinality()-originalRequestCount))
	}
	newPeakRequests := maxRequests(current.Requests.GetCardinality() - originalRequestCount)
	p.peakRequests = newPeakRequests
	p.needRequestUpdate = ""
	p.lastRequestUpdate = time.Now()
	if enableUpdateRequestsTimer {
		p.updateRequestsTimer.Reset(updateRequestsTimerDuration)
	}
}

// This could be set to 10s to match the unchoke/request update interval recommended by some
// specifications. I've set it shorter to trigger it more often for testing for now.
const (
	updateRequestsTimerDuration = 3 * time.Second
	enableUpdateRequestsTimer   = false
)
