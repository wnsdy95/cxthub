package archivequery

import (
	"context"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (queries *ArchiveQueries) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadRepositories(ctx, read)
	}, func(bound context.Context) ([]domain.Repo, error) {
		repositories, err := queries.metadata.ListRepos(bound, team)
		if err != nil {
			return nil, err
		}
		for _, repository := range repositories {
			if err := repository.RequiredDocIdentity.Validate(); err != nil {
				return nil, err
			}
		}
		return repositories, nil
	})
}

func (queries *ArchiveQueries) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.Repo{}, err
	}
	return queries.metadata.GetRepo(ctx, repo)
}

func (queries *ArchiveQueries) List(ctx context.Context, input inbound.ListSnapshotsInput) ([]domain.Snapshot, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadRepository(ctx, input.RepoID, read)
	}, func(bound context.Context) ([]domain.Snapshot, error) {
		if err := domain.ValidateContentHash(input.RepoID); err != nil {
			return nil, err
		}
		snapshots, err := queries.metadata.ListSnapshots(bound, input.RepoID, "")
		if err != nil {
			return nil, err
		}
		logs, err := queries.metadata.ReadReflog(bound, input.RepoID)
		if err != nil {
			return nil, err
		}
		members, err := domain.ProjectSnapshotBranchMemberships(bound, snapshots, logs)
		if err != nil {
			return nil, err
		}
		output := make([]domain.Snapshot, 0, len(snapshots))
		for _, snapshot := range snapshots {
			snapshot.Branches = nil
			for branch := range members[snapshot.ID] {
				snapshot.Branches = append(snapshot.Branches, branch)
			}
			sort.Strings(snapshot.Branches)
			if input.Branch == "" || members[snapshot.ID][input.Branch] {
				output = append(output, snapshot)
			}
		}
		return output, nil
	})
}

func (queries *ArchiveQueries) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadRepository(ctx, repo, read)
	}, func(bound context.Context) ([]domain.Ref, error) {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
		return queries.metadata.ListRefs(bound, repo)
	})
}

func (queries *ArchiveQueries) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadRepository(ctx, repo, read)
	}, func(bound context.Context) (domain.Snapshot, error) {
		for _, identifier := range []domain.ContentHash{repo, hash} {
			if err := domain.ValidateContentHash(identifier); err != nil {
				return domain.Snapshot{}, err
			}
		}
		return queries.metadata.GetSnapshot(bound, repo, hash)
	})
}

func (queries *ArchiveQueries) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadDocument(ctx, repo, read)
	}, func(bound context.Context) (domain.SessionDoc, error) {
		for _, identifier := range []domain.ContentHash{repo, hash} {
			if err := domain.ValidateContentHash(identifier); err != nil {
				return domain.SessionDoc{}, err
			}
		}
		return queries.MaterializedDocumentRead(bound, repo, hash)
	})
}

func (queries *ArchiveQueries) MaterializedDocumentRead(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	return queries.documents.MaterializedDocumentRead(ctx, repo, hash)
}
