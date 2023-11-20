package types

import (
	pp "github.com/anacrolix/torrent/peer_protocol"
)

type PieceIndex = int

type ChunkSpec struct {
	Begin, Length pp.Integer
}

// Request is made in chunks.
// Index is the index of the piece that the chunk belongs to.
// Begin is is the starting offset of the chunk within the picec it belongs to.
// Length is the length of the chunk. From BitTorrent's document, it's usually set to 16KB.
type Request struct {
	Index pp.Integer
	ChunkSpec
}

func (r Request) ToMsg(mt pp.MessageType) pp.Message {
	return pp.Message{
		Type:   mt,
		Index:  r.Index,
		Begin:  r.Begin,
		Length: r.Length,
	}
}

// Describes the importance of obtaining a particular piece.
type PiecePriority byte

func (pp *PiecePriority) Raise(maybe PiecePriority) bool {
	if maybe > *pp {
		*pp = maybe
		return true
	}
	return false
}

// Priority for use in PriorityBitmap
func (me PiecePriority) BitmapPriority() int {
	return -int(me)
}

const (
	PiecePriorityNone      PiecePriority = iota // Not wanted. Must be the zero value.
	PiecePriorityNormal                         // Wanted.
	PiecePriorityHigh                           // Wanted a lot.
	PiecePriorityReadahead                      // May be required soon.
	// Succeeds a piece where a read occurred. Currently the same as Now,
	// apparently due to issues with caching.
	PiecePriorityNext
	PiecePriorityNow // A Reader is reading in this piece. Highest urgency.
)
