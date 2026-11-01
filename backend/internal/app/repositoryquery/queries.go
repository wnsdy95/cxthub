package repositoryquery

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type Profile = inbound.RepositoryStateQueryProfile

const (
	LegacyCompatibility = inbound.RepositoryStateCompatibility
	PinnedSnapshots     = inbound.RepositoryStatePinnedSnapshots
)

type Scope interface {
	AuthorizeRead(context.Context, domain.ContentHash, bool) (context.Context, error)
	WithinRepositoryRead(context.Context, domain.ContentHash, func(context.Context) error) error
}

type Resources interface {
	AcquireInteractive(context.Context) (context.Context, func(), error)
}

type MetadataReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error)
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	ReadReflog(context.Context, domain.ContentHash) ([]domain.RefLogEntry, error)
	ListPendings(context.Context, domain.ContentHash) ([]domain.Pending, error)
	ListUnsyncs(context.Context, domain.ContentHash) ([]domain.Unsync, error)
}

type RevisionReader interface {
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
}

type HistoryReader interface {
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
}

type ArchiveReader interface {
	ListSessionArchives(context.Context, domain.ContentHash) ([]domain.SessionArchive, error)
}

type PendingReader interface {
	ReadPendingProjection(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error)
}

type PendingStatusReader interface {
	ReadPendingProjectionStatus(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error)
}

type GraphStatusReader interface {
	ReadQualifiedGraphProjectionStatus(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error)
}

type Config struct {
	Profile      Profile
	Scope        Scope
	Resources    Resources
	Metadata     MetadataReader
	Revisions    RevisionReader
	History      HistoryReader
	Archives     ArchiveReader
	Pending      PendingReader
	PendingState PendingStatusReader
	GraphState   GraphStatusReader
	PendingRepos []domain.ContentHash
	GraphRepos   []domain.ContentHash
	AtomicRead   bool
}

type Queries struct {
	scope        Scope
	resources    Resources
	metadata     MetadataReader
	revisions    RevisionReader
	history      HistoryReader
	archives     ArchiveReader
	pending      PendingReader
	pendingState PendingStatusReader
	graphState   GraphStatusReader
	pendingRepos map[domain.ContentHash]bool
	graphRepos   map[domain.ContentHash]bool
	atomicRead   bool
}

var _ inbound.RepositoryStateQueries = (*Queries)(nil)

func New(config Config) (*Queries, error) {
	if config.Profile != LegacyCompatibility && config.Profile != PinnedSnapshots {
		return nil, fmt.Errorf("%w: repository query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Resources, config.Metadata} {
		if absent(required) {
			return nil, fmt.Errorf("%w: missing repository query dependency", domain.ErrValidation)
		}
	}
	for _, optional := range []any{config.Revisions, config.History, config.Archives, config.Pending, config.PendingState, config.GraphState} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: typed-nil repository query dependency", domain.ErrValidation)
		}
	}
	if config.Profile == PinnedSnapshots && (config.Revisions == nil || config.History == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	if len(config.PendingRepos) > 0 && (config.Pending == nil || config.Profile == PinnedSnapshots && config.PendingState == nil) || len(config.GraphRepos) > 0 && config.GraphState == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	queries := &Queries{scope: config.Scope, resources: config.Resources, metadata: config.Metadata, revisions: config.Revisions,
		history: config.History, archives: config.Archives, pending: config.Pending, pendingState: config.PendingState, graphState: config.GraphState,
		pendingRepos: make(map[domain.ContentHash]bool, len(config.PendingRepos)), graphRepos: make(map[domain.ContentHash]bool, len(config.GraphRepos)),
		atomicRead: config.AtomicRead || config.Profile == PinnedSnapshots}
	for _, configured := range []struct {
		input []domain.ContentHash
		repos map[domain.ContentHash]bool
	}{{config.PendingRepos, queries.pendingRepos}, {config.GraphRepos, queries.graphRepos}} {
		for _, repo := range configured.input {
			if err := domain.ValidateContentHash(repo); err != nil {
				return nil, err
			}
			configured.repos[repo] = true
		}
	}
	return queries, nil
}

func absent(value any) bool {
	if value == nil {
		return true
	}
	switch candidate := reflect.ValueOf(value); candidate.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return candidate.IsNil()
	}
	return false
}

func readRepository[Result any](ctx context.Context, queries *Queries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := queries.scope.WithinRepositoryRead(ctx, repo, func(bound context.Context) error {
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
