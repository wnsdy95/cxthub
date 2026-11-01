//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func historyCommandsOwnerPG(test *testing.T, service *Service) *HistoryCommands {
	test.Helper()
	metadata, repositories := service.meta, service.repositories
	public, err := service.HistoryCommands()
	if err != nil {
		test.Fatal(err)
	}
	commands, ok := public.(*HistoryCommands)
	if !ok || commands == nil {
		test.Fatalf("public factory returned %T, not the actual history owner", public)
	}
	scope, ok := commands.scope.(*historyCommandScope)
	if !ok || scope == nil || scope.work == nil || scope.work.meta != metadata || scope.work.repositories != repositories {
		test.Fatalf("history scope %T did not capture original repository work", commands.scope)
	}
	service.meta, service.blobs, service.repositories, service.engine, service.auth = nil, nil, nil, nil, nil
	return commands
}

type historyCommandsStorePG struct {
	*documentPublicationProbePG
	afterVerify func(context.Context) error
}

func (storage *historyCommandsStorePG) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	if err := storage.note(ctx, "history"); err != nil {
		return err
	}
	if err := storage.PostgresStore.ApplyHistoryEvent(ctx, event); err != nil {
		return err
	}
	return storage.after("history")
}

func (storage *historyCommandsStorePG) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	proof, err := storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
	if err == nil && storage.afterVerify != nil {
		err = storage.afterVerify(ctx)
	}
	return proof, err
}

func historyCommandsAdvancePG(test *testing.T, fixture settingsCodexFixturePG) domain.HistoryEvent {
	test.Helper()
	base := collaborationSnapshot(test, fixture.storage, fixture.repo, "history owner base")
	tip := collaborationSnapshot(test, fixture.storage, fixture.repo, "history owner tip", base)
	branch := domain.LegacyContextBranchID(string(fixture.repo), "main")
	if err := fixture.storage.CompareAndSwapRef(fixture.ctx, fixture.repo, domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", BranchID: branch, Target: base}, ""); err != nil {
		test.Fatal(err)
	}
	return domain.HistoryEvent{ID: "00000000000000000000000000000001", RepoID: string(fixture.repo), BranchID: branch, Branch: "main", Kind: "advance", Source: base, Target: tip, SharedTarget: tip, CreatedAt: time.Now().UTC()}
}

func TestPGHistoryCommandsIndependentAcceptedReplayWithoutBody(test *testing.T) {
	service, probe, event := historyDocumentLockFixture(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	repo := domain.ContentHash(event.RepoID)
	if err := probe.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: event.BranchID, Target: event.Source}, ""); err != nil {
		test.Fatal(err)
	}
	event.Kind = "advance"
	event.Target = collaborationSnapshot(test, probe.PostgresStore, repo, "accepted independent target", event.Source)
	event.SharedTarget = event.Target
	commands := historyCommandsOwnerPG(test, service)
	if err := commands.RecordHistory(ctx, event); err != nil {
		test.Fatal(err)
	}
	accepted, err := probe.ListHistoryEvents(ctx, repo)
	if err != nil || !reflect.DeepEqual(accepted, []domain.HistoryEvent{event}) {
		test.Fatal("actual owner did not persist exact history", accepted, err)
	}
	current, err := probe.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || current.Target != event.Target {
		test.Fatal("actual owner did not advance current main", current, err)
	}
	current.Target = collaborationSnapshot(test, probe.PostgresStore, repo, "later current main", event.Target)
	if err := probe.CompareAndSwapRef(ctx, repo, current, event.Target); err != nil {
		test.Fatal(err)
	}
	metadata, err := probe.GetRepo(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	metadata.ProtectDefault = true
	if _, err := probe.PutRepo(ctx, metadata); err != nil {
		test.Fatal(err)
	}
	if removed, err := probe.peer.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, event.RepoID, string(event.Target)); err != nil || removed.RowsAffected() != 1 {
		test.Fatal("accepted body ownership was not removed", removed, err)
	}
	verifications, applies := len(probe.observations), probe.applies
	if err := commands.RecordHistory(ctx, event); err != nil {
		test.Fatal("accepted replay rechecked body or protected-branch policy", err)
	}
	changed := event
	changed.CreatedAt = changed.CreatedAt.Add(time.Second)
	if err := commands.RecordHistory(ctx, changed); !errors.Is(err, domain.ErrRefConflict) {
		test.Fatal("conflicting accepted ID bypassed immutable payload equality", err)
	}
	if verifications != len(probe.observations) || applies != probe.applies {
		test.Fatal("accepted replay or conflict reverified or reapplied")
	}
	if events, err := probe.ListHistoryEvents(ctx, repo); err != nil || !reflect.DeepEqual(events, accepted) {
		test.Fatal("replay changed actual history", events, err)
	}
	if ref, err := probe.GetRef(ctx, repo, domain.RefBranch, "main"); err != nil || ref != current {
		test.Fatal("replay rewound the later current pointer", ref, err)
	}
}

func TestPGHistoryCommandsAtomicFailureAuditAndRevision(test *testing.T) {
	for _, phase := range []string{"history", "revision", "audit"} {
		test.Run(phase, func(test *testing.T) {
			fixture := settingsCodexPG(test)
			event := historyCommandsAdvancePG(test, fixture)
			failure := errors.New("failure after actual " + phase)
			storage := &historyCommandsStorePG{documentPublicationProbePG: &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int), failAt: phase, failure: failure}}
			commands := historyCommandsOwnerPG(test, documentPublicationServicePG(storage))
			member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
			before := documentPublicationImagePG(test, fixture)
			audits := documentPublicationAuditsPG(test, fixture)
			revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo)
			if err != nil {
				test.Fatal(err)
			}
			if err := commands.RecordHistory(member, event); !errors.Is(err, failure) || storage.calls[phase] != 1 {
				test.Fatal("independent command missed actual failure boundary", phase, storage.calls, err)
			}
			if documentPublicationImagePG(test, fixture) != before {
				test.Fatal("failed history left refs, retention, revision, audit or outbox effects")
			}
			storage.failAt = ""
			if err := commands.RecordHistory(member, event); err != nil {
				test.Fatal("authorized independent retry", err)
			}
			if events, err := fixture.storage.ListHistoryEvents(fixture.ctx, fixture.repo); err != nil || !reflect.DeepEqual(events, []domain.HistoryEvent{event}) {
				test.Fatal("retry did not persist one exact event", events, err)
			}
			if ref, err := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefBranch, "main"); err != nil || ref.Target != event.Target || ref.BranchID != event.BranchID {
				test.Fatal("retry lost the actual branch CAS", ref, err)
			}
			revision.Graph++
			if current, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo); err != nil || current != revision {
				test.Fatal("retry did not commit exactly one graph revision", current, revision, err)
			}
			documentPublicationAuditPG(test, fixture, audits, "context.updated")
		})
	}
}

func TestPGHistoryCommandsFinalAuthorityAfterPreparationSQLBarrier(test *testing.T) {
	fixture := settingsCodexPG(test)
	event := historyCommandsAdvancePG(test, fixture)
	observer := authorityScopeObserver(test, fixture.ctx)
	writer, writerName := authorityScopePeer(test, fixture.ctx)
	authority, authorityName := authorityScopePeer(test, fixture.ctx)
	storage := &historyCommandsStorePG{documentPublicationProbePG: &documentPublicationProbePG{PostgresStore: writer, calls: make(map[string]int)}}
	commands := historyCommandsOwnerPG(test, documentPublicationServicePG(storage))
	verified, resume := make(chan struct{}), make(chan struct{})
	var resumed, observed sync.Once
	defer resumed.Do(func() { close(resume) })
	storage.afterVerify = func(read context.Context) error {
		if !storage.InReadOnlyTransaction(read) {
			return domain.ErrIntegrity
		}
		observed.Do(func() { close(verified) })
		select {
		case <-resume:
			return nil
		case <-read.Done():
			return read.Err()
		}
	}
	before := documentPublicationImagePG(test, fixture)
	writeDone, revokeDone := make(chan error, 1), make(chan error, 1)
	member := inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
	go func() { writeDone <- commands.RecordHistory(member, event) }()
	authorityScopeAwait(test, fixture.ctx, verified, writeDone)
	release := make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	paused := &authorityScopeHoldPG{PostgresStore: authority, ready: make(chan struct{}), release: release}
	go func() {
		revokeDone <- authorityScopeMutation(fixture.ctx, NewIdentityService(nil, paused), fixture.team, "role")
	}()
	authorityScopeAwait(test, fixture.ctx, paused.ready, revokeDone)
	resumed.Do(func() { close(resume) })
	authorityScopeBlocked(test, fixture.ctx, observer, writerName, authorityName, "repositories", writeDone)
	if documentPublicationImagePG(test, fixture) != before {
		test.Fatal("preparation or blocked final admission exposed writes")
	}
	released.Do(func() { close(release) })
	if err := authorityScopeDone(test, fixture.ctx, revokeDone); err != nil {
		test.Fatal(err)
	}
	if err := authorityScopeDone(test, fixture.ctx, writeDone); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("prepared independent owner retained revoked write authority", err)
	}
	if storage.calls["history"] != 0 || documentPublicationImagePG(test, fixture) != before {
		test.Fatal("revoked final admission changed history, current pointer, audit or revision")
	}
}

func TestPGHistoryCommandsPRPrivateOwnerSharesOriginalWork(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, true)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	storage := &prPromotionPGTransactionStore{PostgresStore: fixture.storage, failure: "finish"}
	service := documentPublicationServicePG(storage)
	config := service.PRPromotionWorkerConfig(PRPromotionComplete)
	config.RequireCompleteMemory = true
	commands := historyCommandsOwnerPG(test, service)
	scope := commands.scope.(*historyCommandScope)
	worker := assemblePRPromotionWorker(config, scope.work)
	privateScope, ok := worker.history.scope.(*historyCommandScope)
	if !ok || privateScope == nil || privateScope.work != scope.work || worker.work != scope.work {
		test.Fatal("private PR history reconstructed rather than sharing original repository work")
	}
	before, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if after, err := fixture.publication(ctx, fixture.storage); err != nil || !reflect.DeepEqual(after, before) || storage.injected != "finish" {
		test.Fatal("private history escaped failed outer PR completion", after, err)
	}
	job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
	if err != nil || job.State != "attention" || job.Reason != "integrity_check_failed" {
		test.Fatal("failed shared PR publication lost its durable claim", job, err)
	}
	storage.failure = ""
	if err := fixture.service.RetryPRPromotion(ctx, fixture.repo, job.ID); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	after, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	bindings, completions := prPromotionPGReceipts(after.history, fixture.job.PR)
	if bindings != 1 || completions != 1 || after.revision.Graph <= before.revision.Graph || len(after.notifications) != len(before.notifications)+1 || !reflect.DeepEqual(after.audit, before.audit) {
		test.Fatal("shared private owner lost exact receipts, revision, outbox or audit scope", after)
	}
	if ref, err := fixture.storage.GetRef(ctx, fixture.repo, domain.RefBranch, "main"); err != nil || ref.Target != fixture.tip || ref.BranchID != fixture.job.BaseBranchID {
		test.Fatal("shared PR owner lost the final current pointer", ref, err)
	}
	if current, err := fixture.storage.GetPRJob(ctx, fixture.repo, job.ID); err != nil || current.State != "completed" {
		test.Fatal("shared PR owner did not settle its original job", current, err)
	}
	assertResourcesReleased(test, processResources)
}
