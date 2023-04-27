package metainfo

import "testing"

func TestChoosePieceLength(t *testing.T) {
	testCases := []struct {
		name        string // test case name
		totalLength int64
		want        int64 // desired piece length
	}{
		{name: "1KB file", totalLength: 1024, want: minimumPieceLength},
		{name: "minimumPieceLength KB file", totalLength: minimumPieceLength, want: minimumPieceLength},
		{name: "minimumPieceLength KB + 1B file", totalLength: minimumPieceLength + 1, want: minimumPieceLength},

		{name: "max piece number", totalLength: targetPieceCountMax * minimumPieceLength, want: minimumPieceLength},
		{name: "over max piece number", totalLength: targetPieceCountMax*minimumPieceLength + 1, want: minimumPieceLength * 2},

		{name: "larger file size", totalLength: 10 * 1024 * 1024 * 1024, want: maximumPieceLength},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ChoosePieceLength(tc.totalLength)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			} else {
				t.Logf("got %d, piece number %d", got,tc.totalLength/got)
			}
		})
	}
}
