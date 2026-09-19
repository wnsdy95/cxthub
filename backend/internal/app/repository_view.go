package app

import (
	"context"
	"errors"

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
	v, err := repositoryRead(ctx, s, func(ctx context.Context) (domain.RepositoryView, error) {
		v, err := s.loadRepositoryView(ctx, repo)
		if err != nil {
			return v, err
		}
		graph, err := domain.ProjectGraphState(v, v.DefaultBranch, "")
		if err != nil {
			return domain.RepositoryView{}, err
		}
		v.Graph = &graph
		return v, nil
	})
	return v, err
}

// Called only inside the caller's pinned read. No native documents are loaded.
func (s *Service) loadRepositoryView(ctx context.Context, repo domain.ContentHash) (v domain.RepositoryView, err error) {
	if r, e := s.meta.GetRepo(ctx, repo); e == nil {
		v.DefaultBranch = r.DefaultBranch
	} else if !errors.Is(e, domain.ErrNotFound) {
		return v, e
	}

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
		// Both endpoints classify the same coherent metadata generation. Only the
		// pending target metadata is retransmitted; no documents are loaded.
		base, err := s.loadRepositoryView(ctx, repo)
		if err != nil {
			return v, err
		}
		graph, err := domain.ProjectGraphState(base, base.DefaultBranch, "")
		if err != nil {
			return v, err
		}
		v.Revision = base.Revision
		v.Pending = base.Pending
		v.Graph = &graph
		byID := make(map[domain.ContentHash]domain.Snapshot, len(base.Snapshots))
		for _, snap := range base.Snapshots {
			byID[snap.ID] = snap
		}
		seen := map[domain.ContentHash]bool{}
		for _, p := range v.Pending {
			if seen[p.Target] {
				continue
			}
			snap, ok := byID[p.Target]
			if !ok {
				return v, domain.ErrNotFound
			}
			// Capture patches never own projected branch memberships.
			snap.Branches = nil
			v.Snapshots = append(v.Snapshots, snap)
			seen[p.Target] = true
		}
		v.Snapshots = nonNil(v.Snapshots)
		return v, nil
	})
}
