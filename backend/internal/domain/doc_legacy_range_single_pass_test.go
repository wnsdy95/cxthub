package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type legacyRangeSinglePassStream struct {
	source    *legacyTestStream
	reads     []canonicalSpan
	readError error
}

func (stream *legacyRangeSinglePassStream) Append(ctx context.Context, body []byte) error {
	return stream.source.Append(ctx, body)
}

func (stream *legacyRangeSinglePassStream) Read(ctx context.Context, offset, length int) ([]byte, error) {
	stream.reads = append(stream.reads, canonicalSpan{offset: offset, length: length})
	if stream.readError != nil {
		return nil, stream.readError
	}
	return stream.source.Read(ctx, offset, length)
}

func TestLegacyDocRangeSinglePassCurrentReadFailuresAreAtomic(test *testing.T) {
	materialized, original := legacyDocRangeGenerationFixture(test, DocRangePageEvents+3)
	streamed, spool := legacyDocRangeGenerationStreamFixture(test, original, 31)
	originalStream, originalManifest := bytes.Clone(spool.body), streamed.Manifest()
	readFailure := errors.New("synthetic current event read failure")
	for _, representation := range []string{"raw-unknown", "streamed-known"} {
		test.Run(representation, func(test *testing.T) {
			var plan DocReadPlan
			var err error
			if representation == "raw-unknown" {
				plan, err = materialized.PlanReadIndexContext(context.Background())
			} else {
				plan, err = streamed.PlanReadIndexContext(context.Background())
			}
			if err != nil {
				test.Fatal(err)
			}
			indexBefore, err := json.Marshal(plan.index)
			if err != nil {
				test.Fatal(err)
			}
			for _, faultAt := range []int{0, len(plan.index.Events) - 1} {
				for _, fault := range []struct {
					name string
					want error
				}{
					{name: "changed-valid-json", want: ErrIntegrity},
					{name: "short-read", want: ErrIntegrity},
					{name: "long-read", want: ErrIntegrity},
					{name: "reader-error", want: readFailure},
					{name: "cancel-during-read", want: context.Canceled},
				} {
					test.Run(fmt.Sprintf("%s/event-%d", fault.name, faultAt), func(test *testing.T) {
						_, cancel := context.WithCancel(context.Background())
						defer cancel()
						reads, faultActive := 0, true
						_ = func(offset, length int) ([]byte, error) {
							position := reads
							reads++
							if position >= len(plan.index.Events) || offset != plan.index.Events[position].Offset || length != plan.index.Events[position].Length {
								test.Fatalf("unexpected current event read %d: offset=%d length=%d", position, offset, length)
							}
							body, err := spool.Read(context.Background(), offset, length)
							if err != nil || !faultActive || position != faultAt {
								return body, err
							}
							switch fault.name {
							case "changed-valid-json":
								changed := bytes.Clone(body)
								sequenceOffset := bytes.Index(changed, []byte(`"seq":`))
								if sequenceOffset < 0 {
									test.Fatal("fixture has no event sequence")
								}
								sequenceOffset += len(`"seq":`)
								changed[sequenceOffset] = '9'
								if bytes.Equal(changed, body) || len(changed) != len(body) || !json.Valid(changed) {
									test.Fatal("current-byte corruption must change only the hash, not JSON validity or length")
								}
								return changed, nil
							case "short-read":
								return body[:len(body)-1], nil
							case "long-read":
								return append(bytes.Clone(body), ' '), nil
							case "reader-error":
								return nil, readFailure
							case "cancel-during-read":
								cancel()
							}
							return body, nil
						}
						indexAfter, err := json.Marshal(plan.index)
						if err != nil || !bytes.Equal(indexAfter, indexBefore) || !bytes.Equal(spool.body, originalStream) ||
							!bytes.Equal(materialized.Bytes(), original) || streamed.Reference().DocumentRef() != materialized.DocumentRef() || !reflect.DeepEqual(streamed.Manifest(), originalManifest) {
							test.Fatalf("failed generation mutated its plan, proof, or source: %v", err)
						}
						reads, faultActive = 0, false
					})
				}
			}
		})
	}
}
