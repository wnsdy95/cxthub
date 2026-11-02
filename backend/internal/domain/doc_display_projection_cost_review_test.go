package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func projectionCostReviewFrontier(test testing.TB, count int, done bool) (DocDisplayProjectionSource, DocDisplayProjectionState) {
	test.Helper()
	textBytes := count * DocDisplayPageRuns
	source := projectionTestSource(textBytes)
	if !done {
		source.TextBytes += DocDisplayPageRuns
	}
	state, err := NewDocDisplayProjectionState(source)
	if err != nil {
		test.Fatal(err)
	}
	state.TextBytes, state.Directory.Count, state.Done = textBytes, count, done
	hash := HashContent([]byte("prior projection frontier"))
	for remaining := count; remaining > 0; remaining /= DocRangeDirectoryFanout {
		level := make([]ContentHash, remaining%DocRangeDirectoryFanout)
		for position := range level {
			level[position] = hash
		}
		state.Directory.Levels = append(state.Directory.Levels, level)
	}
	if err := state.Validate(source); err != nil {
		test.Fatal(err)
	}
	return source, state
}

func TestDocDisplayProjectionCostReviewDirectoryParity(test *testing.T) {
	for _, count := range []int{0, 1, 128, 129, 16384, 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			source, state := projectionCostReviewFrontier(test, max(0, count-1), count == 0)
			before, err := state.Encode(source)
			if err != nil {
				test.Fatal(err)
			}
			next := state
			stored := make(map[ContentHash][]byte)
			text := strings.Repeat("z", DocDisplayPageRuns)
			if count > 0 {
				page := projectionTestPage(test, source, state.Directory.Count, state.TextBytes, text)
				raw, err := page.EncodeProjection(source)
				if err != nil {
					test.Fatal(err)
				}
				hash := HashContent(raw)
				directory, expectedObjects, err := state.Directory.Append(context.Background(), hash)
				if err != nil {
					test.Fatal(err)
				}
				expected := state
				expected.Directory, expected.TextBytes, expected.Done = directory, source.TextBytes, true
				expectedObjects = append(expectedObjects, DocRangeObject{Hash: hash, Bytes: raw})
				var objects []DocRangeObject
				next, objects, err = AppendDocDisplayProjectionPage(context.Background(), source, state, page)
				if err != nil || !reflect.DeepEqual(next, expected) || !reflect.DeepEqual(objects, expectedObjects) {
					test.Fatal("append changed native directory objects or progress", err)
				}
				docDisplayStringTestStore(test, stored, objects)
			}
			root, expectedObjects, err := next.Directory.Finish(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			generation, objects, err := FinishDocDisplayProjection(context.Background(), source, next)
			expected := DocDisplayProjectionGeneration{Version: 1, Source: source, Pages: root}
			if err != nil || generation != expected || !reflect.DeepEqual(objects, expectedObjects) {
				test.Fatal("finish changed native directory objects or generation", err)
			}
			docDisplayStringTestStore(test, stored, objects)
			checkpoint, err := next.Encode(source)
			if err != nil {
				test.Fatal(err)
			}
			canonical, err := canonicalJSON(next)
			if err != nil || !bytes.Equal(checkpoint, canonical) {
				test.Fatal("checkpoint bytes changed", err)
			}
			decoded, err := DecodeDocDisplayProjectionState(source, HashContent(checkpoint), checkpoint)
			if err != nil || !reflect.DeepEqual(decoded, next) {
				test.Fatal("checkpoint did not resume", err)
			}
			loads := 0
			load := func(_ context.Context, hash ContentHash) ([]byte, error) {
				loads++
				return stored[hash], nil
			}
			page, err := ReadDocDisplayProjectionPage(context.Background(), generation, max(0, count-1), load)
			if count == 0 {
				if !errors.Is(err, ErrIntegrity) || loads != 0 {
					test.Fatal("empty generation loaded objects", loads, err)
				}
			} else if err != nil || page.Index != count-1 || page.Text != text || page.From+len(page.Text) != source.TextBytes || loads > 4 {
				test.Fatal("selected read changed", loads, err)
			}
			for _, level := range next.Directory.Levels {
				if len(level) > 0 {
					level[0] = HashContent([]byte("changed returned frontier"))
				}
			}
			for _, object := range objects {
				object.Bytes[0] ^= 1
			}
			after, err := state.Encode(source)
			if err != nil || !bytes.Equal(before, after) {
				test.Fatal("append/finish results alias the input frontier", err)
			}
		})
	}
}

func TestDocDisplayProjectionCostReviewRejectsMutableFrontierCorruption(test *testing.T) {
	for _, count := range []int{0, 1, 129, 16383, 16384, 16385} {
		for _, done := range []bool{false, true} {
			test.Run(fmt.Sprintf("%d/%t", count, done), func(test *testing.T) {
				source, original := projectionCostReviewFrontier(test, count, done)
				mutations := []func(*DocDisplayProjectionState){func(state *DocDisplayProjectionState) {
					state.Directory.Levels = append(state.Directory.Levels, nil)
				}}
				for level, hashes := range original.Directory.Levels {
					if len(hashes) == 0 {
						continue
					}
					for _, position := range []int{0, len(hashes) - 1} {
						mutations = append(mutations, func(state *DocDisplayProjectionState) {
							state.Directory.Levels[level][position] = ContentHash("sha256:" + strings.Repeat("g", 64))
						})
					}
				}
				for mutationIndex, mutate := range mutations {
					state := original
					state.Directory = original.Directory.clone()
					page := projectionTestPage(test, source, count, state.TextBytes, strings.Repeat("z", DocDisplayPageRuns))
					operation := func() error {
						if done {
							generation, objects, err := FinishDocDisplayProjection(context.Background(), source, state)
							if err != nil && (generation != (DocDisplayProjectionGeneration{}) || objects != nil) {
								test.Fatal("failed finish leaked effects", mutationIndex, err)
							}
							return err
						}
						next, objects, err := AppendDocDisplayProjectionPage(context.Background(), source, state, page)
						if err != nil && (!reflect.DeepEqual(next, DocDisplayProjectionState{}) || objects != nil) {
							test.Fatal("failed append leaked effects", mutationIndex, err)
						}
						return err
					}
					if err := operation(); err != nil {
						test.Fatal("valid frontier failed before mutation", mutationIndex, err)
					}
					mutate(&state)
					if err := state.Validate(source); !errors.Is(err, ErrIntegrity) {
						test.Fatal("validation accepted mutated frontier", mutationIndex, err)
					}
					if _, err := state.Encode(source); !errors.Is(err, ErrIntegrity) {
						test.Fatal("encoding accepted mutated frontier", mutationIndex, err)
					}
					raw, err := marshalDocDisplayProjectionState(state)
					if err != nil {
						test.Fatal(err)
					}
					decoded, err := DecodeDocDisplayProjectionState(source, HashContent(raw), raw)
					if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayProjectionState{}) {
						test.Fatal("decoding accepted hash-consistent mutated frontier", mutationIndex, err)
					}
					if err := operation(); !errors.Is(err, ErrIntegrity) {
						test.Fatal("operation accepted mutated frontier", mutationIndex, err)
					}
				}
			})
		}
	}
}
