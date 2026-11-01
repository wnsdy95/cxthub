package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryArchiveQueries interface {
	ListRepos(context.Context, string) ([]domain.Repo, error)
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	List(context.Context, ListSnapshotsInput) ([]domain.Snapshot, error)
	ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error)
	Search(context.Context, SearchInput) (SearchOutput, error)
}
