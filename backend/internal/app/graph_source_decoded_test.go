package app

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphCaptureDecodedInputsRemainPrivateAcrossDeltas(test *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	base, err := CaptureGraphSource(ctx, unchangedGraphCaptureSource{page: graphCapturePage(checkpoint, "baseline", entries, "")}, checkpoint.RepoID, nil)
	if err != nil {
		test.Fatal(err)
	}
	wantRepo, wantView, wantEvidence, err := base.Inputs(ctx)
	if err != nil {
		test.Fatal(err)
	}
	metadata, view, evidence, err := base.prepareInputs(ctx)
	if err != nil || base.decoded == nil || !reflect.DeepEqual(metadata, wantRepo) || !reflect.DeepEqual(view, wantView) || !reflect.DeepEqual(evidence, wantEvidence) {
		test.Fatal("prepared inputs differ from full decode", err)
	}
	evidence[0].Value[0] = '['
	decoder := base.decoded
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			next := checkpoint
			next.Sequence++
			delta := []domain.RepositoryViewSourceEntry{{Kind: "revision", Key: string(checkpoint.RepoID), Sequence: next.Sequence, Value: []byte(`{"graph":"7","pending":"8"}`)}}
			updated, err := CaptureGraphSource(ctx, unchangedGraphCaptureSource{page: graphCapturePage(next, "delta", delta, "")}, checkpoint.RepoID, &base)
			if err != nil || updated.decoded != decoder {
				test.Error("delta did not retain private base decoder", err)
				return
			}
			expectedRepo, expectedView, expectedEvidence, err := updated.Inputs(ctx)
			if err != nil {
				test.Error(err)
				return
			}
			actualRepo, actualView, actualEvidence, err := updated.prepareInputs(ctx)
			if err != nil || !reflect.DeepEqual(actualRepo, expectedRepo) || !reflect.DeepEqual(actualView, expectedView) || !reflect.DeepEqual(actualEvidence, expectedEvidence) {
				test.Error("delta disagrees with full finalization", err)
			}
		})
	}
	group.Wait()
	metadata, view, evidence, err = base.Inputs(ctx)
	if err != nil || base.decoded != decoder || !reflect.DeepEqual(metadata, wantRepo) || !reflect.DeepEqual(view, wantView) || !reflect.DeepEqual(evidence, wantEvidence) {
		test.Fatal("sibling preparation or caller mutated base", err)
	}
	broken := base
	broken.entries = append([]domain.RepositoryViewSourceEntry{}, base.entries...)
	broken.entries[1].Value = []byte(`{"context_protocol":null}`)
	if _, _, _, err := broken.prepareInputs(ctx); err == nil || broken.decoded != decoder || base.decoded != decoder {
		test.Fatal("failed preparation replaced retained decoder", err)
	}
	if _, _, _, err := base.prepareInputs(ctx); err != nil {
		test.Fatal("failed sibling poisoned retry", err)
	}
}
