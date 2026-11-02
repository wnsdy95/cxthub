//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func contextBranchOwnerPG(test *testing.T, service *Service) *ContextBranchOperations {
	test.Helper()
	public, err := service.ContextBranchOperations()
	operations, ok := any(public).(*ContextBranchOperations)
	if err != nil || !ok || operations == nil {
		test.Fatalf("factory returned %T, not the actual context branch owner: %v", public, err)
	}
	service.meta, service.blobs, service.repositories, service.engine, service.auth = nil, nil, nil, nil, nil
	return operations
}

type contextBranchFixturePG struct {
	repositoryRefFixturePG
	head, source domain.ContentHash
}

func newContextBranchFixturePG(test *testing.T) (contextBranchFixturePG, *contextBranchProbePG, inbound.JoinPreviewInput) {
	test.Helper()
	_, storage, repo := collaborationPG(test)
	ctx := systemTestContext()
	team := makeTeamFixture(test, storage)
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: team.repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: team.repository.ID, UserID: team.member.ID, Role: domain.RoleMember}); err != nil {
		test.Fatal(err)
	}
	fixture := contextBranchFixturePG{repositoryRefFixturePG: repositoryRefFixturePG{ctx: ctx, storage: storage, authorityScopeFixture: authorityScopeFixture{team: team, repo: repo}}}
	fixture.base = collaborationSnapshot(test, storage, repo, "P")
	fixture.head = collaborationSnapshot(test, storage, repo, "H", fixture.base)
	fixture.source = collaborationSnapshot(test, storage, repo, "X", fixture.base)
	fixture.tip = collaborationSnapshot(test, storage, repo, "T", fixture.source)
	fixture.loose = collaborationSnapshot(test, storage, repo, "private capture")
	var previous domain.ContentHash
	for _, target := range []domain.ContentHash{fixture.base, fixture.source, fixture.tip, fixture.head} {
		if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: target}, previous); err != nil {
			test.Fatal(err)
		}
		previous = target
	}
	if err := storage.AddGraftParents(ctx, repo, fixture.head, []domain.ContentHash{fixture.tip}); err != nil {
		test.Fatal(err)
	}
	for session, target := range map[string]domain.ContentHash{"shared": fixture.source, "private": fixture.loose} {
		if err := storage.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: session, Target: target}); err != nil {
			test.Fatal(err)
		}
	}
	probe := &contextBranchProbePG{documentPublicationProbePG: &documentPublicationProbePG{PostgresStore: storage, calls: make(map[string]int)}, scopes: make(map[string]*afterCommitActions)}
	return fixture, probe, inbound.JoinPreviewInput{ActorID: team.member.ID, RepoID: repo, Snapshot: fixture.source}
}

type contextBranchProbePG struct {
	*documentPublicationProbePG
	scopes   map[string]*afterCommitActions
	events   []domain.HistoryEvent
	verified []historyDocumentKey
}

func (storage *contextBranchProbePG) mark(ctx context.Context, phase string) error {
	if err := storage.note(ctx, phase); err != nil {
		return err
	}
	after, _ := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	if after == nil {
		return domain.ErrIntegrity
	}
	storage.scopes[phase] = after
	return nil
}

func (storage *contextBranchProbePG) ApplyJoin(ctx context.Context, mutation domain.JoinMutation) error {
	if err := storage.mark(ctx, "join"); err != nil {
		return err
	}
	return storage.PostgresStore.ApplyJoin(ctx, mutation)
}

func (storage *contextBranchProbePG) CompareAndDeletePending(ctx context.Context, repo domain.ContentHash, session string, target domain.ContentHash) (domain.PendingDeleteResult, error) {
	if err := storage.mark(ctx, "pending"); err != nil {
		return domain.PendingDeleteKept, err
	}
	result, err := storage.PostgresStore.CompareAndDeletePending(ctx, repo, session, target)
	if err != nil {
		return result, err
	}
	return result, storage.after("pending")
}

func (storage *contextBranchProbePG) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if err := storage.mark(ctx, "proof"); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	proof, err := storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
	if err != nil {
		return proof, err
	}
	storage.verified = append(storage.verified, historyDocumentKey{repo: repo, snapshot: hash, document: proof.DocumentRef()})
	return proof, storage.after("proof")
}

func (storage *contextBranchProbePG) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	if err := storage.mark(ctx, "history"); err != nil {
		return err
	}
	if err := storage.PostgresStore.ApplyHistoryEvent(ctx, event); err != nil {
		return err
	}
	storage.events = append(storage.events, event)
	return storage.after("history")
}

func TestPGContextBranchOwnerJoinAtomicity(test *testing.T) {
	for _, scenario := range []string{"whole", "partial", "pending", "revision", "audit"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, probe, input := newContextBranchFixturePG(test)
			operations := contextBranchOwnerPG(test, documentPublicationServicePG(probe))
			before := fixture.image(test)
			var preview inbound.JoinPreviewOutput
			if err := fixture.storage.WithinReadSnapshot(context.Background(), func(read context.Context) error {
				var err error
				preview, err = operations.PreviewJoin(read, input)
				if err != nil {
					return err
				}
				history, err := operations.ListHistory(read, fixture.repo)
				if err == nil && (!fixture.storage.InReadOnlyTransaction(read) || len(history) != 0) {
					return domain.ErrIntegrity
				}
				return err
			}); err != nil {
				test.Fatal("direct owner could not borrow read-only snapshot", err)
			}
			if preview.Reason != "" || preview.Descendants != 1 || preview.Tip != fixture.tip || preview.ExpectedHead != fixture.head || preview.Branch != "main" || preview.OnlyRevision == "" || preview.AllRevision == "" || preview.OnlyRevision == preview.AllRevision || !reflect.DeepEqual(preview.DropTargets, []domain.ContentHash{fixture.head, fixture.base}) || probe.transactions != 0 || !reflect.DeepEqual(fixture.image(test), before) {
				test.Fatal("read-only preview changed state or lost the original join plan", preview)
			}
			command := confirmPreview(input, preview, scenario != "partial")
			ambient := inbound.WithRepositoryActor(context.Background(), fixture.team.outsider.ID)
			if scenario != "whole" && scenario != "partial" {
				probe.failAt, probe.failure = scenario, errors.New("rollback after "+scenario)
				output, err := operations.ConfirmJoin(ambient, command)
				if !errors.Is(err, probe.failure) || output != (inbound.JoinOutput{}) || !reflect.DeepEqual(fixture.image(test), before) || probe.calls[scenario] != 1 {
					test.Fatal("failed owner exposed join, pending, history, audit or revision effects", output, err)
				}
				probe.failAt = ""
			}
			output, err := operations.ConfirmJoin(ambient, command)
			if err != nil {
				test.Fatal(err)
			}
			want := fixture.tip
			if scenario == "partial" {
				want = fixture.source
				residual, err := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefSession, output.ForkBranch)
				if err != nil || residual.Target != fixture.tip {
					test.Fatal("partial join lost natural descendants", residual, err)
				}
			} else if output.ForkBranch != "" {
				test.Fatal("whole join allocated a residual ref", output)
			}
			main, refErr := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefBranch, "main")
			source, snapErr := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.source)
			after := fixture.image(test)
			if refErr != nil || snapErr != nil || output.Head != want || main.Target != want || source.GraftSeq != 1 || !reflect.DeepEqual(source.Parents, []domain.ContentHash{fixture.base}) || !reflect.DeepEqual(source.GraftParents, []domain.ContentHash{fixture.head}) || probe.scopes["join"] != probe.scopes["pending"] {
				test.Fatal("owner lost atomic main, graft, natural parents or passed pending scope", output, main, source)
			}
			if len(after.pending) != 1 || after.pending[0].SessionID != "private" || after.pending[0].Target != fixture.loose || !reflect.DeepEqual(after.documents, before.documents) || len(after.snapshots) != len(before.snapshots) || after.revision.Graph != before.revision.Graph+1 || after.revision.Pending != before.revision.Pending || len(after.audit) != len(before.audit)+1 || after.audit[0].ActorID != input.ActorID {
				test.Fatal("explicit actor join lost retained captures or committed more than one scope", after)
			}
		})
	}
}

func TestPGContextBranchOwnerConcurrentConfirmation(test *testing.T) {
	fixture, probe, input := newContextBranchFixturePG(test)
	operations := contextBranchOwnerPG(test, documentPublicationServicePG(probe))
	preview, err := operations.PreviewJoin(fixture.ctx, input)
	if err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	other := contextBranchOwnerPG(test, documentPublicationServicePG(peer))
	before := fixture.image(test)
	start, results := make(chan struct{}), make(chan error, 2)
	for _, owner := range []*ContextBranchOperations{operations, other} {
		go func() {
			<-start
			_, err := owner.ConfirmJoin(context.Background(), confirmPreview(input, preview, true))
			results <- err
		}()
	}
	close(start)
	wins, stale := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			wins++
		} else if errors.Is(err, domain.ErrJoinPreviewChanged) {
			stale++
		} else {
			test.Fatal(err)
		}
	}
	main, refErr := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefBranch, "main")
	source, snapErr := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.source)
	after := fixture.image(test)
	if wins != 1 || stale != 1 || refErr != nil || snapErr != nil || main.Target != fixture.tip || source.GraftSeq != 1 || after.revision.Graph != before.revision.Graph+1 || len(after.audit) != len(before.audit)+1 || len(after.pending) != 1 {
		test.Fatal("concurrent direct owners did not commit exactly once", wins, stale, main, source, after)
	}
}

func TestPGContextBranchOwnerCurrentExplicitActor(test *testing.T) {
	for _, scenario := range []string{"missing actor", "outsider", "revoked member"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, probe, input := newContextBranchFixturePG(test)
			operations := contextBranchOwnerPG(test, documentPublicationServicePG(probe))
			preview, err := operations.PreviewJoin(fixture.ctx, input)
			if err != nil {
				test.Fatal(err)
			}
			want := domain.ErrForbidden
			switch scenario {
			case "missing actor":
				input.ActorID, want = "", domain.ErrUnauthorized
			case "outsider":
				input.ActorID = fixture.team.outsider.ID
			case "revoked member":
				if err := fixture.team.identity.RemoveMember(fixture.ctx, fixture.team.owner.ID, fixture.team.repository.ID, fixture.team.member.ID); err != nil {
					test.Fatal(err)
				}
			}
			before := fixture.image(test)
			ambient := inbound.WithRepositoryActor(context.Background(), fixture.team.owner.ID)
			if _, err := operations.PreviewJoin(ambient, input); !errors.Is(err, want) {
				test.Fatal("preview trusted ambient owner or stale membership", err)
			}
			if output, err := operations.ConfirmJoin(ambient, confirmPreview(input, preview, true)); !errors.Is(err, want) || output != (inbound.JoinOutput{}) || !reflect.DeepEqual(fixture.image(test), before) || probe.calls["join"] != 0 {
				test.Fatal("confirmation reused preview as authority", output, err)
			}
		})
	}
}

func TestPGContextBranchOwnerProtocolAndExactForkSource(test *testing.T) {
	fixture, probe, _ := newContextBranchFixturePG(test)
	service := documentPublicationServicePG(probe)
	recorder, err := documentPublicationServicePG(fixture.storage).HistoryCommands()
	if err != nil {
		test.Fatal(err)
	}
	operations := contextBranchOwnerPG(test, service)
	member := inbound.WithRepositoryActor(context.Background(), fixture.team.member.ID)
	input := inbound.ForkInput{RepoID: fixture.repo, FromSnapshot: fixture.source, NewBranch: "legacy"}
	if output, err := operations.Fork(member, input); err != nil || output.Head != fixture.source || probe.calls["history"] != 0 || probe.calls["proof"] != 0 {
		test.Fatal("protocol zero fork no longer uses direct CAS", output, err)
	}
	if err := operations.EnableContextProtocol(member, fixture.repo); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("member bypassed protocol manage authority", err)
	}
	if metadata, err := fixture.storage.GetRepo(fixture.ctx, fixture.repo); err != nil || metadata.ContextProtocol != 0 {
		test.Fatal("denied protocol upgrade changed storage", metadata, err)
	}
	owner := inbound.WithRepositoryActor(context.Background(), fixture.team.owner.ID)
	if err := operations.EnableContextProtocol(owner, fixture.repo); err != nil {
		test.Fatal(err)
	}
	if metadata, err := fixture.storage.GetRepo(fixture.ctx, fixture.repo); err != nil || metadata.ContextProtocol != 1 {
		test.Fatal("direct owner did not persist protocol", metadata, err)
	}
	retained := fixture.image(test)
	input.NewBranch = "feature"
	for _, phase := range []string{"proof", "history", ""} {
		before := fixture.image(test)
		probe.failAt, probe.failure = phase, errors.New("fork failure after "+phase)
		probe.events, probe.verified = nil, nil
		output, err := operations.Fork(member, input)
		if phase != "" {
			if !errors.Is(err, probe.failure) || output != (inbound.ForkOutput{}) || !reflect.DeepEqual(fixture.image(test), before) {
				test.Fatal("failed fork published an unverified birth or partial ref", phase, output, err)
			}
			continue
		}
		if err != nil || output.Branch != input.NewBranch || output.Head != fixture.source || len(probe.events) != 1 || len(probe.verified) != 1 || probe.scopes["proof"] != probe.scopes["history"] {
			test.Fatal("fork bypassed original recorder or writable source proof", output, err)
		}
	}
	birth := probe.events[0]
	expected := domain.HistoryEvent{ID: birth.ID, RepoID: string(fixture.repo), BranchID: birth.ID, Branch: "feature", Kind: "birth", Source: fixture.source, Target: fixture.source, CreatedAt: birth.CreatedAt}
	proof := probe.verified[0]
	if !reflect.DeepEqual(birth, expected) || domain.ValidateHistoryEvent(birth) != nil || birth.CreatedAt.IsZero() || proof.repo != fixture.repo || proof.snapshot != fixture.source || proof.document != (domain.DocumentRef{Hash: fixture.source, Identity: domain.DocumentIdentityLegacy}) {
		test.Fatal("fork fabricated source, identity, protocol or history evidence", birth, proof)
	}
	archive := domain.HistoryEvent{ID: "00000000000000000000000000000063", RepoID: string(fixture.repo), BranchID: birth.BranchID, Branch: birth.Branch, Kind: "archive", BindingParent: birth.ID, Source: fixture.source, Target: fixture.source, CreatedAt: time.Now().UTC()}
	if err := recorder.RecordHistory(member, archive); err != nil {
		test.Fatal(err)
	}
	if _, err := operations.Fork(member, input); err != nil {
		test.Fatal("released branch alias could not be reused", err)
	}
	reborn := probe.events[len(probe.events)-1]
	if reborn.BindingParent != archive.ID || reborn.BranchID == birth.BranchID || reborn.Source != fixture.source || reborn.Target != fixture.source {
		test.Fatal("reused alias lost released binding or exact source", reborn)
	}
	main, refErr := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefBranch, "main")
	after := fixture.image(test)
	if refErr != nil || main.Target != fixture.head || !reflect.DeepEqual(after.snapshots, retained.snapshots) || !reflect.DeepEqual(after.documents, retained.documents) || !reflect.DeepEqual(after.pending, retained.pending) {
		test.Fatal("fork changed current main, immutable parents, documents or pending captures", after, refErr)
	}
	if err := fixture.storage.WithinReadSnapshot(member, func(read context.Context) error {
		actual, err := operations.ListHistory(read, fixture.repo)
		stored, storedErr := fixture.storage.ListHistoryEvents(read, fixture.repo)
		expectedHistory := []domain.HistoryEvent{birth, archive, reborn}
		if err != nil || storedErr != nil || !reflect.DeepEqual(actual, stored) || !reflect.DeepEqual(actual, expectedHistory) {
			return domain.ErrIntegrity
		}
		return nil
	}); err != nil {
		test.Fatal("borrowed history changed exact immutable events", err)
	}
}
