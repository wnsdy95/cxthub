package gitchangeworker

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Profile string

const (
	Compatibility Profile = "compatibility"
	Complete      Profile = "complete"
)

type Scope interface {
	WorkerContext(context.Context) context.Context
	WithinWork(context.Context, func(context.Context) error) error
	ReadRepository(context.Context, domain.ContentHash) (domain.Repo, error)
	WithinEvidenceWrite(context.Context, domain.ContentHash, func(context.Context) error) error
}

type RepositoryReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type Config struct {
	Profile    Profile
	Scope      Scope
	Jobs       outbound.GitChangeStore
	Metadata   RepositoryReader
	Reader     outbound.GitEvidenceReader
	Cache      evidencequery.GitDeltaReader
	Authorizer outbound.GitSourceAuthorizer
}
