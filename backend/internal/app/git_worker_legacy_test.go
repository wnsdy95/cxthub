package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var ()

type wave42LegacyGitTransactionKey struct{}

type wave42LegacyWorkMetadata struct {
	outbound.MetadataStore
	repo        domain.Repo
	getError    error
	revision    error
	evidence    int
	pending     []bool
	readCtx     []context.Context
	revisionCtx context.Context
}

func (metadata *wave42LegacyWorkMetadata) GetRepo(ctx context.Context, _ domain.ContentHash) (domain.Repo, error) {
	metadata.readCtx = append(metadata.readCtx, ctx)
	return metadata.repo, metadata.getError
}

func (metadata *wave42LegacyWorkMetadata) AdvanceEvidenceRevision(ctx context.Context, _ domain.ContentHash) error {
	metadata.evidence++
	metadata.revisionCtx = ctx
	return metadata.revision
}

func (metadata *wave42LegacyWorkMetadata) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	return domain.RepositoryRevision{}, nil
}

func (metadata *wave42LegacyWorkMetadata) AdvanceRepositoryRevision(ctx context.Context, _ domain.ContentHash, pending bool) error {
	metadata.pending = append(metadata.pending, pending)
	metadata.revisionCtx = ctx
	return metadata.revision
}

type wave42LegacyWorkTransactions struct {
	*wave42LegacyWorkMetadata
	writes   int
	reads    int
	outcomes []error
}

func (metadata *wave42LegacyWorkTransactions) WithinRepository(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	metadata.writes++
	if err := write(context.WithValue(ctx, wave42LegacyGitTransactionKey{}, "write")); err != nil {
		return err
	}
	if metadata.writes <= len(metadata.outcomes) {
		return metadata.outcomes[metadata.writes-1]
	}
	return nil
}

func (metadata *wave42LegacyWorkTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	metadata.reads++
	return read(context.WithValue(ctx, wave42LegacyGitTransactionKey{}, "read"))
}

type wave42LegacyAbort string

func (failure wave42LegacyAbort) Error() string    { return string(failure) }
func (failure wave42LegacyAbort) SQLState() string { return string(failure) }

func TestWave42LegacyRepositoryWorkSharesOriginalKeysAndNestedServiceTransaction(test *testing.T) {
	repo := hh(test.Name())
	metadata := &wave42LegacyWorkTransactions{wave42LegacyWorkMetadata: &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo}}}
	work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	service := &Service{meta: metadata}
	committed := 0
	ctx := evidenceWriteContext(systemTestContext())
	value, err := repositoryWrite(ctx, service, repo, func(bound context.Context) (string, error) {
		if _, nested := bound.Value(afterCommitKey{}).(*afterCommitActions); !nested {
			test.Fatal("original after-commit key was replaced")
		}
		return repositoryWriteWithWork(bound, work, repo, func(nested context.Context) (string, error) {
			afterRepositoryCommit(nested, func() { committed++ })
			if nested.Value(revisionScopeKey{}) != "evidence" || nested.Value(wave42LegacyGitTransactionKey{}) != "write" || committed != 0 {
				test.Fatal("nested work escaped original transaction or fired before commit")
			}
			return "result", nil
		})
	})
	if err != nil || value != "result" || metadata.writes != 2 || metadata.evidence != 1 || committed != 1 || len(metadata.pending) != 0 {
		test.Fatal("shared work duplicated revision or after-commit actions", value, err, metadata.writes, metadata.evidence, committed)
	}
	for _, scope := range []string{"none", "pending", ""} {
		if err := work.revisionWrite(context.WithValue(ctx, revisionScopeKey{}, scope), repo); err != nil {
			test.Fatal(err)
		}
	}
	if metadata.evidence != 1 || !reflect.DeepEqual(metadata.pending, []bool{true, false}) {
		test.Fatal("original revision keys changed", metadata.evidence, metadata.pending)
	}
	value, err = repositoryReadWithWork(ctx, work, func(bound context.Context) (string, error) {
		if bound.Value(wave42LegacyGitTransactionKey{}) != "read" {
			test.Fatal("read callback not snapshot-bound")
		}
		return "read-result", domain.ErrIntegrity
	})
	if value != "" || !errors.Is(err, domain.ErrIntegrity) || metadata.reads != 1 {
		test.Fatal("failed transactional read leaked result", value, err, metadata.reads)
	}
}

func TestWave42LegacyRepositoryWorkAbortRetriesAndUnknownAcknowledgement(test *testing.T) {
	unknown := errors.New("commit outcome unknown")
	for _, scenario := range []struct {
		name     string
		outcomes []error
		writes   int
		failure  error
	}{
		{"serialization", []error{wave42LegacyAbort("40001"), wave42LegacyAbort("40001")}, 3, nil},
		{"deadlock", []error{wave42LegacyAbort("40P01")}, 2, nil},
		{"retry-limit", []error{wave42LegacyAbort("40001"), wave42LegacyAbort("40001"), wave42LegacyAbort("40001")}, 3, wave42LegacyAbort("40001")},
		{"unknown-ack", []error{unknown}, 1, unknown},
		{"other-state", []error{wave42LegacyAbort("23505")}, 1, wave42LegacyAbort("23505")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			repo := hh(test.Name())
			metadata := &wave42LegacyWorkTransactions{wave42LegacyWorkMetadata: &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo}}, outcomes: scenario.outcomes}
			work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
			committed, callbacks := 0, 0
			value, err := repositoryWriteWithWork(evidenceWriteContext(systemTestContext()), work, repo, func(bound context.Context) (string, error) {
				callbacks++
				afterRepositoryCommit(bound, func() { committed++ })
				return "value", nil
			})
			if !errors.Is(err, scenario.failure) || metadata.writes != scenario.writes || callbacks != scenario.writes {
				test.Fatal("transaction replay classification changed", err, metadata.writes, callbacks)
			}
			if err == nil && (value != "value" || committed != 1) || err != nil && (value != "" || committed != 0) {
				test.Fatal("failed attempt leaked result or after-commit actions", value, committed, err)
			}
		})
	}
}

func TestWave42LegacyRepositoryWorkPreservesFSFailureAndIdentityOrdering(test *testing.T) {
	repo := hh(test.Name())
	metadata := &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo}, revision: domain.ErrIntegrity}
	work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	called := 0
	value, err := repositoryWriteWithWork(evidenceWriteContext(systemTestContext()), work, repo, func(context.Context) (string, error) {
		called++
		return "fs-result", nil
	})
	if value != "fs-result" || !errors.Is(err, domain.ErrIntegrity) || called != 1 {
		test.Fatal("FS callback/revision error result order changed", value, err, called)
	}
	metadata.repo.RequiredDocIdentity = domain.DocumentIdentityRootV1
	metadata.revision = nil
	if err := repositoryWriteErrorWithWork(systemTestContext(), work, repo, func(context.Context) error {
		called++
		return nil
	}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || called != 1 {
		test.Fatal("unsupported document identity reached write", err, called)
	}
	metadata.getError = domain.ErrNotFound
	if err := work.checkDocumentIdentity(systemTestContext(), repo, true); err != nil {
		test.Fatal("legacy missing repository no longer allowed", err)
	}
	if err := work.checkDocumentIdentity(systemTestContext(), repo, false); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("required repository lookup failure hidden", err)
	}
	if err := work.authorizeRepositoryWrite(context.Background(), repo); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("anonymous write error ordering changed", err)
	}
	if err := work.authorizeRepositoryWrite(writeAction(inbound.WithRepositoryActor(context.Background(), "actor"), "register"), repo); err != nil {
		test.Fatal("registration incorrectly required an existing binding", err)
	}
}

type wave42LegacyWorkRepositories struct {
	outbound.RepositoryStore
	outbound.OrganizationStore
	record domain.Repository
	role   domain.MemberRole
	locks  int
	audit  []domain.OrganizationAuditEvent
	bound  context.Context
}

func (repositories *wave42LegacyWorkRepositories) GetRepository(context.Context, string) (domain.Repository, error) {
	return repositories.record, nil
}

func (repositories *wave42LegacyWorkRepositories) ListMembers(context.Context, string) ([]domain.Membership, error) {
	return []domain.Membership{{RepositoryID: repositories.record.ID, UserID: "actor", Role: repositories.role}}, nil
}

func (repositories *wave42LegacyWorkRepositories) LockRepositoryAccess(ctx context.Context, _, _ string) error {
	repositories.locks++
	repositories.bound = ctx
	return nil
}

func (repositories *wave42LegacyWorkRepositories) GetNamespace(context.Context, string) (domain.Namespace, error) {
	return domain.Namespace{Kind: domain.NamespaceOrganization, OrganizationID: "organization"}, nil
}

func (repositories *wave42LegacyWorkRepositories) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if repositories.bound == nil || ctx.Value(wave42LegacyGitTransactionKey{}) != "write" || ctx.Value(wave42LegacyGitTransactionKey{}) != repositories.bound.Value(wave42LegacyGitTransactionKey{}) {
		return domain.ErrIntegrity
	}
	repositories.audit = append(repositories.audit, event)
	return nil
}

func TestWave42LegacyRepositoryWorkRechecksAuthorityAndUsesOriginalAuditAction(test *testing.T) {
	repo := hh(test.Name())
	metadata := &wave42LegacyWorkTransactions{wave42LegacyWorkMetadata: &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}}
	repositories := &wave42LegacyWorkRepositories{record: domain.Repository{ID: "repository", OwnerID: "owner", OwnerNamespaceID: "namespace"}, role: domain.RoleMember}
	work := newRepositoryWork(metadata, repositories, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	ctx := auditOperation(inbound.WithRepositoryActor(context.Background(), "actor"), "evidence.updated")
	called := 0
	write := func(context.Context) error { called++; return nil }
	if err := repositoryWriteErrorWithWork(ctx, work, repo, write); err != nil || called != 1 || repositories.locks != 1 || len(repositories.audit) != 1 {
		test.Fatal("shared work lost current authority lock or audit", err, called, repositories.locks, repositories.audit)
	}
	if event := repositories.audit[0]; event.ActorID != "actor" || event.Action != "evidence.updated" || event.TargetID != "repository" || event.TargetType != "repository" {
		test.Fatal("original audit context key changed", event)
	}
	repositories.role = domain.RolePuller
	if err := repositoryWriteErrorWithWork(ctx, work, repo, write); !errors.Is(err, domain.ErrForbidden) || called != 1 || len(repositories.audit) != 1 || repositories.locks != 2 {
		test.Fatal("revoked authority reused cached authorization", err, called, repositories.locks)
	}
	repositories.role = domain.RoleMember
	if err := repositoryWriteErrorWithWork(writeAction(ctx, "manage"), work, repo, write); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("management action ignored original action key", err)
	}
	repositories.role = domain.RoleMaintainer
	repositories.record.SettingsPolicy = "owner"
	if err := repositoryWriteErrorWithWork(writeAction(ctx, "settings"), work, repo, write); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("settings owner policy bypassed", err)
	}
	repositories.record.Archived = true
	if err := repositoryWriteErrorWithWork(ctx, work, repo, write); !errors.Is(err, domain.ErrForbidden) || called != 1 {
		test.Fatal("archived repository reached write", err, called)
	}
}
