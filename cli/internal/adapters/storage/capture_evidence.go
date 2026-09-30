package storage

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// CaptureEvidenceReader exposes only passive capture-recovery evidence reads.
// It never acquires mutation locks or recovers pending transactions. Concurrent
// writers may change evidence between reads; this is not an atomic replica view.
type CaptureEvidenceReader struct {
	store *FileStore
}

func NewCaptureEvidenceReader(root string) *CaptureEvidenceReader {
	return &CaptureEvidenceReader{store: NewFileStore(root)}
}

func (r *CaptureEvidenceReader) GetSnapshot(ctx context.Context, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	snapshot, err := r.store.GetSnapshot(ctx, hash)
	if canceled := ctx.Err(); canceled != nil {
		return domain.Snapshot{}, canceled
	}
	return snapshot, err
}

func (r *CaptureEvidenceReader) ListHistoryEvents(ctx context.Context, repo string) ([]domain.HistoryEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Keep typed validation, current position selections, duplicate detection
	// and dependency ordering identical to the ordinary evidence reader, without
	// its mutation lock and journal recovery side effects.
	events, err := r.store.listHistoryEventsAndPositions(repo)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	return events, err
}

var _ outbound.CaptureRecoveryEvidence = (*CaptureEvidenceReader)(nil)
