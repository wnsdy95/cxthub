//go:build postgres

package app

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func repositoryRefOwnerPG(test *testing.T, service *Service) *RepositoryRefCommands {
	test.Helper()
	public, err := service.RepositoryRefCommands()
	if err != nil {
		test.Fatal(err)
	}
	commands, ok := public.(*RepositoryRefCommands)
	if !ok || commands == nil || commands.profile != RepositoryRefComplete {
		test.Fatalf("public Complete factory returned %T, not the actual reference owner", public)
	}
	scope, ok := commands.scope.(*repositoryRefScope)
	if !ok || scope == nil || scope.work == nil {
		test.Fatalf("actual reference owner did not retain its original repository scope: %T", commands.scope)
	}
	service.meta, service.blobs, service.engine, service.repositories, service.auth = nil, nil, nil, nil, nil
	return commands
}

func repositoryRefObserverPG(test *testing.T, ctx context.Context) *pgxpool.Pool {
	test.Helper()
	config, err := pgxpool.ParseConfig(collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	name := "rq60-observer-" + domain.NewID("")[:12]
	config.ConnConfig.RuntimeParams["application_name"] = name
	observer, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(observer.Close)
	var accepted string
	if err := observer.QueryRow(ctx, `SHOW application_name`).Scan(&accepted); err != nil || accepted != name || len(name) > 63 {
		test.Fatal("SQL observer identity was not accepted exactly", accepted, name, err)
	}
	return observer
}

func repositoryRefPeerPG(test *testing.T, ctx context.Context, observer *pgxpool.Pool, repo domain.ContentHash, role string) (*store.PostgresStore, string) {
	test.Helper()
	name := "rq60-" + role + "-" + domain.NewID("")[:12]
	if len(name) > 63 {
		test.Fatal("SQL peer identity exceeds PostgreSQL's limit", name)
	}
	dsn := collaborationDSN(test)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			test.Fatal(err)
		}
		query := parsed.Query()
		query.Set("application_name", name)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " application_name=" + name
	}
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.GetRepo(ctx, repo); err != nil {
		test.Fatal(err)
	}
	var accepted string
	if err := observer.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1 LIMIT 1`, name).Scan(&accepted); err != nil || accepted != name {
		test.Fatal("SQL peer identity was not accepted exactly", accepted, name, err)
	}
	return storage, name
}

type repositoryRefFixturePG struct {
	ctx      context.Context
	storage  *store.PostgresStore
	observer *pgxpool.Pool
	authorityScopeFixture
	orphan domain.ContentHash
	loose  domain.ContentHash
}

func newRepositoryRefFixturePG(test *testing.T) repositoryRefFixturePG {
	test.Helper()
	wave40IsolatedPG(test)
	_, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 20*time.Second)
	test.Cleanup(cancel)
	observer := repositoryRefObserverPG(test, ctx)
	fixture := repositoryRefFixturePG{ctx: ctx, storage: storage, observer: observer,
		authorityScopeFixture: authorityScopeSeed(test, ctx, storage, observer, repo)}
	var mappings int
	if err := observer.QueryRow(ctx, `SELECT count(*) FROM repos WHERE repository_id=$1`, fixture.team.repository.ID).Scan(&mappings); err != nil || mappings != 1 {
		test.Fatal("fixture must have exactly one context/product repository mapping", mappings, err)
	}
	fixture.orphan = collaborationSnapshot(test, storage, repo, "ref owner orphan")
	fixture.loose = collaborationSnapshot(test, storage, repo, "ref owner unshared capture")
	for _, ref := range []domain.Ref{
		{Kind: domain.RefBranch, Name: "topic", Target: fixture.base},
		{Kind: domain.RefTag, Name: "v1", Target: fixture.base},
		{Kind: domain.RefSession, Name: "retained/scope", Target: fixture.base},
	} {
		if err := storage.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
			test.Fatal(err)
		}
	}
	for _, pending := range []domain.Pending{
		{RepoID: repo, SessionID: "graft-base", Target: fixture.base},
		{RepoID: repo, SessionID: "incoming", Target: fixture.orphan},
		{RepoID: repo, SessionID: "unshared", Target: fixture.loose},
	} {
		if err := storage.PutPending(ctx, repo, pending); err != nil {
			test.Fatal(err)
		}
	}
	if err := storage.CreateSession(ctx, domain.Session{Token: domain.HashToken(test.Name()), UserID: fixture.team.member.ID,
		Kind: "cli", Label: "retained ref-owner login", ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		test.Fatal(err)
	}
	return fixture
}

type repositoryRefImagePG struct {
	refs          []domain.Ref
	snapshots     []domain.Snapshot
	documents     []domain.SessionDoc
	history       []domain.HistoryEvent
	reflog        []domain.RefLogEntry
	pending       []domain.Pending
	revision      domain.RepositoryRevision
	notifications []domain.NotificationJob
	audit         []domain.OrganizationAuditEvent
	sessions      []domain.Session
}

func (fixture repositoryRefFixturePG) image(test *testing.T) repositoryRefImagePG {
	test.Helper()
	var state repositoryRefImagePG
	err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		var err error
		if state.refs, err = fixture.storage.ListRefs(read, fixture.repo); err != nil {
			return err
		}
		if state.snapshots, err = fixture.storage.ListSnapshots(read, fixture.repo, ""); err != nil {
			return err
		}
		for _, snapshot := range state.snapshots {
			document, err := fixture.storage.GetDoc(read, fixture.repo, snapshot.DocHash)
			if err != nil {
				return err
			}
			state.documents = append(state.documents, document)
		}
		if state.history, err = fixture.storage.ListHistoryEvents(read, fixture.repo); err != nil {
			return err
		}
		if state.reflog, err = fixture.storage.ReadReflog(read, fixture.repo); err != nil {
			return err
		}
		if state.pending, err = fixture.storage.ListPendings(read, fixture.repo); err != nil {
			return err
		}
		if state.revision, err = fixture.storage.RepositoryRevision(read, fixture.repo); err != nil {
			return err
		}
		if state.notifications, err = fixture.storage.ListNotifications(read, fixture.team.repository.ID); err != nil {
			return err
		}
		if state.audit, err = fixture.storage.ListOrganizationAudit(read, fixture.team.organization.ID, 500); err != nil {
			return err
		}
		state.sessions, err = fixture.storage.ListSessionsForUser(read, fixture.team.member.ID)
		return err
	})
	if err != nil {
		test.Fatal(err)
	}
	return state
}

type repositoryRefProbePG struct {
	*store.PostgresStore
	calls          map[string]int
	transactions   int
	failAt         string
	failure        error
	replaceSession string
	replacement    domain.ContentHash
}

func (storage *repositoryRefProbePG) WithinRepository(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	if !storage.InTransaction(ctx) {
		storage.transactions++
	}
	return storage.PostgresStore.WithinRepository(ctx, repo, write)
}

func (storage *repositoryRefProbePG) note(ctx context.Context, phase string) error {
	storage.calls[phase]++
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) || phase != "revision" && phase != "audit" && ctx.Value(afterCommitKey{}) == nil {
		return errors.New("actual ref command escaped its original writable scope")
	}
	return nil
}

func (storage *repositoryRefProbePG) after(phase string) error {
	if storage.failAt == phase {
		return storage.failure
	}
	return nil
}

func (storage *repositoryRefProbePG) CompareAndSwapRef(ctx context.Context, repo domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	if err := storage.note(ctx, "cas"); err != nil {
		return err
	}
	if err := storage.PostgresStore.CompareAndSwapRef(ctx, repo, ref, expected); err != nil {
		return err
	}
	return storage.after("cas")
}

func (storage *repositoryRefProbePG) AppendRef(ctx context.Context, repo domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	if err := storage.note(ctx, "append"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AppendRef(ctx, repo, ref, expected); err != nil {
		return err
	}
	return storage.after("append")
}

func (storage *repositoryRefProbePG) ListPendings(ctx context.Context, repo domain.ContentHash) ([]domain.Pending, error) {
	if err := storage.note(ctx, "pending-list"); err != nil {
		return nil, err
	}
	return storage.PostgresStore.ListPendings(ctx, repo)
}

func (storage *repositoryRefProbePG) CompareAndDeletePending(ctx context.Context, repo domain.ContentHash, session string, expected domain.ContentHash) (domain.PendingDeleteResult, error) {
	if err := storage.note(ctx, "pending-delete"); err != nil {
		return domain.PendingDeleteKept, err
	}
	if session == storage.replaceSession {
		if err := storage.PostgresStore.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: session, Target: storage.replacement}); err != nil {
			return domain.PendingDeleteKept, err
		}
	}
	result, err := storage.PostgresStore.CompareAndDeletePending(ctx, repo, session, expected)
	if err != nil {
		return result, err
	}
	return result, storage.after("pending-delete")
}

func (storage *repositoryRefProbePG) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	if err := storage.note(ctx, "outbox"); err != nil {
		return err
	}
	if err := storage.PostgresStore.EnqueueNotification(ctx, delivery); err != nil {
		return err
	}
	return storage.after("outbox")
}

func (storage *repositoryRefProbePG) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.note(ctx, "revision"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	return storage.after("revision")
}

func (storage *repositoryRefProbePG) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.note(ctx, "audit"); err != nil {
		return err
	}
	if err := storage.PostgresStore.AppendOrganizationAudit(ctx, event); err != nil {
		return err
	}
	return storage.after("audit")
}

func repositoryRefProbeOwnerPG(test *testing.T, storage *repositoryRefProbePG) *RepositoryRefCommands {
	test.Helper()
	return repositoryRefOwnerPG(test, NewService(storage, storage, nil, gitengine.NewEngine(storage), storage))
}

func repositoryRefActorPG(fixture repositoryRefFixturePG) context.Context {
	return inbound.WithRepositoryActor(fixture.ctx, fixture.team.member.ID)
}

func repositoryRefAuditCountPG(events []domain.OrganizationAuditEvent, action string) int {
	count := 0
	for _, event := range events {
		if event.Action == action {
			count++
		}
	}
	return count
}

func TestPGRepositoryRefOwnerBatchAtomicPublicationAndRollback(test *testing.T) {
	for _, failure := range []string{"missing-target", "non-fast-forward", "append", "pending-delete", "outbox", "revision", "audit", "success"} {
		test.Run(failure, func(test *testing.T) {
			fixture := newRepositoryRefFixturePG(test)
			probe := &repositoryRefProbePG{PostgresStore: fixture.storage, calls: make(map[string]int), failAt: failure, failure: errors.New("ref-owner injected " + failure)}
			owner := repositoryRefProbeOwnerPG(test, probe)
			input := inbound.UpdateRefsInput{RepoID: fixture.repo, Updates: []inbound.UpdateRefInput{
				{Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: fixture.orphan}, ExpectedTarget: fixture.base, Append: true},
				{Ref: domain.Ref{Kind: domain.RefBranch, Name: "topic", Target: fixture.tip}, ExpectedTarget: fixture.base},
			}}
			want := probe.failure
			switch failure {
			case "missing-target":
				input.Updates[1].Ref.Target, want = hh("missing batch ref-owner target"), domain.ErrIntegrity
			case "non-fast-forward":
				input.Updates[1].Ref.Target = fixture.loose
				input.Updates = append(input.Updates, inbound.UpdateRefInput{Ref: domain.Ref{Kind: domain.RefTag, Name: "v1", Target: fixture.tip}})
				want = domain.ErrNonFastForward
			case "success":
				want = nil
			}
			before := fixture.image(test)
			output, err := owner.UpdateRefs(repositoryRefActorPG(fixture), input)
			if !errors.Is(err, want) {
				test.Fatalf("batch error=%v want=%v", err, want)
			}
			if probe.transactions != 1 || probe.calls["append"] != 1 {
				test.Fatalf("batch did not traverse one actual scope and append body: transactions=%d calls=%v", probe.transactions, probe.calls)
			}
			after := fixture.image(test)
			if failure != "success" {
				if output != (inbound.UpdateRefsOutput{}) || !reflect.DeepEqual(after, before) {
					test.Fatalf("failed Complete batch exposed acknowledgement or durable effects: output=%+v before=%+v after=%+v", output, before, after)
				}
				if failure == "non-fast-forward" && (!strings.Contains(err.Error(), "branch/topic") || !strings.Contains(err.Error(), "tag/v1")) {
					test.Fatal("actual batch stopped aggregating rejected names", err)
				}
				if want == probe.failure && probe.calls[failure] != 1 {
					test.Fatal("failure never crossed its real SQL mutation", failure, probe.calls)
				}
				return
			}
			if output.Applied != 2 || probe.calls["pending-list"] != 1 || probe.calls["pending-delete"] != 2 || probe.calls["revision"] != 1 || probe.calls["audit"] != 1 || probe.calls["outbox"] != 2 {
				test.Fatal("batch split revision/audit or reconciled per item", output, probe.calls)
			}
			if after.revision.Graph != before.revision.Graph+1 || after.revision.Pending != before.revision.Pending || after.revision.Evidence != before.revision.Evidence ||
				len(after.notifications) != len(before.notifications)+2 || len(after.reflog) != len(before.reflog)+2 || len(after.audit) != len(before.audit)+1 ||
				repositoryRefAuditCountPG(after.audit, "context.refs.updated") != repositoryRefAuditCountPG(before.audit, "context.refs.updated")+1 {
				test.Fatal("successful batch lost its single original durable publication boundary", before, after)
			}
			if len(after.pending) != 1 || after.pending[0].SessionID != "unshared" || after.pending[0].Target != fixture.loose ||
				!reflect.DeepEqual(after.documents, before.documents) || !reflect.DeepEqual(after.sessions, before.sessions) || !reflect.DeepEqual(after.history, before.history) {
				test.Fatal("pending reconciliation removed immutable capture or session data", after)
			}
			for _, expected := range []struct {
				name   string
				target domain.ContentHash
			}{{"main", fixture.orphan}, {"topic", fixture.tip}} {
				ref, err := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefBranch, expected.name)
				if err != nil || ref.Target != expected.target {
					test.Fatal("batch lost a requested ref", ref, expected, err)
				}
			}
			snapshot, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.orphan)
			if err != nil || len(snapshot.Parents) != 0 || !reflect.DeepEqual(snapshot.GraftParents, []domain.ContentHash{fixture.base}) {
				test.Fatal("actual append changed natural ancestry or lost its graft", snapshot, err)
			}
		})
	}
}

func TestPGRepositoryRefOwnerConditionalFences(test *testing.T) {
	for _, mode := range []string{"graph", "evidence", "head", "same-target", "branch", "missing-target", "force", "not-append", "revoked", "revision-rollback"} {
		test.Run(mode, func(test *testing.T) {
			wave40IsolatedPG(test)
			fixture, storage, input := conditionalAppendPGFixture(test)
			probe := &repositoryRefProbePG{PostgresStore: storage, calls: make(map[string]int), failure: domain.ErrIntegrity}
			owner := repositoryRefProbeOwnerPG(test, probe)
			want := domain.ErrRefConflict
			switch mode {
			case "graph":
				if err := storage.AdvanceRepositoryRevision(fixture.ctx, fixture.repo, false); err != nil {
					test.Fatal(err)
				}
			case "evidence":
				if err := storage.AdvanceEvidenceRevision(fixture.ctx, fixture.repo); err != nil {
					test.Fatal(err)
				}
			case "head":
				input.ExpectedTarget = fixture.other
			case "same-target":
				input.Ref.Target, input.ExpectedTarget = fixture.before.Target, fixture.other
			case "branch":
				input.Ref.BranchID = "reused-context-identity"
			case "missing-target":
				input.ExpectedTarget, want = "", domain.ErrIntegrity
			case "force":
				input.Force, want = true, domain.ErrValidation
			case "not-append":
				input.Append, want = false, domain.ErrValidation
			case "revoked":
				if err := storage.WithinIdentity(fixture.ctx, func(bound context.Context) error {
					return storage.AddMember(bound, domain.Membership{RepositoryID: fixture.repository, UserID: fixture.member, Role: domain.RoleViewer})
				}); err != nil {
					test.Fatal(err)
				}
				input.ExpectedRevision.Graph++
				want = domain.ErrForbidden
			case "revision-rollback":
				probe.failAt, want = "revision", domain.ErrIntegrity
			}
			before := conditionalAppendPGState(test, fixture, storage)
			output, err := owner.UpdateRef(fixture.ctx, input)
			if !errors.Is(err, want) || !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) {
				test.Fatal("Complete conditional failure exposed a success or wrong error", mode, output, err, want)
			}
			if after := conditionalAppendPGState(test, fixture, storage); after != before {
				test.Fatal("actual conditional owner changed the original PostgreSQL oracle", mode, before, after)
			}
			if mode == "revision-rollback" && (probe.calls["append"] != 1 || probe.calls["revision"] != 1) {
				test.Fatal("conditional rollback did not reach the actual append and revision", probe.calls)
			}
		})
	}
}

func TestPGRepositoryRefOwnerConditionalWaitsForActualSQLWriter(test *testing.T) {
	for _, change := range []string{"graph", "evidence", "head", "branch"} {
		test.Run(change, func(test *testing.T) {
			wave40IsolatedPG(test)
			fixture, storage, input := conditionalAppendPGFixture(test)
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			observer := repositoryRefObserverPG(test, ctx)
			holder, holderName := repositoryRefPeerPG(test, ctx, observer, fixture.repo, "semantic")
			writer, writerName := repositoryRefPeerPG(test, ctx, observer, fixture.repo, "conditional")
			owner := repositoryRefOwnerPG(test, NewService(writer, writer, nil, gitengine.NewEngine(writer), writer))
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			paused := &authorityScopeWriteHoldPG{PostgresStore: holder, ready: make(chan struct{}), release: release}
			first, second := make(chan error, 1), make(chan error, 1)
			go func() {
				first <- paused.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
					switch change {
					case "graph":
						return holder.AdvanceRepositoryRevision(bound, fixture.repo, false)
					case "evidence":
						return holder.AdvanceEvidenceRevision(bound, fixture.repo)
					default:
						ref, err := holder.GetRef(bound, fixture.repo, domain.RefBranch, "main")
						if err != nil {
							return err
						}
						if change == "head" {
							ref.Target = fixture.other
						} else {
							ref.BranchID = "replacement-context-identity"
						}
						return holder.CompareAndSwapRef(bound, fixture.repo, ref, fixture.before.Target)
					}
				})
			}()
			authorityScopeAwait(test, ctx, paused.ready, first)
			before := conditionalAppendPGState(test, fixture, storage)
			go func() {
				output, err := owner.UpdateRef(ctx, input)
				if !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) {
					second <- errors.New("blocked conditional failure exposed a success acknowledgement")
					return
				}
				second <- err
			}()
			authorityScopeBlocked(test, ctx, observer, writerName, holderName, "pg_advisory_xact_lock", second)
			if visible := conditionalAppendPGState(test, fixture, storage); visible != before {
				test.Fatal("semantic writer or append escaped the held SQL transaction", before, visible)
			}
			releaseOnce.Do(func() { close(release) })
			if err := authorityScopeDone(test, ctx, first); err != nil {
				test.Fatal(err)
			}
			if err := authorityScopeDone(test, ctx, second); !errors.Is(err, domain.ErrRefConflict) {
				test.Fatal("conditional owner ignored a writer that committed ahead of it", change, err)
			}
			snapshot, err := storage.GetSnapshot(ctx, fixture.repo, fixture.other)
			if err != nil || len(snapshot.GraftParents) != 0 {
				test.Fatal("rejected SQL waiter grafted the incoming capture", snapshot, err)
			}
			ref, err := storage.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
			if err != nil || change == "head" && ref.Target != fixture.other || change != "head" && ref.Target != fixture.before.Target || change == "branch" && ref.BranchID != "replacement-context-identity" {
				test.Fatal("rejected SQL waiter overwrote the winning writer", ref, err)
			}
			revision, err := storage.RepositoryRevision(ctx, fixture.repo)
			want := *input.ExpectedRevision
			if change == "graph" {
				want.Graph++
			}
			if change == "evidence" {
				want.Evidence++
			}
			if err != nil || revision != want {
				test.Fatal("rejected waiter published an extra revision", revision, want, err)
			}
		})
	}
}

func TestPGRepositoryRefOwnerConditionalConcurrentAndLegacyRetry(test *testing.T) {
	wave40IsolatedPG(test)
	fixture, storage, input := conditionalAppendPGFixture(test)
	ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
	defer cancel()
	owner := repositoryRefOwnerPG(test, fixture.svc)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	other := repositoryRefOwnerPG(test, NewService(peer, peer, nil, gitengine.NewEngine(peer), peer))
	if err := storage.AdvanceRepositoryRevision(ctx, fixture.repo, true); err != nil {
		test.Fatal(err)
	}
	type result struct {
		output inbound.UpdateRefOutput
		err    error
	}
	start, done := make(chan struct{}), make(chan result, 2)
	for _, commands := range []*RepositoryRefCommands{owner, other} {
		go func() {
			<-start
			output, err := commands.UpdateRef(ctx, input)
			done <- result{output, err}
		}()
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		select {
		case current := <-done:
			switch {
			case current.err == nil && current.output.Result == inbound.RefAppended:
				succeeded++
			case errors.Is(current.err, domain.ErrRefConflict) && reflect.DeepEqual(current.output, inbound.UpdateRefOutput{}):
				conflicted++
			default:
				test.Fatal("unexpected concurrent conditional result", current)
			}
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		}
	}
	if succeeded != 1 || conflicted != 1 {
		test.Fatal("one conditional plan did not have exactly one SQL winner", succeeded, conflicted)
	}
	revision, err := storage.RepositoryRevision(ctx, fixture.repo)
	if err != nil || revision.Graph != input.ExpectedRevision.Graph+1 || revision.Evidence != input.ExpectedRevision.Evidence || revision.Pending != input.ExpectedRevision.Pending+1 {
		test.Fatal("pending churn invalidated the plan or losers published revisions", revision, err)
	}
	before := conditionalAppendPGState(test, fixture, storage)
	if output, err := other.UpdateRef(ctx, input); !errors.Is(err, domain.ErrRefConflict) || !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) {
		test.Fatal("stale same-target conditional replay bypassed its semantic revision", output, err)
	}
	if after := conditionalAppendPGState(test, fixture, storage); after != before {
		test.Fatal("rejected conditional replay changed the original oracle", before, after)
	}
	logs, err := storage.ReadReflog(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	input.ExpectedRevision = nil
	for range 2 {
		output, err := other.UpdateRef(ctx, input)
		if err != nil || output.Result != inbound.RefUpToDate || output.Ref.Target != fixture.other {
			test.Fatal("legacy same-target retry lost its idempotent result", output, err)
		}
	}
	afterLogs, err := storage.ReadReflog(ctx, fixture.repo)
	snapshot, snapshotErr := storage.GetSnapshot(ctx, fixture.repo, fixture.other)
	if err != nil || snapshotErr != nil || !reflect.DeepEqual(logs, afterLogs) || len(snapshot.Parents) != 0 || !reflect.DeepEqual(snapshot.GraftParents, []domain.ContentHash{fixture.before.Target}) {
		test.Fatal("duplicate owner retries repeated a ref move or changed natural ancestry", afterLogs, snapshot, err, snapshotErr)
	}
}

func TestPGRepositoryRefOwnerCurrentAuthorityBothSQLOrders(test *testing.T) {
	for _, kind := range []string{"single", "batch"} {
		for _, mutation := range []string{"role", "remove", "archive"} {
			for _, order := range []string{"revoke-first", "write-first"} {
				test.Run(kind+"/"+mutation+"/"+order, func(test *testing.T) {
					fixture := newRepositoryRefFixturePG(test)
					ctx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
					defer cancel()
					writer, writerName := repositoryRefPeerPG(test, ctx, fixture.observer, fixture.repo, "writer")
					authority, authorityName := repositoryRefPeerPG(test, ctx, fixture.observer, fixture.repo, "authority")
					release := make(chan struct{})
					var releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					var owner *RepositoryRefCommands
					var heldWrite *authorityScopeWriteHoldPG
					if order == "write-first" {
						heldWrite = &authorityScopeWriteHoldPG{PostgresStore: writer, ready: make(chan struct{}), release: release}
						owner = repositoryRefOwnerPG(test, NewService(heldWrite, heldWrite, nil, gitengine.NewEngine(heldWrite), heldWrite))
					} else {
						owner = repositoryRefOwnerPG(test, NewService(writer, writer, nil, gitengine.NewEngine(writer), writer))
					}
					write := func() error {
						input := inbound.UpdateRefInput{RepoID: fixture.repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: fixture.tip}, ExpectedTarget: fixture.base}
						actor := inbound.WithRepositoryActor(ctx, fixture.team.member.ID)
						if kind == "single" {
							output, err := owner.UpdateRef(actor, input)
							if err != nil && !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) {
								return errors.New("denied single ref write exposed a success acknowledgement")
							}
							return err
						}
						output, err := owner.UpdateRefs(actor, inbound.UpdateRefsInput{RepoID: fixture.repo, Updates: []inbound.UpdateRefInput{input}})
						if err != nil && output != (inbound.UpdateRefsOutput{}) {
							return errors.New("denied ref batch exposed an applied count")
						}
						return err
					}
					before := fixture.image(test)
					first, second := make(chan error, 1), make(chan error, 1)
					if order == "revoke-first" {
						paused := &authorityScopeHoldPG{PostgresStore: authority, ready: make(chan struct{}), release: release}
						go func() { first <- authorityScopeMutation(ctx, NewIdentityService(nil, paused), fixture.team, mutation) }()
						authorityScopeAwait(test, ctx, paused.ready, first)
						go func() { second <- write() }()
						authorityScopeBlocked(test, ctx, fixture.observer, writerName, authorityName, "repositories", second)
					} else {
						go func() { first <- write() }()
						authorityScopeAwait(test, ctx, heldWrite.ready, first)
						go func() {
							second <- authorityScopeMutation(ctx, NewIdentityService(nil, authority), fixture.team, mutation)
						}()
						authorityScopeBlocked(test, ctx, fixture.observer, authorityName, writerName, "repositories", second)
					}
					if visible := fixture.image(test); !reflect.DeepEqual(visible, before) {
						test.Fatal("held actual owner/authority transaction exposed partial durable effects", before, visible)
					}
					releaseOnce.Do(func() { close(release) })
					if err := authorityScopeDone(test, ctx, first); err != nil {
						test.Fatal(err)
					}
					secondErr := authorityScopeDone(test, ctx, second)
					after := fixture.image(test)
					auditDelta := 1
					if order == "revoke-first" {
						if !errors.Is(secondErr, domain.ErrForbidden) {
							test.Fatal("Complete owner retained authority after a prior revocation", secondErr)
						}
						withoutAudit := after
						withoutAudit.audit = before.audit
						if !reflect.DeepEqual(withoutAudit, before) {
							test.Fatal("denied actual owner changed ref, captures, revision, pending, outbox or sessions", before, after)
						}
					} else {
						if secondErr != nil {
							test.Fatal(secondErr)
						}
						auditDelta++
						if after.revision.Graph != before.revision.Graph+1 || after.revision.Pending != before.revision.Pending || after.revision.Evidence != before.revision.Evidence || len(after.reflog) != len(before.reflog)+1 || len(after.notifications) != len(before.notifications)+1 {
							test.Fatal("authorized owner did not commit one publication before revocation", before, after)
						}
						ref, err := fixture.storage.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
						if err != nil || ref.Target != fixture.tip {
							test.Fatal("write ordered ahead of revocation was lost", ref, err)
						}
						if err := write(); !errors.Is(err, domain.ErrForbidden) {
							test.Fatal("the same detached owner cached pre-revocation authority", err)
						}
						if current := fixture.image(test); !reflect.DeepEqual(current, after) {
							test.Fatal("later denial emitted partial owner effects", after, current)
						}
					}
					if len(after.audit) != len(before.audit)+auditDelta {
						test.Fatal("authority and reference command audit boundaries changed", before.audit, after.audit)
					}
				})
			}
		}
	}
}

func TestPGRepositoryRefOwnerEmptyRepairKeepsReachabilityAndNewerPending(test *testing.T) {
	for _, mode := range []string{"repair", "advanced-pointer"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newRepositoryRefFixturePG(test)
			setup := repositoryRefOwnerPG(test, NewService(fixture.storage, fixture.storage, nil, gitengine.NewEngine(fixture.storage), fixture.storage))
			if _, err := setup.UpdateRef(repositoryRefActorPG(fixture), inbound.UpdateRefInput{RepoID: fixture.repo,
				Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: fixture.orphan}, ExpectedTarget: fixture.base, Append: true}); err != nil {
				test.Fatal(err)
			}
			natural := collaborationSnapshot(test, fixture.storage, fixture.repo, "natural child above actual graft", fixture.orphan)
			if err := fixture.storage.CompareAndSwapRef(fixture.ctx, fixture.repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: natural}, fixture.orphan); err != nil {
				test.Fatal(err)
			}
			for _, pending := range []domain.Pending{
				{RepoID: fixture.repo, SessionID: "graft-base", Target: fixture.base},
				{RepoID: fixture.repo, SessionID: "incoming", Target: fixture.orphan},
				{RepoID: fixture.repo, SessionID: "natural", Target: natural},
			} {
				if err := fixture.storage.PutPending(fixture.ctx, fixture.repo, pending); err != nil {
					test.Fatal(err)
				}
			}
			probe := &repositoryRefProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
			if mode == "advanced-pointer" {
				probe.replaceSession, probe.replacement = "graft-base", fixture.loose
			}
			owner := repositoryRefProbeOwnerPG(test, probe)
			for attempt := range 2 {
				before := fixture.image(test)
				output, err := owner.UpdateRefs(repositoryRefActorPG(fixture), inbound.UpdateRefsInput{RepoID: fixture.repo})
				if err != nil || output.Applied != 0 || probe.calls["pending-list"] != attempt+1 || probe.calls["revision"] != attempt+1 || probe.calls["audit"] != attempt+1 || probe.transactions != attempt+1 {
					test.Fatal("empty retry did not use exactly one existing scope and reconciliation", output, probe.calls, probe.transactions, err)
				}
				after := fixture.image(test)
				if after.revision.Graph != before.revision.Graph || after.revision.Evidence != before.revision.Evidence || after.revision.Pending != before.revision.Pending+1 || len(after.audit) != len(before.audit)+1 {
					test.Fatal("empty recovery published graph changes or split audit/revision", before, after)
				}
				expected := 1
				if mode == "advanced-pointer" {
					expected++
				}
				if len(after.pending) != expected {
					test.Fatal("repair ignored natural/graft reachability or deleted a newer pointer", after.pending)
				}
				for _, pending := range after.pending {
					if pending.Target != fixture.loose || pending.SessionID != "unshared" && (mode != "advanced-pointer" || pending.SessionID != "graft-base") {
						test.Fatal("target-qualified repair changed an unrelated capture", pending)
					}
				}
				after.pending, after.revision, after.audit = before.pending, before.revision, before.audit
				if !reflect.DeepEqual(after, before) {
					test.Fatal("empty retry removed documents, snapshots, session refs or login sessions", before, after)
				}
			}
			if probe.calls["pending-delete"] != 3 || probe.calls["cas"] != 0 || probe.calls["append"] != 0 || probe.calls["outbox"] != 0 {
				test.Fatal("empty retry repeated deletes or ref publication instead of pending-only repair", probe.calls)
			}
		})
	}
}

func TestPGRepositoryRefOwnerPromotionSharesOriginalScopeAndPreservesMainRewind(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, true)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	probe := &repositoryRefProbePG{PostgresStore: fixture.storage, calls: make(map[string]int)}
	worker := newPRPromotionPGOwner(test, probe, probe, probe)
	scope, ok := worker.refs.scope.(*repositoryRefScope)
	if !ok || scope == nil || scope.work != worker.work || worker.refs.profile != RepositoryRefComplete {
		test.Fatal("actual PR consumer constructed a parallel reference scope", worker.refs, worker.work)
	}
	owner := repositoryRefOwnerPG(test, fixture.service)
	before, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	done, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
	if err != nil || done.State != "completed" || probe.transactions != 1 || probe.calls["append"] != 1 || probe.calls["revision"] != 1 || probe.calls["pending-list"] != 1 {
		test.Fatal("actual worker did not consume one shared ref/PR write scope", done, probe.transactions, probe.calls, err)
	}
	after, err := fixture.publication(ctx, fixture.storage)
	if err != nil || after.revision.Graph != before.revision.Graph+1 {
		test.Fatal("nested promotion ref command split its outer revision", before, after, err)
	}
	bindings, completions := prPromotionPGReceipts(after.history, fixture.job.PR)
	if bindings != 1 || completions != 1 {
		test.Fatal("shared actual ref body omitted or duplicated immutable PR receipts", bindings, completions)
	}
	input := inbound.UpdateRefInput{RepoID: fixture.repo,
		Ref:            domain.Ref{Kind: domain.RefBranch, Name: "main", BranchID: fixture.job.BaseBranchID, Target: fixture.base},
		ExpectedTarget: fixture.tip, Force: true}
	output, err := owner.UpdateRef(ctx, input)
	if err != nil || output.Result != inbound.RefForced || output.Ref.Target != fixture.base {
		test.Fatal("actual detached Complete owner could not perform an explicit main rewind", output, err)
	}
	rewound, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	for range 2 {
		output, err := worker.promoteBoundPR(ctx, fixture.repo, fixture.job.PR, fixture.job.BaseBranchID)
		if err != nil || output.Result != inbound.RefUpToDate || output.Ref.Target != fixture.base {
			test.Fatal("completed PR replay did not preserve a later explicit main rewind", output, err)
		}
	}
	replayed, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	if replayed.revision.Graph < rewound.revision.Graph {
		test.Fatal("completed replay regressed repository revisions", rewound.revision, replayed.revision)
	}
	replayed.revision = rewound.revision
	if !reflect.DeepEqual(replayed, rewound) {
		test.Fatal("duplicate PR delivery moved main, changed ancestry/memory or duplicated outbox/receipts", rewound, replayed)
	}
}
