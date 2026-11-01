//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type visibleReviewGenerationPorts interface {
	outbound.BlobStore
	outbound.DocRangeGenerationStore
	outbound.DocVisibleGenerationStore
	outbound.DocVisiblePreparationStore
	outbound.DocJobStore
	outbound.StoredDocVerifier
	outbound.VerifiedDocReader
}

type visibleReviewWithoutRanges struct {
	visibleReviewGenerationPorts
	outbound.ConversationChunkReader
	outbound.DocRangeBoundaryStore
}

func (storage *visibleReviewWithoutRanges) InReadOnlyTransaction(ctx context.Context) bool {
	return storage.visibleReviewGenerationPorts.InReadOnlyTransaction(ctx)
}

type visibleReviewWithoutChunks struct {
	visibleReviewGenerationPorts
	outbound.DocRangeStore
	outbound.DocRangeBoundaryStore
}

func (storage *visibleReviewWithoutChunks) InReadOnlyTransaction(ctx context.Context) bool {
	return storage.visibleReviewGenerationPorts.InReadOnlyTransaction(ctx)
}

type visibleReviewWithoutBoundaries struct {
	visibleReviewGenerationPorts
	outbound.DocRangeStore
	outbound.ConversationChunkReader
	hashReads int
}

type visibleReviewRangeRead struct {
	outbound.DocRangeGenerationRead
	storage *visibleReviewWithoutBoundaries
}

func (storage *visibleReviewWithoutBoundaries) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.visibleReviewGenerationPorts.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &visibleReviewRangeRead{DocRangeGenerationRead: read, storage: storage}, nil
}

func (read *visibleReviewRangeRead) PageHash(ctx context.Context, position int) (domain.ContentHash, error) {
	read.storage.hashReads++
	return read.DocRangeGenerationRead.PageHash(ctx, position)
}

func TestPGDocVisibleReviewRejectsPartialCapabilities(test *testing.T) {
	for _, capability := range []string{"range reader", "chunk reader", "boundary reader"} {
		test.Run(capability, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, fixture.hash); err != nil {
				test.Fatal(err)
			}
			request := domain.DocVisiblePageRequest{View: "chat", Offset: -1, Limit: 1}
			withoutBoundaries := &visibleReviewWithoutBoundaries{
				visibleReviewGenerationPorts: fixture.storage,
				DocRangeStore:                fixture.storage,
				ConversationChunkReader:      fixture.storage,
			}
			switch capability {
			case "range reader":
				fixture.service.blobs = &visibleReviewWithoutRanges{
					visibleReviewGenerationPorts: fixture.storage,
					ConversationChunkReader:      fixture.storage,
					DocRangeBoundaryStore:        fixture.storage,
				}
			case "chunk reader":
				fixture.service.blobs = &visibleReviewWithoutChunks{
					visibleReviewGenerationPorts: fixture.storage,
					DocRangeStore:                fixture.storage,
					DocRangeBoundaryStore:        fixture.storage,
				}
			case "boundary reader":
				fixture.service.blobs = withoutBoundaries
				request.Base = fixture.hash
			}
			page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, fixture.hash, request)
			if !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) || page.Hash != "" || withoutBoundaries.hashReads != 0 {
				test.Fatalf("missing %s: error=%v hash=%s inherited=%d prefix_hash_reads=%d; want unsupported without prefix traversal", capability, err, page.Hash, page.Inherited, withoutBoundaries.hashReads)
			}
		})
	}
}

func TestPGDocVisibleReviewBoundsEmptyPageEnvelope(test *testing.T) {
	for _, scenario := range []string{"empty document", "end of document"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
			document.Envelope.Cwd = strings.Repeat("x", docViewBytes+4096)
			if scenario == "end of document" {
				document.Events = []domain.CIREvent{historyMessage(domain.RoleUser, "small event")}
			}
			hash := seedDocViewBoundaryPG(test, fixture, document)
			page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: "chat", Offset: len(document.Events)})
			if errors.Is(err, domain.ErrDocumentResourceLimit) {
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			raw, err := json.Marshal(page)
			if err != nil {
				test.Fatal(err)
			}
			if len(raw) > docViewBytes {
				test.Fatalf("empty page bypassed wire budget: bytes=%d limit=%d", len(raw), docViewBytes)
			}
		})
	}
}

func TestPGDocVisibleReviewRejectsReinsertedSourceCursor(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document := visiblePGDocument(fixture, 2, 3)
	hash := seedDocViewBoundaryPG(test, fixture, document)
	page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 0, Limit: 1})
	if err != nil || page.NextCursor == nil {
		test.Fatal("initial cursor", page.NextCursor, err)
	}
	var before int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, hash).Scan(&before); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, hash); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, hash); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM blobs WHERE hash=$1`, hash); err != nil {
		test.Fatal(err)
	}
	if reinserted := seedDocViewBoundaryPG(test, fixture, document); reinserted != hash {
		test.Fatal("reinsert changed content identity", reinserted, hash)
	}
	var after int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, hash).Scan(&after); err != nil {
		test.Fatal(err)
	}
	continued, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: *page.NextCursor})
	if !errors.Is(err, domain.ErrDocVisibleGenerationStale) || continued.Hash != "" {
		test.Fatalf("deleted/reinserted source reused cursor: version=%d->%d same_generation=%v from=%d error=%v", before, after, page.Generation == continued.Generation, continued.From, err)
	}
}
