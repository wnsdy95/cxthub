package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave42TransactionKey struct{}

type wave42WorkStore struct {
	*store.FSStore
	calls       []string
	get         func(context.Context, domain.ContentHash) (domain.Repo, error)
	commitError error
	readError   error
	lockError   error
	revisionErr error
	auditError  error
	audits      []domain.OrganizationAuditEvent
}

func (storage *wave42WorkStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.calls = append(storage.calls, "metadata")
	if storage.get != nil {
		return storage.get(ctx, repo)
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

func (storage *wave42WorkStore) WithinRepository(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	storage.calls = append(storage.calls, "write")
	if err := write(context.WithValue(ctx, wave42TransactionKey{}, "write")); err != nil {
		return err
	}
	return storage.commitError
}

func (storage *wave42WorkStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	storage.calls = append(storage.calls, "read")
	if err := read(context.WithValue(ctx, wave42TransactionKey{}, "read")); err != nil {
		return err
	}
	return storage.readError
}

func (*wave42WorkStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(wave42TransactionKey{}) != nil
}

func (*wave42WorkStore) InReadOnlyTransaction(ctx context.Context) bool {
	return ctx.Value(wave42TransactionKey{}) == "read"
}

func (storage *wave42WorkStore) LockRepositoryAccess(ctx context.Context, repository, actor string) error {
	storage.calls = append(storage.calls, "lock:"+repository+":"+actor)
	if ctx.Value(wave42TransactionKey{}) != "write" {
		return domain.ErrConflict
	}
	return storage.lockError
}

func (storage *wave42WorkStore) AdvanceRepositoryRevision(_ context.Context, _ domain.ContentHash, pending bool) error {
	kind := "graph"
	if pending {
		kind = "pending"
	}
	storage.calls = append(storage.calls, kind)
	return storage.revisionErr
}

func (storage *wave42WorkStore) AdvanceEvidenceRevision(context.Context, domain.ContentHash) error {
	storage.calls = append(storage.calls, "evidence")
	return storage.revisionErr
}

func (storage *wave42WorkStore) AppendOrganizationAudit(_ context.Context, event domain.OrganizationAuditEvent) error {
	storage.calls = append(storage.calls, "audit")
	storage.audits = append(storage.audits, event)
	return storage.auditError
}

type wave42SQLState string

func (state wave42SQLState) Error() string { return string(state) }

func (state wave42SQLState) SQLState() string { return string(state) }

func wave42Count(calls []string, wanted string) int {
	count := 0
	for _, call := range calls {
		if call == wanted {
			count++
		}
	}
	return count
}

func TestWave42RepositoryWorkConstructionCopiesIdentityWithoutExecuting(test *testing.T) {
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
	supported := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	work := newRepositoryWork(storage, storage, supported)
	if work == nil || len(storage.calls) != 0 {
		test.Fatal("work construction executed storage", storage.calls)
	}
	repo := hh(test.Name())
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	if err := storage.FSStore.RequireDocumentIdentity(systemTestContext(), repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	supported[1] = domain.DocumentIdentityLegacy
	ctx := inbound.WithDocumentIdentities(systemTestContext(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := work.checkDocumentIdentity(ctx, repo, false); err != nil {
		test.Fatal("work retained mutable caller identity selection", err)
	}
	if err := work.checkDocumentIdentity(systemTestContext(), repo, false); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("supported identity substituted for caller support", err)
	}
}

func TestWave42RepositoryWorkAuthorizationAndOriginalKeys(test *testing.T) {
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
	fixture := makeTeamFixture(test, storage.FSStore)
	repo := hh(test.Name())
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo, RepositoryID: fixture.repository.ID}); err != nil {
		test.Fatal(err)
	}
	work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	for _, scenario := range []struct {
		name   string
		actor  string
		action string
		want   error
	}{
		{name: "anonymous", want: domain.ErrUnauthorized},
		{name: "outsider", actor: fixture.outsider.ID, want: domain.ErrForbidden},
		{name: "organization membership is not repository access", actor: fixture.member.ID, want: domain.ErrForbidden},
		{name: "owner", actor: fixture.owner.ID},
		{name: "manage", actor: fixture.owner.ID, action: "manage"},
		{name: "settings", actor: fixture.owner.ID, action: "settings"},
		{name: "unknown action", actor: fixture.owner.ID, action: "unknown", want: domain.ErrForbidden},
		{name: "registration", actor: fixture.outsider.ID, action: "register"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := writeAction(inbound.WithRepositoryActor(context.Background(), scenario.actor), scenario.action)
			storage.calls = nil
			called := false
			result, err := repositoryWriteWithWork(ctx, work, repo, func(bound context.Context) (string, error) {
				called = true
				if bound.Value(writeActionKey{}) != scenario.action || bound.Value(wave42TransactionKey{}) != "write" {
					test.Fatal("original action or transaction context lost")
				}
				return "written", nil
			})
			if !errors.Is(err, scenario.want) || called != (scenario.want == nil) {
				test.Fatal("authorization changed", result, called, err)
			}
			if scenario.want != nil && result != "" {
				test.Fatal("authorization failure leaked a result", result)
			}
			if scenario.actor == "" && !reflect.DeepEqual(storage.calls, []string{"write"}) {
				test.Fatal("anonymous write inspected metadata before actor", storage.calls)
			}
		})
	}
	storage.lockError = domain.ErrConflict
	if err := repositoryWriteErrorWithWork(inbound.WithRepositoryActor(context.Background(), fixture.owner.ID), work, repo, func(context.Context) error {
		test.Fatal("access lock failure reached mutation")
		return nil
	}); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("lock error hidden", err)
	}
	storage.lockError = nil
	unlocked := struct{ outbound.RepositoryStore }{RepositoryStore: storage.FSStore}
	if err := newRepositoryWork(storage, unlocked, nil).authorizeRepositoryWrite(inbound.WithRepositoryActor(context.Background(), fixture.owner.ID), repo); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("transactional writes accepted a missing access locker", err)
	}
	if err := work.authorizeRepositoryWrite(systemTestContext(), repo); err != nil {
		test.Fatal("trusted system entry lost original bypass", err)
	}
}

func TestWave42RepositoryWorkNestedAfterCommitRevisionAndAudit(test *testing.T) {
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
	fixture := makeTeamFixture(test, storage.FSStore)
	repo := hh(test.Name())
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo, RepositoryID: fixture.repository.ID}); err != nil {
		test.Fatal(err)
	}
	work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	ctx := inbound.WithCorrelation(inbound.WithRepositoryActor(context.Background(), fixture.owner.ID), "wave42-request")
	ctx = auditOperation(pendingWriteContext(ctx), "wave42.updated")
	notified := []string{}
	result, err := repositoryWriteWithWork(ctx, work, repo, func(bound context.Context) (string, error) {
		after, ok := bound.Value(afterCommitKey{}).(*afterCommitActions)
		if !ok || after == nil || bound.Value(revisionScopeKey{}) != "pending" {
			test.Fatal("root after-commit or revision marker replaced")
		}
		afterRepositoryCommit(bound, func() { notified = append(notified, "outer") })
		return repositoryWriteWithWork(bound, work, repo, func(nested context.Context) (string, error) {
			if nested.Value(afterCommitKey{}) != after || len(notified) != 0 {
				test.Fatal("nested work lost outer commit ownership")
			}
			afterRepositoryCommit(nested, func() { notified = append(notified, "nested") })
			return "nested result", nil
		})
	})
	if err != nil || result != "nested result" || !reflect.DeepEqual(notified, []string{"outer", "nested"}) {
		test.Fatal(result, notified, err)
	}
	if wave42Count(storage.calls, "write") != 2 || wave42Count(storage.calls, "pending") != 1 || wave42Count(storage.calls, "audit") != 1 || wave42Count(storage.calls, "graph") != 0 {
		test.Fatal("nested work independently revised or audited", storage.calls)
	}
	if len(storage.audits) != 1 || storage.audits[0].Action != "wave42.updated" || storage.audits[0].ActorID != fixture.owner.ID || storage.audits[0].TargetID != fixture.repository.ID || storage.audits[0].CorrelationID != "wave42-request" {
		test.Fatal("original audit operation context lost", storage.audits)
	}
	for _, scope := range []string{"none", "evidence", "pending", "graph"} {
		storage.calls = nil
		if err := work.revisionWrite(context.WithValue(systemTestContext(), revisionScopeKey{}, scope), repo); err != nil {
			test.Fatal(err)
		}
		want := []string{scope}
		if scope == "none" {
			want = nil
		}
		if !reflect.DeepEqual(storage.calls, want) {
			test.Fatal("original revision key routing changed", scope, storage.calls)
		}
	}
}

func TestWave42RepositoryWorkRetryOnlyExplicitAbortCodes(test *testing.T) {
	for _, state := range []wave42SQLState{"40001", "40P01", "23505", "08006"} {
		for _, eventualSuccess := range []bool{false, true} {
			test.Run(string(state)+map[bool]string{false: "/exhausted", true: "/success"}[eventualSuccess], func(test *testing.T) {
				storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
				work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
				attempts, notifications := 0, 0
				result, err := repositoryWriteWithWork(systemTestContext(), work, hh(test.Name()), func(bound context.Context) (string, error) {
					attempts++
					afterRepositoryCommit(bound, func() { notifications++ })
					if eventualSuccess && attempts == 3 {
						return "committed", nil
					}
					return "partial", errors.Join(errors.New("transaction abort"), state)
				})
				retryable := state == "40001" || state == "40P01"
				wantAttempts := 1
				if retryable {
					wantAttempts = 3
				}
				if attempts != wantAttempts || wave42Count(storage.calls, "write") != wantAttempts {
					test.Fatal("retry bound or SQL-state selection changed", attempts, storage.calls, err)
				}
				if retryable && eventualSuccess {
					if err != nil || result != "committed" || notifications != 1 || wave42Count(storage.calls, "graph") != 1 {
						test.Fatal("aborted attempt callbacks escaped", result, notifications, err)
					}
				} else if !errors.Is(err, state) || result != "" || notifications != 0 || wave42Count(storage.calls, "graph") != 0 {
					test.Fatal("failed write published a partial result", result, notifications, err)
				}
			})
		}
	}
}

func TestWave42RepositoryWorkUnknownCommitDoesNotRetryOrNotify(test *testing.T) {
	failure := errors.New("COMMIT acknowledgement lost")
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir()), commitError: failure}
	work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	attempts, notifications := 0, 0
	result, err := repositoryWriteWithWork(systemTestContext(), work, hh(test.Name()), func(bound context.Context) (string, error) {
		attempts++
		afterRepositoryCommit(bound, func() { notifications++ })
		return "unacknowledged", nil
	})
	if !errors.Is(err, failure) || result != "" || attempts != 1 || notifications != 0 || wave42Count(storage.calls, "write") != 1 {
		test.Fatal("unknown outcome was replayed, exposed, or notified", result, attempts, notifications, storage.calls, err)
	}
}

func TestWave42RepositoryWorkRevisionAndAuditFailuresSuppressCommitActions(test *testing.T) {
	for _, stage := range []string{"revision", "audit"} {
		test.Run(stage, func(test *testing.T) {
			storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
			fixture := makeTeamFixture(test, storage.FSStore)
			repo := hh(test.Name())
			if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo, RepositoryID: fixture.repository.ID}); err != nil {
				test.Fatal(err)
			}
			failure := errors.New(stage + " failure")
			if stage == "revision" {
				storage.revisionErr = failure
			} else {
				storage.auditError = failure
			}
			work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
			notifications := 0
			ctx := inbound.WithRepositoryActor(context.Background(), fixture.owner.ID)
			result, err := repositoryWriteWithWork(ctx, work, repo, func(bound context.Context) (string, error) {
				afterRepositoryCommit(bound, func() { notifications++ })
				return "partial", nil
			})
			if !errors.Is(err, failure) || result != "" || notifications != 0 {
				test.Fatal("post-write failure exposed result or notified", result, notifications, err)
			}
			if stage == "revision" && len(storage.audits) != 0 {
				test.Fatal("audit ran after failed revision", storage.audits)
			}
		})
	}
}

func TestWave42RepositoryWorkReadFailuresZeroAndFSPreservesPartialError(test *testing.T) {
	failure := errors.New("snapshot completion failure")
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir()), readError: failure}
	work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	result, err := repositoryReadWithWork(context.Background(), work, func(bound context.Context) (string, error) {
		if bound.Value(wave42TransactionKey{}) != "read" {
			test.Fatal("read did not use bound snapshot")
		}
		return "partial", nil
	})
	if !errors.Is(err, failure) || result != "" {
		test.Fatal("snapshot exit failure leaked a result", result, err)
	}
	storage.readError = nil
	result, err = repositoryReadWithWork(context.Background(), work, func(context.Context) (string, error) { return "partial", failure })
	if !errors.Is(err, failure) || result != "" {
		test.Fatal("transactional callback error leaked a result", result, err)
	}
	fsWork := newRepositoryWork(storage.FSStore, storage.FSStore, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	result, err = repositoryReadWithWork(context.Background(), fsWork, func(context.Context) (string, error) { return "partial", failure })
	if !errors.Is(err, failure) || result != "partial" {
		test.Fatal("FS read changed original partial-error behavior", result, err)
	}
	notified := false
	result, err = repositoryWriteWithWork(systemTestContext(), fsWork, hh(test.Name()), func(bound context.Context) (string, error) {
		afterRepositoryCommit(bound, func() { notified = true })
		return "partial", failure
	})
	if !errors.Is(err, failure) || result != "partial" || !notified {
		test.Fatal("FS write fabricated transaction rollback or delayed immediate action", result, notified, err)
	}
	metadata := struct {
		outbound.MetadataStore
		outbound.RepositoryRevisions
	}{MetadataStore: storage.FSStore, RepositoryRevisions: storage}
	storage.revisionErr = failure
	fsWork = newRepositoryWork(metadata, storage.FSStore, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	result, err = repositoryWriteWithWork(systemTestContext(), fsWork, hh(test.Name()), func(context.Context) (string, error) { return "written", nil })
	if !errors.Is(err, failure) || result != "written" {
		test.Fatal("FS revision failure lost original partial-result semantics", result, err)
	}
	storage.revisionErr, storage.calls = nil, nil
	if err := fsWork.revisionWrite(evidenceWriteContext(systemTestContext()), hh(test.Name())); err != nil || !reflect.DeepEqual(storage.calls, []string{"graph"}) {
		test.Fatal("missing evidence-revision port changed original graph fallback", storage.calls, err)
	}
}

func TestWave42RepositoryWorkRechecksCurrentMembershipAndArchive(test *testing.T) {
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
	fixture := makeTeamFixture(test, storage.FSStore)
	repo := hh(test.Name())
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo, RepositoryID: fixture.repository.ID}); err != nil {
		test.Fatal(err)
	}
	work := newRepositoryWork(storage, storage, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.member.ID)
	write := func(ctx context.Context) error {
		return repositoryWriteErrorWithWork(ctx, work, repo, func(context.Context) error { return nil })
	}
	if err := write(ctx); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("work captured organization membership as repository authority", err)
	}
	if err := storage.FSStore.AddMember(systemTestContext(), domain.Membership{RepositoryID: fixture.repository.ID, UserID: fixture.member.ID, Role: domain.RoleMember}); err != nil {
		test.Fatal(err)
	}
	if err := write(ctx); err != nil {
		test.Fatal("work failed to use current repository membership", err)
	}
	for _, action := range []string{"manage", "settings"} {
		if err := write(writeAction(ctx, action)); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("member gained maintainer action", action, err)
		}
	}
	if err := storage.FSStore.RemoveMember(systemTestContext(), fixture.repository.ID, fixture.member.ID); err != nil {
		test.Fatal(err)
	}
	if err := write(ctx); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("work reused revoked membership", err)
	}
	fixture.repository.Archived = true
	if err := storage.FSStore.CreateRepository(systemTestContext(), fixture.repository); err != nil {
		test.Fatal(err)
	}
	if err := write(inbound.WithRepositoryActor(context.Background(), fixture.owner.ID)); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("current archive policy did not deny owner writes", err)
	}
}

func TestWave42RepositoryWorkIdentityErrorOrderAndCancellation(test *testing.T) {
	storage := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}
	repo := hh(test.Name())
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	if err := storage.FSStore.RequireDocumentIdentity(systemTestContext(), repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	work := newRepositoryWork(storage, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	if err := repositoryWriteErrorWithWork(context.Background(), work, repo, func(context.Context) error {
		test.Fatal("unauthorized caller reached mutation")
		return nil
	}); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("identity failure replaced original authorization order", err)
	}
	storage.calls = nil
	if err := repositoryWriteErrorWithWork(systemTestContext(), work, repo, func(context.Context) error {
		test.Fatal("unsupported root identity reached mutation")
		return nil
	}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(storage.calls, []string{"write", "metadata"}) {
		test.Fatal("identity error crossed mutation/revision", storage.calls, err)
	}
	ctx, cancel := context.WithCancel(systemTestContext())
	cancel()
	storage.calls = nil
	if err := work.checkDocumentIdentity(ctx, repo, false); !errors.Is(err, context.Canceled) || len(storage.calls) != 0 {
		test.Fatal("canceled identity check accessed metadata", storage.calls, err)
	}
	if err := work.checkDocumentIdentity(ctx, "invalid", false); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("cancellation displaced original hash-validation order", err)
	}
	missing := hh("wave42 missing")
	if err := work.checkDocumentIdentity(systemTestContext(), missing, true); err != nil {
		test.Fatal("allowMissing compatibility lost", err)
	}
	if err := work.checkDocumentIdentity(systemTestContext(), missing, false); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("strict missing repository accepted", err)
	}
}
