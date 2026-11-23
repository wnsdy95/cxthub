package domain

import (
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
