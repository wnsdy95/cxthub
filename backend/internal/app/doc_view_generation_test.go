package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewGenerationFailure struct {
	*docViewSnapshotStore
	generationOpens int
}

func (storage *docViewGenerationFailure) OpenDocRangeGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.generationOpens++
	return nil, storage.openErr
}

func TestDocViewGenerationFailureNeverFallsBackToWholeMetadata(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrDocRangeIndexUnavailable, context.Canceled} {
			storage := &docViewGenerationFailure{docViewSnapshotStore: &docViewSnapshotStore{readonly: readonly, openErr: failure}}
			_, err := openDocViewRange(context.Background(), storage, h('1'), h('2'))
			if readonly {
				if !errors.Is(err, failure) || storage.generationOpens != 1 {
					t.Fatal("generation failure changed", err, storage.generationOpens)
				}
			} else if !errors.Is(err, domain.ErrConflict) || storage.generationOpens != 0 {
				t.Fatal("unpinned generation opened", err, storage.generationOpens)
			}
			if storage.opens != 0 {
				t.Fatal("generation failure silently fell back")
			}
		}
	}
}

func TestDocViewInheritedReadsLazyPageHashesAndPropagatesFailures(t *testing.T) {
	ctx := context.Background()
	calls := 0
	block := func(page int) (domain.ContentHash, error) {
		calls++
		if page < 0 || page > 1 {
			t.Fatal("unrelated page was read", page)
		}
		return h('1'), nil
	}
	source := docViewSource{total: 200, block: block, location: func(int) (domain.DocEventIndex, error) {
		t.Fatal("equal authenticated pages read individual entries")
		return domain.DocEventIndex{}, nil
	}}
	if inherited, err := docViewInherited(ctx, source, source); err != nil || inherited != 200 || calls != 4 {
		t.Fatal(inherited, calls, err)
	}
	broken := source
	broken.block = func(int) (domain.ContentHash, error) { return "", domain.ErrIntegrity }
	for _, pair := range [][2]docViewSource{{broken, source}, {source, broken}} {
		if _, err := docViewInherited(ctx, pair[0], pair[1]); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("directory failure was hidden", err)
		}
	}
}
