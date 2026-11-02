//go:build postgres

package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type visibleCostStore struct {
	*docViewPGStore
	unitReads, chunkReads, chunkBytes int
}

type visibleCostRead struct {
	outbound.DocVisibleGenerationRead
	store *visibleCostStore
}

func (storage *visibleCostStore) OpenDocVisibleGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocVisibleGenerationRead, error) {
	read, err := storage.PostgresStore.OpenDocVisibleGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &visibleCostRead{DocVisibleGenerationRead: read, store: storage}, nil
}

func (read *visibleCostRead) Unit(ctx context.Context, mode string, position int, byEvent bool) (domain.DocVisibleUnit, int, error) {
	read.store.unitReads++
	return read.DocVisibleGenerationRead.Unit(ctx, mode, position, byEvent)
}

func (storage *visibleCostStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	storage.chunkBytes += int(length)
	return storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}

func TestPGDocVisiblePageCostDoesNotScanHiddenHistory(t *testing.T) {
	for _, hidden := range []int{32, 320} {
		t.Run(fmt.Sprint(hidden), func(t *testing.T) {
			fixture := newDocViewPGFixture(t)
			document := visiblePGDocument(fixture, hidden, 32)
			hash := seedDocViewBoundaryPG(t, fixture, document)
			for _, mode := range []string{"chat", "prompts"} {
				observed := &visibleCostStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}}
				fixture.service.blobs = observed
				page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: mode, Offset: 0, Limit: 20})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Units) != 20 || page.NextCursor == nil || observed.unitReads != 21 || observed.pages > 20 || observed.fallbackPages != 0 || observed.chunkReads > 20 || observed.chunkBytes > 20*domain.ConversationManifestChunkBytes {
					t.Fatal("page work grew with hidden history", len(page.Units), observed, err)
				}
				if hidden == 320 && page.To <= docViewScanEvents {
					t.Fatal("legacy scan bound still truncates visible page", page.To)
				}
				t.Logf("events=%d mode=%s covered=%d units=%d unit_lookups=%d range_pages=%d chunks=%d chunk_bytes=%d", len(document.Events), mode, page.To, len(page.Units), observed.unitReads, observed.pages, observed.chunkReads, observed.chunkBytes)
			}
		})
	}
}
