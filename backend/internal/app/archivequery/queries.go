package archivequery

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type Scope interface {
	ReadRepositories(context.Context, func(context.Context) error) error
	ReadRepository(context.Context, domain.ContentHash, func(context.Context) error) error
	ReadDocument(context.Context, domain.ContentHash, func(context.Context) error) error
}

type MetadataReader interface {
	ListRepos(context.Context, string) ([]domain.Repo, error)
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error)
	ReadReflog(context.Context, domain.ContentHash) ([]domain.RefLogEntry, error)
}

type CandidateReader interface {
	MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error)
}

type Config struct {
	Profile    documentquery.Profile
	Scope      Scope
	Metadata   MetadataReader
	Documents  *documentquery.RawReader
	Candidates CandidateReader
	AtomicRead bool
}

type ArchiveQueries struct {
	scope      Scope
	metadata   MetadataReader
	documents  *documentquery.RawReader
	candidates CandidateReader
	atomicRead bool
}

var _ inbound.RepositoryArchiveQueries = (*ArchiveQueries)(nil)

func New(config Config) (*ArchiveQueries, error) {
	if config.Profile != documentquery.LegacyCompatibility && config.Profile != documentquery.VerifiedRoots {
		return nil, fmt.Errorf("%w: archive query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Metadata, config.Documents} {
		if absent(required) {
			return nil, fmt.Errorf("%w: missing archive query dependency", domain.ErrValidation)
		}
	}
	if config.Candidates != nil && absent(config.Candidates) {
		return nil, fmt.Errorf("%w: typed-nil archive query dependency", domain.ErrValidation)
	}
	if config.Documents.Profile() != config.Profile {
		return nil, fmt.Errorf("%w: archive document query profile mismatch", domain.ErrValidation)
	}
	return &ArchiveQueries{scope: config.Scope, metadata: config.Metadata, documents: config.Documents,
		candidates: config.Candidates, atomicRead: config.AtomicRead}, nil
}

func absent(value any) bool {
	if value == nil {
		return true
	}
	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	}
	return false
}

func scopedRead[Result any](queries *ArchiveQueries, execute func(func(context.Context) error) error, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := execute(func(bound context.Context) error {
		var err error
		result, err = read(bound)
		return err
	})
	if err != nil && queries.atomicRead {
		var empty Result
		return empty, err
	}
	return result, err
}
