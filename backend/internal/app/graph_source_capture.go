package app

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type CapturedGraphSource struct {
	checkpoint domain.RepositoryViewSourceCheckpoint
	entries    []domain.RepositoryViewSourceEntry
	decoded    *domain.RepositoryViewSourceDecoder
}

func (source CapturedGraphSource) Checkpoint() domain.RepositoryViewSourceCheckpoint {
	return source.checkpoint
}

func (source CapturedGraphSource) Inputs(ctx context.Context) (domain.Repo, domain.RepositoryView, []domain.RepositoryViewSourceEntry, error) {
	return repositoryquery.DecodeCapturedGraphInputs(ctx, source.checkpoint, source.entries)
}

func (source *CapturedGraphSource) prepareInputs(ctx context.Context) (domain.Repo, domain.RepositoryView, []domain.RepositoryViewSourceEntry, error) {
	metadata, evidence, err := source.inputEntries(ctx)
	if err != nil {
		return domain.Repo{}, domain.RepositoryView{}, nil, err
	}
	repo, view, decoded, err := domain.DecodeRepositoryViewSourceReusing(ctx, source.checkpoint.RepoID, metadata, source.decoded)
	if err != nil {
		return domain.Repo{}, domain.RepositoryView{}, nil, err
	}
	source.decoded = decoded
	return repo, view, evidence, nil
}

func (source CapturedGraphSource) inputEntries(ctx context.Context) ([]domain.RepositoryViewSourceEntry, []domain.RepositoryViewSourceEntry, error) {
	return repositoryquery.SplitCapturedGraphInputs(ctx, source.checkpoint, source.entries)
}

func CaptureGraphSource(ctx context.Context, storage outbound.RepositoryGraphSourceStore, repo domain.ContentHash, previous *CapturedGraphSource) (CapturedGraphSource, error) {
	return captureGraphSource(ctx, storage, repo, previous, nil)
}

func captureGraphSource(ctx context.Context, storage outbound.RepositoryGraphSourceStore, repo domain.ContentHash, previous *CapturedGraphSource, delta *memoryGraphDelta) (CapturedGraphSource, error) {
	if err := ctx.Err(); err != nil {
		return CapturedGraphSource{}, err
	}
	if domain.ValidateContentHash(repo) != nil || storage == nil {
		return CapturedGraphSource{}, domain.ErrValidation
	}
	request := domain.RepositoryViewSourceRequest{Version: domain.RepositoryViewSourceVersion}
	mode := "baseline"
	after := int64(-1)
	var images map[[2]string]domain.RepositoryViewSourceEntry
	retainedBytes := 0
	if previous != nil {
		checkpoint := previous.checkpoint
		if checkpoint.Validate() != nil || checkpoint.Scope != domain.RepositoryGraphSourceScope || checkpoint.RepoID != repo {
			return CapturedGraphSource{}, domain.ErrValidation
		}
		request.After, mode, after = &checkpoint, "delta", checkpoint.Sequence
	}
	var checkpoint domain.RepositoryViewSourceCheckpoint
	var last domain.RepositoryViewSourceEntry
	visitedCursors := map[string]bool{}
	readBytes, readRows := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return CapturedGraphSource{}, err
		}
		page, err := storage.RepositoryGraphSourceChanges(ctx, repo, request)
		if err != nil {
			return CapturedGraphSource{}, err
		}
		current := domain.RepositoryViewSourceCheckpoint{Version: page.Version, Scope: page.Scope, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}
		if current.Validate() != nil || current.Scope != domain.RepositoryGraphSourceScope || current.RepoID != repo || page.Mode != mode || current.Sequence < after ||
			(previous != nil && current.Epoch != previous.checkpoint.Epoch) || (checkpoint.Epoch != "" && checkpoint != current) {
			return CapturedGraphSource{}, domain.ErrIntegrity
		}
		checkpoint = current
		if len(page.Entries) > domain.MaxRepositoryViewSourceLimit || len(page.NextCursor) > 16<<10 {
			return CapturedGraphSource{}, domain.ErrRepositoryViewSourceLimit
		}
		if page.NextCursor != "" {
			if page.Checkpoint != nil || len(page.Entries) == 0 || visitedCursors[page.NextCursor] {
				return CapturedGraphSource{}, domain.ErrIntegrity
			}
			visitedCursors[page.NextCursor] = true
			readBytes += len(page.NextCursor)
			if readBytes > domain.MaxPendingProjectionInputBytes {
				return CapturedGraphSource{}, domain.ErrRepositoryViewSourceLimit
			}
		} else if page.Checkpoint == nil || *page.Checkpoint != checkpoint {
			return CapturedGraphSource{}, domain.ErrIntegrity
		}
		if len(page.Entries) > 0 && images == nil {
			images = make(map[[2]string]domain.RepositoryViewSourceEntry)
			if previous != nil {
				for _, entry := range previous.entries {
					if err := ctx.Err(); err != nil {
						return CapturedGraphSource{}, err
					}
					images[[2]string{entry.Kind, entry.Key}] = entry
					retainedBytes += graphSourceEntryBytes(entry)
				}
			}
		}
		pageBytes := 0
		for _, entry := range page.Entries {
			if err := ctx.Err(); err != nil {
				return CapturedGraphSource{}, err
			}
			cost := graphSourceEntryBytes(entry)
			readBytes, readRows = readBytes+cost, readRows+1
			pageBytes += len(entry.Key) + len(entry.Value) + 128
			if len(entry.Key) > domain.MaxRepositoryViewSourceKeyBytes || len(entry.Value) > domain.MaxRepositoryViewSourceImageBytes ||
				pageBytes > domain.MaxRepositoryViewSourcePageBytes || readBytes > domain.MaxPendingProjectionInputBytes || readRows > domain.MaxPendingProjectionInputRows {
				return CapturedGraphSource{}, domain.ErrRepositoryViewSourceLimit
			}
			ordered := entry.Kind > last.Kind || (entry.Kind == last.Kind && entry.Key > last.Key)
			if mode == "delta" {
				ordered = entry.Sequence > last.Sequence || (entry.Sequence == last.Sequence && ordered)
			}
			trimmed := bytes.TrimSpace(entry.Value)
			if !ordered || !domain.RepositoryGraphSourceKindValid(entry.Kind) || entry.Key == "" || entry.Sequence < 0 || entry.Sequence > checkpoint.Sequence ||
				(mode == "delta" && entry.Sequence <= after) || (mode == "baseline" && entry.Deleted) ||
				(entry.Deleted && len(entry.Value) != 0) || (!entry.Deleted && (len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed))) {
				return CapturedGraphSource{}, domain.ErrIntegrity
			}
			key := [2]string{entry.Kind, entry.Key}
			if old, exists := images[key]; exists {
				retainedBytes -= graphSourceEntryBytes(old)
			}
			if entry.Deleted {
				delete(images, key)
			} else {
				retainedBytes += cost
				if retainedBytes > domain.MaxPendingProjectionInputBytes || (len(images) == domain.MaxPendingProjectionInputRows && images[key].Kind == "") {
					return CapturedGraphSource{}, domain.ErrRepositoryViewSourceLimit
				}
				entry.Value = bytes.Clone(entry.Value)
				images[key] = entry
			}
			if delta != nil {
				delta.record(entry)
			}
			last = entry
		}
		if page.NextCursor == "" {
			break
		}
		request = domain.RepositoryViewSourceRequest{Version: domain.RepositoryViewSourceVersion, Cursor: page.NextCursor}
	}
	if err := ctx.Err(); err != nil {
		return CapturedGraphSource{}, err
	}
	if previous != nil && images == nil {
		return CapturedGraphSource{checkpoint: checkpoint, entries: previous.entries, decoded: previous.decoded}, nil
	}
	entries := make([]domain.RepositoryViewSourceEntry, 0, len(images))
	for _, entry := range images {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Kind != entries[right].Kind {
			return entries[left].Kind < entries[right].Kind
		}
		return entries[left].Key < entries[right].Key
	})
	if err := ctx.Err(); err != nil {
		return CapturedGraphSource{}, err
	}
	result := CapturedGraphSource{checkpoint: checkpoint, entries: entries}
	if previous != nil {
		result.decoded = previous.decoded
	}
	return result, nil
}

func graphSourceEntryBytes(entry domain.RepositoryViewSourceEntry) int {
	return len(entry.Kind) + len(entry.Key) + len(entry.Value) + 128
}
