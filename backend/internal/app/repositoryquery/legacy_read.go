package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *LegacyGraphQueries) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	return readLegacyGraphResult(ctx, queries, repo, func(bound context.Context) (domain.RepositoryView, error) {
		view, err := queries.LoadRepositoryView(bound, repo)
		if err != nil {
			return domain.RepositoryView{}, err
		}
		graph, err := queries.projection.Cached(bound, repo, view, "")
		if err != nil {
			return domain.RepositoryView{}, err
		}
		view.Graph = &graph
		return view, bound.Err()
	})
}

func (queries *LegacyGraphQueries) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	return readLegacyGraphResult(ctx, queries, repo, func(bound context.Context) (domain.PendingView, error) {
		view, err := queries.LoadRepositoryView(bound, repo)
		if err != nil {
			return domain.PendingView{}, err
		}
		graph, err := queries.projection.Cached(bound, repo, view, "")
		if err != nil {
			return domain.PendingView{}, err
		}
		view.Graph = &graph
		return domain.ProjectPendingView(view)
	})
}

func (queries *LegacyGraphQueries) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	return readLegacyGraphResult(ctx, queries, repo, func(bound context.Context) (domain.GraphState, error) {
		view, err := queries.LoadRepositoryView(bound, repo)
		if err != nil {
			return domain.GraphState{}, err
		}
		return queries.projection.Cached(bound, repo, view, position)
	})
}

func readLegacyGraphResult[Result any](ctx context.Context, queries *LegacyGraphQueries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	if err := domain.ValidateContentHash(repo); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	err := queries.scope.ReadGraph(ctx, repo, func(bound context.Context) error {
		if err := bound.Err(); err != nil {
			return err
		}
		var err error
		result, err = read(bound)
		if err != nil {
			return err
		}
		return bound.Err()
	})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		var empty Result
		return empty, err
	}
	return result, nil
}

type RepositoryViewReaders struct {
	Metadata  MetadataReader
	History   HistoryReader
	Revisions RevisionReader
	Archives  ArchiveReader
}

func (queries *LegacyGraphQueries) LoadRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	return LoadRepositoryView(ctx, repo, RepositoryViewReaders{
		Metadata: queries.metadata, History: queries.history, Revisions: queries.revisions, Archives: queries.archives,
	})
}

func LoadRepositoryView(ctx context.Context, repo domain.ContentHash, readers RepositoryViewReaders) (domain.RepositoryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryView{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.RepositoryView{}, err
	}
	if absent(readers.Metadata) {
		return domain.RepositoryView{}, fmt.Errorf("%w: repository view metadata is required", domain.ErrCatalogUnsupported)
	}
	for _, optional := range []any{readers.History, readers.Revisions, readers.Archives} {
		if optional != nil && absent(optional) {
			return domain.RepositoryView{}, fmt.Errorf("%w: declared repository view reader is nil", domain.ErrCatalogUnsupported)
		}
	}
	var view domain.RepositoryView
	metadata, err := readers.Metadata.GetRepo(ctx, repo)
	if err == nil {
		if metadata.ID != repo {
			return domain.RepositoryView{}, domain.ErrIntegrity
		}
		view.DefaultBranch = metadata.DefaultBranch
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.RepositoryView{}, err
	}
	if view.Refs, err = readers.Metadata.ListRefs(ctx, repo); err != nil {
		return domain.RepositoryView{}, err
	}
	if view.Snapshots, err = readers.Metadata.ListSnapshots(ctx, repo, ""); err != nil {
		return domain.RepositoryView{}, err
	}
	if view.Reflog, err = readers.Metadata.ReadReflog(ctx, repo); err != nil {
		return domain.RepositoryView{}, err
	}
	if readers.History == nil {
		return domain.RepositoryView{}, errors.New("context history storage unavailable")
	}
	if view.History, err = readers.History.ListHistoryEvents(ctx, repo); err != nil {
		return domain.RepositoryView{}, err
	}
	if view.Pending, err = readers.Metadata.ListPendings(ctx, repo); err != nil {
		return domain.RepositoryView{}, err
	}
	if view.Unsync, err = readers.Metadata.ListUnsyncs(ctx, repo); err != nil {
		return domain.RepositoryView{}, err
	}
	var archives []domain.SessionArchive
	if readers.Archives != nil {
		if archives, err = readers.Archives.ListSessionArchives(ctx, repo); err != nil {
			return domain.RepositoryView{}, err
		}
	}
	for _, archive := range archives {
		if err := ctx.Err(); err != nil {
			return domain.RepositoryView{}, err
		}
		if archive.RepoID != repo {
			return domain.RepositoryView{}, domain.ErrIntegrity
		}
	}
	if readers.Revisions != nil {
		if view.Revision, err = readers.Revisions.RepositoryRevision(ctx, repo); err != nil {
			return domain.RepositoryView{}, err
		}
	}
	if err := domain.ValidateRepositoryViewScope(ctx, repo, view); err != nil {
		return domain.RepositoryView{}, err
	}
	memberships, err := domain.ProjectSnapshotBranchMemberships(ctx, view.Snapshots, view.Reflog)
	if err != nil {
		return domain.RepositoryView{}, err
	}
	view.Snapshots = append([]domain.Snapshot{}, view.Snapshots...)
	byID := make(map[domain.ContentHash]domain.Snapshot, len(view.Snapshots))
	for index := range view.Snapshots {
		if err := ctx.Err(); err != nil {
			return domain.RepositoryView{}, err
		}
		snapshot := &view.Snapshots[index]
		snapshot.Branches = nil
		for branch := range memberships[snapshot.ID] {
			snapshot.Branches = append(snapshot.Branches, branch)
		}
		sort.Strings(snapshot.Branches)
		byID[snapshot.ID] = *snapshot
	}
	view.Pending = append([]domain.Pending{}, view.Pending...)
	for index := range view.Pending {
		if err := ctx.Err(); err != nil {
			return domain.RepositoryView{}, err
		}
		if snapshot, found := byID[view.Pending[index].Target]; found && !snapshot.CreatedAt.IsZero() {
			view.Pending[index].UpdatedAt = snapshot.CreatedAt
		}
	}
	view.ArchivedSessions = domain.ProjectSessionArchives(archives, view.Snapshots, view.History, view.DefaultBranch)
	view.Semantics = domain.ProjectContextSemantics(view.Snapshots, view.History)
	view.Refs = legacyGraphNonNil(view.Refs)
	view.Reflog = legacyGraphNonNil(view.Reflog)
	view.History = legacyGraphNonNil(view.History)
	view.Unsync = legacyGraphNonNil(view.Unsync)
	if err := ctx.Err(); err != nil {
		return domain.RepositoryView{}, err
	}
	return view, nil
}

func legacyGraphNonNil[Item any](items []Item) []Item {
	if items == nil {
		return []Item{}
	}
	return items
}
