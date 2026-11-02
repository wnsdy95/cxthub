package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestDocRangeAccumulatorMatchesBatchDirectory(test *testing.T) {
	for _, count := range []int{0, 1, 127, 128, 129, 255, 256, 16383, 16384, 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			var accumulator DocRangeDirectoryAccumulator
			var hashes []ContentHash
			objects := map[ContentHash][]byte{}
			for index := 0; index < count; index++ {
				hash := HashContent([]byte(fmt.Sprint(index)))
				hashes = append(hashes, hash)
				before, _ := json.Marshal(accumulator)
				next, produced, err := accumulator.Append(context.Background(), hash)
				if err != nil || next.Validate() != nil {
					test.Fatal(index, err)
				}
				after, _ := json.Marshal(accumulator)
				if string(before) != string(after) {
					test.Fatal("append mutated committed checkpoint")
				}
				for _, object := range produced {
					objects[object.Hash] = object.Bytes
				}
				accumulator = next
			}
			before, _ := json.Marshal(accumulator)
			root, produced, err := accumulator.Finish(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			after, _ := json.Marshal(accumulator)
			if string(before) != string(after) {
				test.Fatal("finish mutated resumable frontier")
			}
			for _, object := range produced {
				objects[object.Hash] = object.Bytes
			}
			var expectedObjects []DocRangeObject
			expected, err := buildDocRangeDirectory(context.Background(), hashes, &expectedObjects)
			if err != nil || root != expected {
				test.Fatal("incremental directory changed content identity", root, expected, err)
			}
			for _, object := range expectedObjects {
				if !reflect.DeepEqual(objects[object.Hash], object.Bytes) {
					test.Fatal("object changed", object.Hash)
				}
			}
			if count > 0 {
				reads := 0
				for _, position := range []int{0, count / 2, count - 1} {
					before := reads
					got, err := ResolveDocRangeLeaf(context.Background(), root, position, func(_ context.Context, hash ContentHash) ([]byte, error) { reads++; return objects[hash], nil })
					if err != nil || got != hashes[position] || reads-before > 3 {
						test.Fatal(position, got, err, reads-before)
					}
				}
			}
		})
	}
}

func TestDocRangeAccumulatorMalformedAndCanceled(test *testing.T) {
	hash := HashContent([]byte("leaf"))
	for _, accumulator := range []DocRangeDirectoryAccumulator{
		{Count: -1}, {Count: MaxDocRangePages + 1}, {Count: 1}, {Count: 0, Levels: [][]ContentHash{{hash}}},
		{Count: 128, Levels: [][]ContentHash{{hash}}}, {Count: 1, Levels: [][]ContentHash{{"bad"}}},
	} {
		if accumulator.Validate() == nil {
			test.Fatal("invalid frontier", accumulator)
		}
		if _, objects, err := accumulator.Append(context.Background(), hash); !errors.Is(err, ErrIntegrity) || len(objects) != 0 {
			test.Fatal(objects, err)
		}
		if _, objects, err := accumulator.Finish(context.Background()); !errors.Is(err, ErrIntegrity) || len(objects) != 0 {
			test.Fatal(objects, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, objects, err := (DocRangeDirectoryAccumulator{}).Append(ctx, hash); !errors.Is(err, context.Canceled) || len(objects) != 0 {
		test.Fatal(objects, err)
	}
}
