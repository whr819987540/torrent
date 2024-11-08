package metainfo

import (
	"crypto/sha1"
	"io"
)

func GeneratePieces(r io.Reader, pieceLength int64, b []byte) ([]byte, error) {
	for {
		h := sha1.New()
		written, err := io.CopyN(h, r, pieceLength)
		if written > 0 {
			b = h.Sum(b)
		}
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return b, err
		}
	}
}

func GeneratePiecesFromMemory(reader io.Reader, pieceLength int64, b []byte) ([]byte, error) {
	for {
		h := sha1.New()
		// copy pieceLength bytes from reader to h
		num, err := io.CopyN(h, reader, pieceLength)
		if num > 0 {
			b = h.Sum(b)
		}
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return b, err
		}
	}

}
