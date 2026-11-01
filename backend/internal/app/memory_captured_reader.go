package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.MemoryQueryReader = (*capturedMemoryReader)(nil)

type capturedMemoryReader struct {
	repo         domain.ContentHash
	view         domain.RepositoryView
	prPins       []domain.PRContributionMemory
	gitPins      []domain.GitContributionMemory
	pinsCaptured bool
	objects      map[domain.ContentHash]json.RawMessage
}

func (reader *capturedMemoryReader) check(ctx context.Context, repo domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || repo != reader.repo {
		return domain.ErrIntegrity
	}
	return nil
}

func cloneMemoryInput[Value any](ctx context.Context, value Value) (Value, error) {
	var result Value
	if err := ctx.Err(); err != nil {
		return result, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

func (reader *capturedMemoryReader) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if err := reader.check(ctx, repo); err != nil {
		return nil, err
	}
	var rows []domain.Snapshot
	if branch == "" && reader.view.Snapshots != nil {
		rows = make([]domain.Snapshot, 0, len(reader.view.Snapshots))
	}
	for _, snapshot := range reader.view.Snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if branch == "" || snapshot.Branch == branch {
			rows = append(rows, cloneCapturedSnapshot(snapshot))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func cloneCapturedSnapshot(snapshot domain.Snapshot) domain.Snapshot {
	snapshot = snapshot.Clone()
	if len(snapshot.Branches) == 0 {
		snapshot.Branches = nil
	}
	if len(snapshot.GraftParents) == 0 {
		snapshot.GraftParents = nil
	}
	if len(snapshot.Models) == 0 {
		snapshot.Models = nil
	}
	return snapshot
}

func (reader *capturedMemoryReader) GetSnapshot(ctx context.Context, repo, snapshot domain.ContentHash) (domain.Snapshot, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.Snapshot{}, err
	}
	for _, row := range reader.view.Snapshots {
		if err := ctx.Err(); err != nil {
			return domain.Snapshot{}, err
		}
		if row.ID == snapshot {
			cloned := cloneCapturedSnapshot(row)
			if err := ctx.Err(); err != nil {
				return domain.Snapshot{}, err
			}
			return cloned, nil
		}
	}
	return domain.Snapshot{}, domain.ErrNotFound
}

func (reader *capturedMemoryReader) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.Ref{}, err
	}
	for _, ref := range reader.view.Refs {
		if err := ctx.Err(); err != nil {
			return domain.Ref{}, err
		}
		if ref.Kind == kind && ref.Name == name {
			return ref, nil
		}
	}
	return domain.Ref{}, domain.ErrNotFound
}

func (reader *capturedMemoryReader) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if err := reader.check(ctx, repo); err != nil {
		return nil, err
	}
	var rows []domain.HistoryEvent
	if reader.view.History != nil {
		rows = make([]domain.HistoryEvent, len(reader.view.History))
	}
	for index, event := range reader.view.History {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows[index] = event.Clone()
		if rows[index].Creation != nil && len(rows[index].Creation.Command) == 0 {
			rows[index].Creation.Command = nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func (reader *capturedMemoryReader) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.RepositoryRevision{}, err
	}
	return reader.view.Revision, nil
}

func (reader *capturedMemoryReader) ListPRContributionMemories(ctx context.Context, repo domain.ContentHash) ([]domain.PRContributionMemory, error) {
	if err := reader.check(ctx, repo); err != nil {
		return nil, err
	}
	if !reader.pinsCaptured {
		return nil, domain.ErrIntegrity
	}
	return cloneMemoryInput(ctx, reader.prPins)
}

func (reader *capturedMemoryReader) ListGitContributionMemories(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionMemory, error) {
	if err := reader.check(ctx, repo); err != nil {
		return nil, err
	}
	if !reader.pinsCaptured {
		return nil, domain.ErrIntegrity
	}
	return cloneMemoryInput(ctx, reader.gitPins)
}

func (reader *capturedMemoryReader) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.MemoryDigest{}, err
	}
	raw, captured := reader.objects[hash]
	if !captured {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if raw == nil {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	var digest domain.MemoryDigest
	if err := json.Unmarshal(raw, &digest); err != nil {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	actual, err := domain.MemoryDigestHashBounded(digest, maxProjectionMemoryBytes)
	if err != nil || actual != hash {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	return digest, ctx.Err()
}

type recordingMemoryReader struct {
	*capturedMemoryReader
	read  func(context.Context, domain.ContentHash, domain.ContentHash, int) (domain.MemoryDigest, error)
	bytes int
}

func (reader *recordingMemoryReader) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.MemoryDigest{}, err
	}
	if domain.ValidateContentHash(hash) != nil {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if _, captured := reader.objects[hash]; captured {
		return reader.capturedMemoryReader.GetMemory(ctx, repo, hash)
	}
	if len(reader.objects) >= maxProjectionSnapshots || reader.bytes >= maxProjectionMemoryBytes {
		return domain.MemoryDigest{}, domain.ErrMemoryProjectionLimit
	}
	digest, err := reader.read(ctx, repo, hash, maxProjectionMemoryBytes-reader.bytes)
	if canceled := ctx.Err(); canceled != nil {
		return domain.MemoryDigest{}, canceled
	}
	if errors.Is(err, domain.ErrNotFound) {
		reader.objects[hash] = nil
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	actual, err := domain.MemoryDigestHashBounded(digest, maxProjectionMemoryBytes-reader.bytes)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if actual != hash {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	raw, err := json.Marshal(digest)
	if err != nil {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	if len(raw) > maxProjectionMemoryBytes-reader.bytes {
		return domain.MemoryDigest{}, domain.ErrMemoryProjectionLimit
	}
	reader.bytes += len(raw)
	reader.objects[hash] = raw
	return reader.capturedMemoryReader.GetMemory(ctx, repo, hash)
}
