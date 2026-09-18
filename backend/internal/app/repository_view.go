package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// GetRepositoryView pins every graph input to the same PostgreSQL MVCC snapshot.
// A failed input fails the whole view; clients keep the last complete generation.
func (s *Service) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryView{}, err
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (v domain.RepositoryView, err error) {
		if v.Refs, err = s.ListRefs(ctx, repo); err != nil {
			return
		}
		if v.Snapshots, err = s.List(ctx, inbound.ListSnapshotsInput{RepoID: repo}); err != nil {
			return
		}
		if v.Reflog, err = s.Reflog(ctx, repo); err != nil {
			return
		}
		if v.History, err = s.ListHistory(ctx, repo); err != nil {
			return
		}
		if v.Pending, err = s.meta.ListPendings(ctx, repo); err != nil {
			return
		}
		if v.Unsync, err = s.ListUnsyncs(ctx, repo); err != nil {
			return
		}
		byID := make(map[domain.ContentHash]domain.Snapshot, len(v.Snapshots))
		for _, snap := range v.Snapshots {
			byID[snap.ID] = snap
		}
		for i := range v.Pending {
			if snap, ok := byID[v.Pending[i].Target]; ok && !snap.CreatedAt.IsZero() {
				v.Pending[i].UpdatedAt = snap.CreatedAt
			}
		}
		if v.Revision, err = s.RepositoryRevision(ctx, repo); err != nil {
			return
		}
		v.Semantics = domain.ProjectContextSemantics(v.Snapshots, v.History)
		v.Refs = nonNil(v.Refs)
		v.Snapshots = nonNil(v.Snapshots)
		v.Reflog = nonNil(v.Reflog)
		v.History = nonNil(v.History)
		v.Pending = nonNil(v.Pending)
		v.Unsync = nonNil(v.Unsync)
		return
	})
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func (s *Service) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryRevision{}, err
	}
	if store, ok := s.meta.(outbound.RepositoryRevisions); ok {
		return store.RepositoryRevision(ctx, repo)
	}
	return domain.RepositoryRevision{}, nil
}
func (s *Service) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PendingView{}, err
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (v domain.PendingView, err error) {
		if v.Revision, err = s.RepositoryRevision(ctx, repo); err != nil {
			return
		}
		if v.Pending, err = s.meta.ListPendings(ctx, repo); err != nil {
			return
		}

		// The initial graph view already contains historical ancestors. Do not
		// walk them again for old captures behind current ref tips. A client
		// seeing a new target with unknown parents refreshes its complete view.
		seen := map[domain.ContentHash]domain.Snapshot{}
		for _, p := range v.Pending {
			id := p.Target
			if _, ok := seen[id]; ok {
				continue
			}
			snap, e := s.meta.GetSnapshot(ctx, repo, id)
			if e != nil {
				return v, e
			}
			// Capture patches never own projected memberships, including values
			// persisted by legacy FS versions. The full graph generation does.
			snap.Branches = nil
			seen[id] = snap
			v.Snapshots = append(v.Snapshots, snap)
		}
		for i, p := range v.Pending {
			v.Pending[i].UpdatedAt = seen[p.Target].CreatedAt
		}

		v.Pending = nonNil(v.Pending)
		v.Snapshots = nonNil(v.Snapshots)
		return
	})
}
