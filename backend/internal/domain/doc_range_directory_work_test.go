package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func frozenRangeDirectory(test testing.TB, hashes []ContentHash) (DocRangeDirectoryRoot, []DocRangeObject) {
	test.Helper()
	root := DocRangeDirectoryRoot{Count: len(hashes)}
	var objects []DocRangeObject
	if len(hashes) == 0 {
		return root, objects
	}
	level, span := 0, 1
	for {
		var parents []ContentHash
		for position := 0; position < len(hashes); position += DocRangeDirectoryFanout {
			directory := docRangeDirectory{Level: level, Start: position * span, Hashes: hashes[position:min(position+DocRangeDirectoryFanout, len(hashes))]}
			raw, err := canonicalJSON(directory)
			if err != nil {
				test.Fatal(err)
			}
			hash := HashContent(raw)
			objects = append(objects, DocRangeObject{Hash: hash, Bytes: raw})
			parents = append(parents, hash)
		}
		if len(parents) == 1 {
			root.Hash = parents[0]
			return root, objects
		}
		hashes, level, span = parents, level+1, span*DocRangeDirectoryFanout
	}
}

func TestDocRangeDirectoryWorkPreservesLevelOrderAndBounds(test *testing.T) {
	for _, count := range []int{0, 1, 127, 128, 129, 16384, 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			hashes := make([]ContentHash, count)
			for ordinal := range hashes {
				hashes[ordinal] = HashContent([]byte(fmt.Sprint(ordinal)))
			}
			want, expected := frozenRangeDirectory(test, hashes)
			work, err := newDocRangeDirectoryWork(hashes)
			if err != nil {
				test.Fatal(err)
			}
			var actual []DocRangeObject
			for step := 0; step < len(expected)+1; step++ {
				calls := 0
				complete, err := work.step(context.Background(), func(object DocRangeObject) error {
					calls++
					if len(object.Bytes) > MaxDocRangeDirectoryBytes || HashContent(object.Bytes) != object.Hash {
						test.Fatal("directory quantum exceeds its authenticated object bound")
					}
					actual = append(actual, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
					object.Bytes[0] ^= 1
					return nil
				})
				if err != nil || calls > 1 {
					test.Fatal("directory step emitted more than one object or failed", calls, err)
				}
				estimate := docRangeHeaderHeapEstimate{}
				work.retained(&estimate)
				if estimate.err != nil || estimate.bytes <= 0 {
					test.Fatal("directory state omitted its retained storage", estimate)
				}
				if complete {
					if !reflect.DeepEqual(actual, expected) || work.root != want || work.hashes != nil || len(work.parents) != 0 {
						test.Fatal("directory changed ordered bytes/root or retained finished input")
					}
					complete, err = work.step(context.Background(), func(DocRangeObject) error {
						test.Fatal("completed directory called its sink again")
						return nil
					})
					if !complete || err != nil {
						test.Fatal("completed directory is not idempotent", err)
					}
					return
				}
			}
			test.Fatal("directory state did not terminate")
		})
	}
}

func TestDocRangeChunkDirectoryWorkPreservesOrderedPages(test *testing.T) {
	for _, count := range []int{0, 1, 128, 129, 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			chunks := make([]ConversationManifestChunk, count)
			var hashes []ContentHash
			var expected []DocRangeObject
			for ordinal := range chunks {
				chunks[ordinal] = ConversationManifestChunk{Hash: HashContent([]byte(fmt.Sprint(ordinal))), Bytes: int64(ordinal + 1)}
			}
			for start := 0; start < len(chunks); start += DocRangeChunkPageSize {
				raw, err := canonicalJSON(chunks[start:min(start+DocRangeChunkPageSize, len(chunks))])
				if err != nil {
					test.Fatal(err)
				}
				hash := HashContent(raw)
				expected = append(expected, DocRangeObject{Hash: hash, Bytes: raw})
				hashes = append(hashes, hash)
			}
			want, directories := frozenRangeDirectory(test, hashes)
			expected = append(expected, directories...)
			work, err := newDocRangeChunkDirectoryWork(chunks)
			if err != nil {
				test.Fatal(err)
			}
			var actual []DocRangeObject
			for step := 0; step <= len(expected); step++ {
				calls := 0
				complete, err := work.step(context.Background(), func(object DocRangeObject) error {
					calls++
					actual = append(actual, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
					object.Bytes[0] ^= 1
					return nil
				})
				if err != nil || calls > 1 {
					test.Fatal("chunk directory step exceeded its object bound", calls, err)
				}
				if complete {
					if !reflect.DeepEqual(actual, expected) || work.root != want || work.chunks != nil || work.pages != nil || work.directory != nil {
						test.Fatal("chunk directory changed its ordered pages/root or retained completed input")
					}
					return
				}
			}
			test.Fatal("chunk directory did not terminate")
		})
	}
}

func TestDocRangeDirectoryWorkCancellationAndSinkFailure(test *testing.T) {
	for _, fault := range []string{"pre-canceled", "sink-error", "sink-cancel"} {
		test.Run(fault, func(test *testing.T) {
			work, err := newDocRangeDirectoryWork([]ContentHash{HashContent([]byte("leaf"))})
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("directory sink failed")
			want := failure
			if fault == "pre-canceled" {
				cancel()
				want = context.Canceled
			}
			calls := 0
			complete, err := work.step(ctx, func(DocRangeObject) error {
				calls++
				if fault == "sink-cancel" {
					cancel()
					return nil
				}
				return failure
			})
			if fault == "sink-cancel" {
				want = context.Canceled
			}
			if complete || !errors.Is(err, want) || work.done || work.root.Hash != "" || work.position != 0 || (fault == "pre-canceled" && calls != 0) {
				test.Fatal("failed directory acknowledged data or lost cancellation", complete, err, calls)
			}
			if fault == "pre-canceled" {
				if work.failure != nil {
					test.Fatal("pre-canceled step poisoned unstarted work")
				}
			} else if complete, err := work.step(context.Background(), func(DocRangeObject) error {
				test.Fatal("failed directory resumed callbacks")
				return nil
			}); complete || !errors.Is(err, ErrIntegrity) {
				test.Fatal("failed directory resumed its proof", complete, err)
			}
		})
	}
}
