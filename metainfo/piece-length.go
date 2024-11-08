// From https://github.com/jackpal/Taipei-Torrent

// Copyright (c) 2010 Jack Palevich. All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package metainfo

import (
	"math"
)

// For more context on why these numbers, see http://wiki.vuze.com/w/Torrent_Piece_Size
const (
	minimumPieceLength   = 64 * 1024
	maximumPieceLength   = 2 * 1024 * 1024
	targetPieceCountLog2 = 10
	targetPieceCountMin  = 1 << targetPieceCountLog2
)

// Target piece count should be < targetPieceCountMax
const targetPieceCountMax = targetPieceCountMin << 1

// Choose a good piecelength.
// piecelength >= 64KB and piecelength <= 2MB
// piecelength = 64KB*X
// totalLength / piecelength = piecenumber <= 2048
func ChoosePieceLength(totalLength int64) (pieceLength int64) {
	// Must be a power of 2.
	// Must be a multiple of 64KB
	// Prefer to provide around 1024..2048 pieces.
	pieceLength = minimumPieceLength
	pieces := int64(math.Ceil(float64(totalLength) / float64(pieceLength)))
	for pieces > targetPieceCountMax {
		if (pieceLength << 1) > maximumPieceLength {
			break
		}
		pieceLength <<= 1
		pieces >>= 1
	}
	return
}
