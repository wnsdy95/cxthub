package domain

import (
	"testing"
)

func FuzzMarshalDocDisplayProjectionStateCanonicalJSON(fuzz *testing.F) {
	fuzz.Add("source", "page", int64(1), uint8(1), false)
	fuzz.Add("\xff", "\ufffd\xed\xa0\x80", int64(1<<53+1), uint8(2), true)
	fuzz.Add("<>&", "\ud55c\uae00🙂\x00\"\\/", int64(-1<<63), uint8(0), false)
	fuzz.Fuzz(func(test *testing.T, source, hash string, number int64, shape uint8, done bool) {
		var levels [][]ContentHash
		if shape != 0 {
			levels = make([][]ContentHash, int(shape)%4)
			for level := range levels {
				if level%2 == 0 {
					levels[level] = []ContentHash{ContentHash(hash), ""}
				}
			}
		}
	})
}
