package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type refOwnerBoundKey struct{}

type refOwnerProbe struct {
	test   *testing.T
	repo   domain.ContentHash
	bound  context.Context
	active bool
	calls  []string
}

func (probe *refOwnerProbe) touch(ctx context.Context, repo domain.ContentHash, operation string) {
	probe.test.Helper()
	if !probe.active || ctx != probe.bound || ctx.Value(refOwnerBoundKey{}) != probe || repo != probe.repo {
		probe.test.Fatalf("%s escaped the original repository scope: repo=%s active=%v", operation, repo, probe.active)
	}
	probe.calls = append(probe.calls, operation)
}

func (probe *refOwnerProbe) count(operation string) int {
	count := 0
	for _, actual := range probe.calls {
		if actual == operation {
			count++
		}
	}
	return count
}

type refOwnerWrite struct {
	Ref      domain.Ref
	Expected domain.ContentHash
}

type refOwnerStore struct {
	probe        *refOwnerProbe
	repository   domain.Repo
	refs         map[string]domain.Ref
	snapshots    map[domain.ContentHash]domain.Snapshot
	pendings     []domain.Pending
	pendingNow   map[string]domain.ContentHash
	failures     map[string]error
	swaps        []refOwnerWrite
	lifecycles   []domain.Ref
	deletions    []domain.Pending
	beforeSwap   func()
	beforeDelete func()
}

func refOwnerKey(kind domain.RefKind, name string) string {
	return string(kind) + "/" + name
}

func (store *refOwnerStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	store.probe.touch(ctx, repo, "repo")
	return store.repository, store.failures["repo"]
}

func (store *refOwnerStore) GetSnapshot(ctx context.Context, repo, target domain.ContentHash) (domain.Snapshot, error) {
	store.probe.touch(ctx, repo, "snapshot")
	if err := store.failures["snapshot"]; err != nil {
		return domain.Snapshot{}, err
	}
	snapshot, exists := store.snapshots[target]
	if !exists {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return snapshot, nil
}

func (store *refOwnerStore) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	key := refOwnerKey(kind, name)
	store.probe.touch(ctx, repo, "ref:"+key)
	if err := store.failures["ref:"+key]; err != nil {
		return domain.Ref{}, err
	}
	ref, exists := store.refs[key]
	if !exists {
		return domain.Ref{}, domain.ErrNotFound
	}
	return ref, nil
}

func (store *refOwnerStore) CompareAndSwapRef(ctx context.Context, repo domain.ContentHash, next domain.Ref, expected domain.ContentHash) error {
	store.probe.touch(ctx, repo, "cas")
	store.swaps = append(store.swaps, refOwnerWrite{Ref: next, Expected: expected})
	if store.beforeSwap != nil {
		store.beforeSwap()
	}
	if err := store.failures["cas"]; err != nil {
		return err
	}
	key := refOwnerKey(next.Kind, next.Name)
	if store.refs[key].Target != expected {
		return domain.ErrRefConflict
	}
	store.refs[key] = next
	return nil
}

func (store *refOwnerStore) ApplyBranchLifecycleRef(ctx context.Context, repo domain.ContentHash, ref domain.Ref) error {
	store.probe.touch(ctx, repo, "lifecycle")
	store.lifecycles = append(store.lifecycles, ref)
	if err := store.failures["lifecycle"]; err != nil {
		return err
	}
	store.refs[refOwnerKey(ref.Kind, ref.Name)] = ref
	return nil
}

func (store *refOwnerStore) ListPendings(ctx context.Context, repo domain.ContentHash) ([]domain.Pending, error) {
	store.probe.touch(ctx, repo, "pendings")
	return append([]domain.Pending(nil), store.pendings...), store.failures["pendings"]
}

func (store *refOwnerStore) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	store.probe.touch(ctx, repo, "refs")
	refs := make([]domain.Ref, 0, len(store.refs))
	for _, ref := range store.refs {
		refs = append(refs, ref)
	}
	return refs, store.failures["refs"]
}

func (store *refOwnerStore) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	store.probe.touch(ctx, repo, "snapshots")
	if branch != "" {
		store.probe.test.Fatalf("pending reconciliation filtered snapshots by branch %q", branch)
	}
	snapshots := make([]domain.Snapshot, 0, len(store.snapshots))
	for _, snapshot := range store.snapshots {
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, store.failures["snapshots"]
}

func (store *refOwnerStore) CompareAndDeletePending(ctx context.Context, repo domain.ContentHash, session string, expected domain.ContentHash) (domain.PendingDeleteResult, error) {
	store.probe.touch(ctx, repo, "delete-pending")
	store.deletions = append(store.deletions, domain.Pending{SessionID: session, Target: expected})
	if store.beforeDelete != nil {
		store.beforeDelete()
	}
	if err := store.failures["delete-pending"]; err != nil {
		return domain.PendingDeleteKept, err
	}
	current, exists := store.pendingNow[session]
	if !exists {
		return domain.PendingDeleteAbsent, nil
	}
	if current != expected {
		return domain.PendingDeleteKept, nil
	}
	delete(store.pendingNow, session)
	return domain.PendingDeleteDeleted, nil
}

type refOwnerEngine struct {
	probe   *refOwnerProbe
	classes map[domain.ContentHash]outbound.RefMoveClass
	err     error
	moves   []refOwnerWrite
}

func (engine *refOwnerEngine) ClassifyRefMove(ctx context.Context, repo, previous, next domain.ContentHash) (outbound.RefMoveClass, error) {
	engine.probe.touch(ctx, repo, "classify")
	engine.moves = append(engine.moves, refOwnerWrite{Ref: domain.Ref{Target: next}, Expected: previous})
	if class, exists := engine.classes[next]; exists {
		return class, engine.err
	}
	return outbound.MoveFastForward, engine.err
}

type refOwnerScopeFrame struct {
	Batch    bool
	Empty    bool
	Audit    string
	Revision string
}

type refOwnerScope struct {
	probe     *refOwnerProbe
	frames    []refOwnerScopeFrame
	beforeErr error
	afterErr  error
}

func (scope *refOwnerScope) enter(ctx context.Context, repo domain.ContentHash, batch, empty bool) context.Context {
	scope.probe.test.Helper()
	if scope.probe.active || repo != scope.probe.repo {
		scope.probe.test.Fatal("ref commands nested or changed repository scope")
	}
	audit, revision := "context.ref.updated", "graph"
	if batch {
		audit = "context.refs.updated"
	}
	if empty {
		revision = "pending"
	}
	bound := context.WithValue(auditOperation(ctx, audit), revisionScopeKey{}, revision)
	bound = context.WithValue(bound, refOwnerBoundKey{}, scope.probe)
	scope.frames = append(scope.frames, refOwnerScopeFrame{Batch: batch, Empty: empty, Audit: audit, Revision: revision})
	scope.probe.bound, scope.probe.active = bound, true
	return bound
}

func (scope *refOwnerScope) WithinRefWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.UpdateRefOutput, error)) (inbound.UpdateRefOutput, error) {
	bound := scope.enter(ctx, repo, false, false)
	defer func() { scope.probe.active = false }()
	if scope.beforeErr != nil {
		return inbound.UpdateRefOutput{}, scope.beforeErr
	}
	out, err := write(bound)
	if err == nil {
		err = scope.afterErr
	}
	return out, err
}

func (scope *refOwnerScope) WithinRefsWrite(ctx context.Context, repo domain.ContentHash, empty bool, write func(context.Context) (inbound.UpdateRefsOutput, error)) (inbound.UpdateRefsOutput, error) {
	bound := scope.enter(ctx, repo, true, empty)
	defer func() { scope.probe.active = false }()
	if scope.beforeErr != nil {
		return inbound.UpdateRefsOutput{}, scope.beforeErr
	}
	out, err := write(bound)
	if err == nil {
		err = scope.afterErr
	}
	return out, err
}

type refOwnerHistory struct {
	probe  *refOwnerProbe
	events []domain.HistoryEvent
	err    error
}

func (history *refOwnerHistory) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	history.probe.touch(ctx, repo, "history")
	return history.events, history.err
}

type refOwnerAppend struct {
	probe  *refOwnerProbe
	store  *refOwnerStore
	writes []refOwnerWrite
	err    error
}

func (appendPort *refOwnerAppend) AppendRef(ctx context.Context, repo domain.ContentHash, next domain.Ref, expected domain.ContentHash) error {
	appendPort.probe.touch(ctx, repo, "append")
	appendPort.writes = append(appendPort.writes, refOwnerWrite{Ref: next, Expected: expected})
	if appendPort.err != nil {
		return appendPort.err
	}
	key := refOwnerKey(next.Kind, next.Name)
	if appendPort.store.refs[key].Target != expected {
		return domain.ErrRefConflict
	}
	appendPort.store.refs[key] = next
	return nil
}

type refOwnerRevisions struct {
	probe    *refOwnerProbe
	revision domain.RepositoryRevision
	err      error
}

func (revisions *refOwnerRevisions) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	revisions.probe.touch(ctx, repo, "revision")
	return revisions.revision, revisions.err
}

type refOwnerNotice struct {
	Ref     domain.Ref
	Forced  bool
	Created bool
}

type refOwnerNotifications struct {
	probe   *refOwnerProbe
	notices []refOwnerNotice
	err     error
}

func (notifications *refOwnerNotifications) NotifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	notifications.probe.touch(ctx, repo, "notify")
	notifications.notices = append(notifications.notices, refOwnerNotice{Ref: ref, Forced: forced, Created: created})
	return notifications.err
}

type refOwnerFixture struct {
	probe         *refOwnerProbe
	store         *refOwnerStore
	engine        *refOwnerEngine
	scope         *refOwnerScope
	history       *refOwnerHistory
	appendPort    *refOwnerAppend
	revisions     *refOwnerRevisions
	notifications *refOwnerNotifications
	base          domain.ContentHash
	next          domain.ContentHash
}

func newRefOwnerFixture(test *testing.T) *refOwnerFixture {
	test.Helper()
	repo, base, next := hh("ref-owner-repo"), hh("ref-owner-base"), hh("ref-owner-next")
	probe := &refOwnerProbe{test: test, repo: repo}
	storage := &refOwnerStore{
		probe: probe, repository: domain.Repo{ID: repo, DefaultBranch: "main"},
		refs: map[string]domain.Ref{}, snapshots: map[domain.ContentHash]domain.Snapshot{},
		pendingNow: map[string]domain.ContentHash{}, failures: map[string]error{},
	}
	storage.refs[refOwnerKey(domain.RefBranch, "topic")] = domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "topic", BranchID: "topic-identity", Target: base}
	storage.snapshots[base] = domain.Snapshot{ID: base, RepoID: repo, DocHash: base, Branch: "topic"}
	storage.snapshots[next] = domain.Snapshot{ID: next, RepoID: repo, DocHash: next, Branch: "topic", Parents: []domain.ContentHash{base}}
	return &refOwnerFixture{
		probe: probe, store: storage, engine: &refOwnerEngine{probe: probe, classes: map[domain.ContentHash]outbound.RefMoveClass{}},
		scope: &refOwnerScope{probe: probe}, history: &refOwnerHistory{probe: probe},
		appendPort:    &refOwnerAppend{probe: probe, store: storage},
		revisions:     &refOwnerRevisions{probe: probe, revision: domain.RepositoryRevision{Graph: 17, Evidence: 23, Pending: 31}},
		notifications: &refOwnerNotifications{probe: probe}, base: base, next: next,
	}
}

func (fixture *refOwnerFixture) config() RepositoryRefCommandsConfig {
	return RepositoryRefCommandsConfig{
		Profile: RepositoryRefComplete, Store: fixture.store, Engine: fixture.engine, Scope: fixture.scope,
		History: fixture.history, Append: fixture.appendPort, Revisions: fixture.revisions,
		Notifications: fixture.notifications, Transactional: true,
	}
}

func (fixture *refOwnerFixture) input() inbound.UpdateRefInput {
	return inbound.UpdateRefInput{
		RepoID: fixture.probe.repo,
		Ref:    domain.Ref{Kind: domain.RefBranch, Name: "topic", BranchID: "topic-identity", Target: fixture.next},
	}
}

func refOwnerCommands(test *testing.T, config RepositoryRefCommandsConfig) *RepositoryRefCommands {
	test.Helper()
	commands, err := NewRepositoryRefCommands(config)
	if err != nil {
		test.Fatalf("construct ref owner: %v", err)
	}
	return commands
}

func refOwnerAssertZero(test *testing.T, actual inbound.UpdateRefOutput) {
	test.Helper()
	if !reflect.DeepEqual(actual, inbound.UpdateRefOutput{}) {
		test.Fatalf("error exposed success output: %+v", actual)
	}
}

var _ RepositoryRefStore = (*refOwnerStore)(nil)
var _ RepositoryRefEngine = (*refOwnerEngine)(nil)
var _ RepositoryRefWriteScope = (*refOwnerScope)(nil)
var _ RepositoryRefHistory = (*refOwnerHistory)(nil)
var _ RepositoryRefAppend = (*refOwnerAppend)(nil)
var _ RepositoryRefRevisions = (*refOwnerRevisions)(nil)
var _ RepositoryRefNotifications = (*refOwnerNotifications)(nil)
var _ inbound.RepositoryRefCommands = (*RepositoryRefCommands)(nil)
var _ *repositoryRefCommands = (*RepositoryRefCommands)(nil)

func TestRepositoryRefOwnerConstructorIsExplicitAndDoesNoIO(test *testing.T) {
	changes := []struct {
		name   string
		change func(*RepositoryRefCommandsConfig)
		want   error
	}{
		{name: "complete"},
		{name: "compatibility", change: func(config *RepositoryRefCommandsConfig) {
			config.Profile = RepositoryRefCompatibility
			config.Engine, config.History, config.Append, config.Revisions, config.Notifications = nil, nil, nil, nil, nil
			config.Transactional = false
		}},
		{name: "unknown profile", change: func(config *RepositoryRefCommandsConfig) { config.Profile = "unknown" }, want: domain.ErrValidation},
		{name: "missing store", change: func(config *RepositoryRefCommandsConfig) { config.Store = nil }, want: domain.ErrValidation},
		{name: "missing scope", change: func(config *RepositoryRefCommandsConfig) { config.Scope = nil }, want: domain.ErrValidation},
		{name: "typed nil store", change: func(config *RepositoryRefCommandsConfig) { config.Store = (*refOwnerStore)(nil) }, want: domain.ErrValidation},
		{name: "typed nil scope", change: func(config *RepositoryRefCommandsConfig) { config.Scope = (*refOwnerScope)(nil) }, want: domain.ErrValidation},
		{name: "typed nil engine", change: func(config *RepositoryRefCommandsConfig) { config.Engine = (*refOwnerEngine)(nil) }, want: domain.ErrValidation},
		{name: "typed nil history", change: func(config *RepositoryRefCommandsConfig) { config.History = (*refOwnerHistory)(nil) }, want: domain.ErrValidation},
		{name: "typed nil append", change: func(config *RepositoryRefCommandsConfig) { config.Append = (*refOwnerAppend)(nil) }, want: domain.ErrValidation},
		{name: "typed nil revisions", change: func(config *RepositoryRefCommandsConfig) { config.Revisions = (*refOwnerRevisions)(nil) }, want: domain.ErrValidation},
		{name: "typed nil notifications", change: func(config *RepositoryRefCommandsConfig) { config.Notifications = (*refOwnerNotifications)(nil) }, want: domain.ErrValidation},
		{name: "typed nil history after absent engine", change: func(config *RepositoryRefCommandsConfig) {
			config.Engine = nil
			config.History = (*refOwnerHistory)(nil)
		}, want: domain.ErrValidation},
		{name: "typed nil notifications after absent capabilities", change: func(config *RepositoryRefCommandsConfig) {
			config.Engine, config.History, config.Append, config.Revisions = nil, nil, nil, nil
			config.Notifications = (*refOwnerNotifications)(nil)
		}, want: domain.ErrValidation},
		{name: "missing complete engine", change: func(config *RepositoryRefCommandsConfig) { config.Engine = nil }, want: domain.ErrCatalogUnsupported},
		{name: "missing complete history", change: func(config *RepositoryRefCommandsConfig) { config.History = nil }, want: domain.ErrCatalogUnsupported},
		{name: "missing complete append", change: func(config *RepositoryRefCommandsConfig) { config.Append = nil }, want: domain.ErrCatalogUnsupported},
		{name: "missing complete revisions", change: func(config *RepositoryRefCommandsConfig) { config.Revisions = nil }, want: domain.ErrCatalogUnsupported},
		{name: "missing complete notifications", change: func(config *RepositoryRefCommandsConfig) { config.Notifications = nil }, want: domain.ErrCatalogUnsupported},
	}
	for _, scenario := range changes {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			config := fixture.config()
			if scenario.change != nil {
				scenario.change(&config)
			}
			commands, err := NewRepositoryRefCommands(config)
			if !errors.Is(err, scenario.want) || (commands == nil) != (scenario.want != nil) {
				test.Fatalf("constructor owner=%v error=%v want=%v", commands, err, scenario.want)
			}
			if len(fixture.probe.calls) != 0 || len(fixture.scope.frames) != 0 {
				test.Fatalf("constructor performed IO: calls=%v scopes=%v", fixture.probe.calls, fixture.scope.frames)
			}
			if strings.HasPrefix(scenario.name, "typed nil") {
				config.Profile = RepositoryRefCompatibility
				if commands, err := NewRepositoryRefCommands(config); commands != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatalf("compatibility accepted typed nil: owner=%v error=%v", commands, err)
				}
			}
		})
	}
	if commands, err := NewRepositoryRefCommands(RepositoryRefCommandsConfig{}); commands != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatalf("zero config owner=%v error=%v", commands, err)
	}
	var nilOwner *RepositoryRefCommands
	if out, err := nilOwner.UpdateRef(context.Background(), inbound.UpdateRefInput{}); !errors.Is(err, domain.ErrValidation) {
		test.Fatalf("nil owner single output=%+v error=%v", out, err)
	} else {
		refOwnerAssertZero(test, out)
	}
	if out, err := nilOwner.UpdateRefs(context.Background(), inbound.UpdateRefsInput{}); !errors.Is(err, domain.ErrValidation) || out != (inbound.UpdateRefsOutput{}) {
		test.Fatalf("nil owner batch output=%+v error=%v", out, err)
	}
}

func TestRepositoryRefOwnerActualMovePolicyUsesObservedCAS(test *testing.T) {
	for _, scenario := range []struct {
		name       string
		class      outbound.RefMoveClass
		force      bool
		appendMove bool
		created    bool
		upToDate   bool
		want       inbound.RefUpdateResult
		wantErr    error
	}{
		{name: "fast forward", class: outbound.MoveFastForward, want: inbound.RefFastForward},
		{name: "new branch", class: outbound.MoveFastForward, created: true, want: inbound.RefFastForward},
		{name: "up to date", class: outbound.MoveUpToDate, upToDate: true, want: inbound.RefUpToDate},
		{name: "force", force: true, want: inbound.RefForced},
		{name: "force new branch", force: true, created: true, want: inbound.RefForced},
		{name: "force echo", force: true, upToDate: true, want: inbound.RefUpToDate},
		{name: "diverged rejects", class: outbound.MoveDiverged, wantErr: domain.ErrNonFastForward},
		{name: "behind rejects", class: outbound.MoveNonFastForward, wantErr: domain.ErrNonFastForward},
		{name: "diverged append", class: outbound.MoveDiverged, appendMove: true, want: inbound.RefAppended},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			input := fixture.input()
			input.Force, input.Append = scenario.force, scenario.appendMove
			observed := fixture.base
			if scenario.created {
				delete(fixture.store.refs, refOwnerKey(input.Ref.Kind, input.Ref.Name))
				observed = ""
			}
			if scenario.upToDate {
				input.Ref.Target = fixture.base
			}
			fixture.engine.classes[input.Ref.Target] = scenario.class
			out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
			if !errors.Is(err, scenario.wantErr) {
				test.Fatalf("move error=%v want=%v", err, scenario.wantErr)
			}
			if scenario.wantErr != nil {
				refOwnerAssertZero(test, out)
				if len(fixture.store.swaps)+len(fixture.appendPort.writes)+len(fixture.notifications.notices) != 0 {
					test.Fatal("rejected ancestry move reached a mutation")
				}
			} else {
				expectedRef := input.Ref
				expectedRef.RepoID = input.RepoID
				want := inbound.UpdateRefOutput{Ref: expectedRef, RequestedTarget: input.Ref.Target, ServerTarget: input.Ref.Target, Result: scenario.want}
				if !reflect.DeepEqual(out, want) {
					test.Fatalf("move output=%+v want=%+v", out, want)
				}
				if scenario.want != inbound.RefUpToDate {
					wantWrite := []refOwnerWrite{{Ref: expectedRef, Expected: observed}}
					if scenario.appendMove {
						if !reflect.DeepEqual(fixture.appendPort.writes, wantWrite) || len(fixture.store.swaps) != 0 {
							test.Fatalf("append writes=%+v CAS=%+v want=%+v", fixture.appendPort.writes, fixture.store.swaps, wantWrite)
						}
					} else if !reflect.DeepEqual(fixture.store.swaps, wantWrite) || len(fixture.appendPort.writes) != 0 {
						test.Fatalf("CAS=%+v append=%+v want=%+v", fixture.store.swaps, fixture.appendPort.writes, wantWrite)
					}
					wantNotice := []refOwnerNotice{{Ref: expectedRef, Forced: scenario.force, Created: scenario.created}}
					if !reflect.DeepEqual(fixture.notifications.notices, wantNotice) {
						test.Fatalf("notices=%+v want=%+v", fixture.notifications.notices, wantNotice)
					}
				} else if len(fixture.store.swaps)+len(fixture.appendPort.writes)+len(fixture.notifications.notices) != 0 {
					test.Fatal("up-to-date echo mutated state")
				}
			}
			wantClassifications := 1
			if scenario.force {
				wantClassifications = 0
			}
			if len(fixture.engine.moves) != wantClassifications {
				test.Fatalf("classifications=%+v want count=%d", fixture.engine.moves, wantClassifications)
			}
			if wantClassifications == 1 && (fixture.engine.moves[0].Expected != observed || fixture.engine.moves[0].Ref.Target != input.Ref.Target) {
				test.Fatalf("classifier did not receive observed targets: %+v", fixture.engine.moves)
			}
			if len(fixture.scope.frames) != 1 || fixture.probe.count("pendings") != boolRefOwnerCount(scenario.wantErr == nil) {
				test.Fatalf("scopes=%v calls=%v", fixture.scope.frames, fixture.probe.calls)
			}
		})
	}
}

func boolRefOwnerCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestRepositoryRefOwnerReservedAndImmutablePointers(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		kind     domain.RefKind
		refName  string
		existing bool
		echo     bool
		force    bool
		want     inbound.RefUpdateResult
		wantErr  error
	}{
		{name: "tag creation", kind: domain.RefTag, refName: "v1", want: inbound.RefFastForward},
		{name: "tag echo", kind: domain.RefTag, refName: "v1", existing: true, echo: true, want: inbound.RefUpToDate},
		{name: "tag immutable", kind: domain.RefTag, refName: "v1", existing: true, wantErr: domain.ErrNonFastForward},
		{name: "tag explicit force", kind: domain.RefTag, refName: "v1", existing: true, force: true, want: inbound.RefForced},
		{name: "history cannot create", kind: domain.RefTag, refName: "cxt/history/v1/retained", wantErr: domain.ErrForbidden},
		{name: "history echo", kind: domain.RefTag, refName: "cxt/history/v1/retained", existing: true, echo: true, want: inbound.RefUpToDate},
		{name: "history cannot force", kind: domain.RefTag, refName: "cxt/history/v1/retained", existing: true, force: true, wantErr: domain.ErrForbidden},
		{name: "session cannot create", kind: domain.RefSession, refName: domain.SessionRefPrefix("topic") + "capture", wantErr: domain.ErrForbidden},
		{name: "session echo", kind: domain.RefSession, refName: domain.SessionRefPrefix("topic") + "capture", existing: true, echo: true, want: inbound.RefUpToDate},
		{name: "session cannot force", kind: domain.RefSession, refName: domain.SessionRefPrefix("topic") + "capture", existing: true, force: true, wantErr: domain.ErrForbidden},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			input := fixture.input()
			input.Ref.Kind, input.Ref.Name, input.Ref.BranchID = scenario.kind, scenario.refName, ""
			input.Force = scenario.force
			if scenario.echo {
				input.Ref.Target = fixture.base
			}
			if scenario.existing {
				current := input.Ref
				current.RepoID, current.Target = input.RepoID, fixture.base
				fixture.store.refs[refOwnerKey(current.Kind, current.Name)] = current
			}
			out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
			if !errors.Is(err, scenario.wantErr) {
				test.Fatalf("error=%v want=%v", err, scenario.wantErr)
			}
			wantCAS := boolRefOwnerCount(scenario.want == inbound.RefFastForward || scenario.want == inbound.RefForced)
			if len(fixture.store.swaps) != wantCAS || len(fixture.engine.moves)+len(fixture.appendPort.writes)+len(fixture.store.lifecycles) != 0 {
				test.Fatalf("unexpected writes/classification: CAS=%v calls=%v", fixture.store.swaps, fixture.probe.calls)
			}
			if err != nil {
				refOwnerAssertZero(test, out)
			} else {
				ref := input.Ref
				ref.RepoID = input.RepoID
				want := inbound.UpdateRefOutput{Ref: ref, RequestedTarget: ref.Target, ServerTarget: ref.Target, Result: scenario.want}
				if !reflect.DeepEqual(out, want) {
					test.Fatalf("output=%+v want=%+v", out, want)
				}
				if wantCAS == 1 {
					expected := domain.ContentHash("")
					if scenario.existing {
						expected = fixture.base
					}
					if !reflect.DeepEqual(fixture.store.swaps, []refOwnerWrite{{Ref: ref, Expected: expected}}) {
						test.Fatalf("CAS did not preserve exact observation: %+v", fixture.store.swaps)
					}
				}
			}
			if fixture.probe.count("notify") != boolRefOwnerCount(scenario.want == inbound.RefForced) || fixture.probe.count("pendings") != 0 {
				test.Fatalf("reserved pointer triggered extra work: %v", fixture.probe.calls)
			}
		})
	}
}

func TestRepositoryRefOwnerProtectedBranchAndLifecycle(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		lifecycle bool
		protected bool
		force     bool
		appendRef bool
		wantErr   error
	}{
		{name: "protected force", protected: true, force: true, wantErr: domain.ErrForbidden},
		{name: "protected append preserves history", protected: true, appendRef: true},
		{name: "protected archive", lifecycle: true, protected: true, wantErr: domain.ErrForbidden},
		{name: "unprotected archive", lifecycle: true},
		{name: "lifecycle cannot force", lifecycle: true, force: true, wantErr: domain.ErrValidation},
		{name: "lifecycle cannot append", lifecycle: true, appendRef: true, wantErr: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			fixture.store.repository.DefaultBranch, fixture.store.repository.ProtectDefault = "topic", scenario.protected
			input := fixture.input()
			input.Force, input.Append = scenario.force, scenario.appendRef
			fixture.engine.classes[fixture.next] = outbound.MoveDiverged
			if scenario.lifecycle {
				var err error
				input.Ref, err = domain.NewBranchLifecycleRef(input.RepoID, "topic", fixture.next, 1, domain.BranchArchived)
				if err != nil {
					test.Fatal(err)
				}
			}
			out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
			if !errors.Is(err, scenario.wantErr) {
				test.Fatalf("error=%v want=%v", err, scenario.wantErr)
			}
			if err != nil {
				refOwnerAssertZero(test, out)
				if len(fixture.store.swaps)+len(fixture.store.lifecycles)+len(fixture.appendPort.writes)+len(fixture.notifications.notices) != 0 {
					test.Fatal("protected or immutable request reached a write")
				}
			} else if scenario.lifecycle {
				if out.Result != inbound.RefFastForward || !reflect.DeepEqual(fixture.store.lifecycles, []domain.Ref{input.Ref}) || len(fixture.store.swaps) != 0 {
					test.Fatalf("archive output=%+v lifecycle=%+v CAS=%v", out, fixture.store.lifecycles, fixture.store.swaps)
				}
			} else if out.Result != inbound.RefAppended || len(fixture.appendPort.writes) != 1 || fixture.appendPort.writes[0].Expected != fixture.base {
				test.Fatalf("protected append output=%+v writes=%v", out, fixture.appendPort.writes)
			}
			if fixture.probe.count("pendings") != boolRefOwnerCount(err == nil) {
				test.Fatalf("pending reconciliation calls=%v", fixture.probe.calls)
			}
		})
	}
}

func TestRepositoryRefOwnerNeverRetriesAStaleHead(test *testing.T) {
	for _, scenario := range []string{"stale force observation", "CAS race", "CAS capability error", "idempotent stale echo"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			input := fixture.input()
			input.ExpectedTarget = fixture.base
			wantErr := error(domain.ErrRefConflict)
			wantCAS, wantReads := 1, 1
			switch scenario {
			case "stale force observation":
				input.ExpectedTarget, input.Force = hh("stale-observation"), true
				wantCAS = 0
			case "CAS race":
				fixture.store.beforeSwap = func() {
					current := fixture.store.refs[refOwnerKey(input.Ref.Kind, input.Ref.Name)]
					current.Target = hh("concurrent-new-head")
					fixture.store.refs[refOwnerKey(input.Ref.Kind, input.Ref.Name)] = current
				}
			case "CAS capability error":
				fixture.store.failures["cas"] = domain.ErrCatalogUnsupported
				wantErr = domain.ErrCatalogUnsupported
			case "idempotent stale echo":
				input.ExpectedTarget, input.Ref.Target, input.Force = hh("stale-observation"), fixture.base, true
				wantErr, wantCAS = nil, 0
			}
			out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
			if !errors.Is(err, wantErr) || len(fixture.store.swaps) != wantCAS || fixture.probe.count("ref:"+refOwnerKey(input.Ref.Kind, input.Ref.Name)) != wantReads {
				test.Fatalf("out=%+v error=%v swaps=%v calls=%v", out, err, fixture.store.swaps, fixture.probe.calls)
			}
			if err != nil {
				refOwnerAssertZero(test, out)
			} else if out.Result != inbound.RefUpToDate || out.ServerTarget != fixture.base {
				test.Fatalf("already applied target was not an echo: %+v", out)
			}
			if wantCAS == 1 && fixture.store.swaps[0].Expected != fixture.base {
				test.Fatalf("CAS expectation rewritten: %+v", fixture.store.swaps)
			}
			if len(fixture.appendPort.writes)+len(fixture.notifications.notices) != 0 {
				test.Fatal("CAS/observation failure fell back to append or notification")
			}
		})
	}
}

func TestRepositoryRefOwnerConditionalAppendChecksBothRevisionAndObservation(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*refOwnerFixture, *RepositoryRefCommandsConfig, *inbound.UpdateRefInput)
		want   error
	}{
		{name: "matching graph and evidence"},
		{name: "pending revision is independent", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.ExpectedRevision.Pending++
		}},
		{name: "graph changed", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.ExpectedRevision.Graph++
		}, want: domain.ErrRefConflict},
		{name: "evidence changed", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.ExpectedRevision.Evidence++
		}, want: domain.ErrRefConflict},
		{name: "observed target changed", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.ExpectedTarget = hh("stale-conditional")
		}, want: domain.ErrRefConflict},
		{name: "matching requested target cannot hide stale observation", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.Ref.Target, input.ExpectedTarget = fixture.base, hh("stale-conditional")
		}, want: domain.ErrRefConflict},
		{name: "branch identity changed", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.Ref.BranchID = "reused-name"
		}, want: domain.ErrRefConflict},
		{name: "legacy observed identity", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			key := refOwnerKey(input.Ref.Kind, input.Ref.Name)
			current := fixture.store.refs[key]
			current.BranchID = ""
			fixture.store.refs[key] = current
			input.Ref.BranchID = domain.LegacyContextBranchID(string(input.RepoID), input.Ref.Name)
		}},
		{name: "requires transactions", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			config.Transactional = false
		}, want: domain.ErrConflict},
		{name: "requires append", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.Append = false
		}, want: domain.ErrValidation},
		{name: "cannot force", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.Force = true
		}, want: domain.ErrValidation},
		{name: "requires identity", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.Ref.BranchID = ""
		}, want: domain.ErrValidation},
		{name: "requires expected target", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.ExpectedTarget = ""
		}, want: domain.ErrIntegrity},
		{name: "requires branch", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			input.Ref.Kind = domain.RefTag
		}, want: domain.ErrValidation},
		{name: "revision capability error is authoritative", change: func(fixture *refOwnerFixture, config *RepositoryRefCommandsConfig, input *inbound.UpdateRefInput) {
			fixture.revisions.err = domain.ErrCatalogUnsupported
		}, want: domain.ErrCatalogUnsupported},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			config, input := fixture.config(), fixture.input()
			revision := fixture.revisions.revision
			input.ExpectedRevision, input.ExpectedTarget, input.Append = &revision, fixture.base, true
			fixture.engine.classes[fixture.next] = outbound.MoveDiverged
			if scenario.change != nil {
				scenario.change(fixture, &config, &input)
			}
			out, err := refOwnerCommands(test, config).UpdateRef(context.Background(), input)
			if !errors.Is(err, scenario.want) {
				test.Fatalf("out=%+v error=%v want=%v", out, err, scenario.want)
			}
			if err != nil {
				refOwnerAssertZero(test, out)
				if len(fixture.store.swaps)+len(fixture.appendPort.writes)+len(fixture.notifications.notices) != 0 {
					test.Fatal("conditional rejection reached a write")
				}
			} else {
				ref := input.Ref
				ref.RepoID = input.RepoID
				want := inbound.UpdateRefOutput{Ref: ref, RequestedTarget: fixture.next, ServerTarget: fixture.next, Result: inbound.RefAppended}
				if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(fixture.appendPort.writes, []refOwnerWrite{{Ref: ref, Expected: fixture.base}}) || fixture.probe.count("revision") != 1 || len(fixture.store.swaps) != 0 {
					test.Fatalf("conditional out=%+v append=%v calls=%v", out, fixture.appendPort.writes, fixture.probe.calls)
				}
			}
		})
	}
}

func TestRepositoryRefOwnerContextProtocolUsesDurableBranchIdentity(test *testing.T) {
	for _, scenario := range []string{"identified continuation", "legacy continuation", "missing identity", "reused name", "birth required", "unknown protocol", "history error"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			fixture.store.repository.ContextProtocol = 1
			input := fixture.input()
			fixture.history.events = []domain.HistoryEvent{{ID: strings.Repeat("1", 32), RepoID: string(input.RepoID), Kind: "birth", Branch: "topic", BranchID: input.Ref.BranchID}}
			var wantErr error
			switch scenario {
			case "legacy continuation":
				fixture.history.events = nil
				input.Ref.BranchID = domain.LegacyContextBranchID(string(input.RepoID), "topic")
				current := fixture.store.refs[refOwnerKey(domain.RefBranch, "topic")]
				current.BranchID = input.Ref.BranchID
				fixture.store.refs[refOwnerKey(domain.RefBranch, "topic")] = current
			case "missing identity":
				input.Ref.BranchID, wantErr = "", domain.ErrConflict
			case "reused name":
				fixture.history.events = append(fixture.history.events,
					domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(input.RepoID), Kind: "rename", Branch: "moved", PreviousBranch: "topic", BranchID: input.Ref.BranchID, BindingParent: strings.Repeat("1", 32)},
					domain.HistoryEvent{ID: strings.Repeat("3", 32), RepoID: string(input.RepoID), Kind: "birth", Branch: "topic", BranchID: "replacement-identity", BindingParent: strings.Repeat("2", 32)},
				)
				wantErr = domain.ErrRefConflict
			case "birth required":
				fixture.history.events = nil
				delete(fixture.store.refs, refOwnerKey(domain.RefBranch, "topic"))
				input.Ref.BranchID = domain.LegacyContextBranchID(string(input.RepoID), "topic")
				wantErr = domain.ErrRefConflict
			case "unknown protocol":
				fixture.store.repository.ContextProtocol, wantErr = 2, domain.ErrConflict
			case "history error":
				fixture.history.err, wantErr = domain.ErrCatalogUnsupported, domain.ErrCatalogUnsupported
			}
			out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
			if !errors.Is(err, wantErr) {
				test.Fatalf("out=%+v error=%v want=%v", out, err, wantErr)
			}
			if err != nil {
				refOwnerAssertZero(test, out)
				if len(fixture.store.swaps)+len(fixture.engine.moves)+len(fixture.appendPort.writes) != 0 {
					test.Fatalf("identity rejection fell through: %v", fixture.probe.calls)
				}
			} else if out.Result != inbound.RefFastForward || out.Ref.BranchID != input.Ref.BranchID || out.Ref.Target != fixture.next || out.ForkedRef != nil || out.MergeBase != "" || len(fixture.history.events) != boolRefOwnerCount(scenario == "identified continuation") {
				test.Fatalf("continuation invented identity/history/fork: out=%+v events=%v", out, fixture.history.events)
			}
		})
	}
}

func TestRepositoryRefOwnerCompatibilityCapabilitiesAreLazyAndNeverFallback(test *testing.T) {
	for _, capability := range []string{"engine", "history", "append", "revisions", "notifications"} {
		for _, present := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/present=%v", capability, present), func(test *testing.T) {
				fixture := newRefOwnerFixture(test)
				config, input := fixture.config(), fixture.input()
				config.Profile, config.Transactional = RepositoryRefCompatibility, false
				switch capability {
				case "engine":
					if present {
						fixture.engine.err = domain.ErrCatalogUnsupported
					} else {
						config.Engine = nil
					}
				case "history":
					fixture.store.repository.ContextProtocol = 1
					if present {
						fixture.history.err = domain.ErrCatalogUnsupported
					} else {
						config.History = nil
					}
				case "append":
					input.Append = true
					fixture.engine.classes[fixture.next] = outbound.MoveDiverged
					if present {
						fixture.appendPort.err = domain.ErrCatalogUnsupported
					} else {
						config.Append = nil
					}
				case "revisions":
					config.Transactional, input.Append, input.ExpectedTarget = true, true, fixture.base
					revision := fixture.revisions.revision
					input.ExpectedRevision = &revision
					if present {
						fixture.revisions.err = domain.ErrCatalogUnsupported
					} else {
						config.Revisions = nil
					}
				case "notifications":
					if present {
						fixture.notifications.err = domain.ErrCatalogUnsupported
					} else {
						config.Notifications = nil
					}
				}
				out, err := refOwnerCommands(test, config).UpdateRef(context.Background(), input)
				if err == nil || (present && !errors.Is(err, domain.ErrCatalogUnsupported)) {
					test.Fatalf("capability failure concealed: out=%+v error=%v", out, err)
				}
				refOwnerAssertZero(test, out)
				wantCAS := boolRefOwnerCount(capability == "notifications")
				wantAppend := boolRefOwnerCount(capability == "append" && present)
				if len(fixture.store.swaps) != wantCAS || len(fixture.appendPort.writes) != wantAppend || fixture.probe.count("pendings") != 0 {
					test.Fatalf("capability failure retried or fell back: swaps=%v append=%v calls=%v", fixture.store.swaps, fixture.appendPort.writes, fixture.probe.calls)
				}
			})
		}
	}
	fixture := newRefOwnerFixture(test)
	config, input := fixture.config(), fixture.input()
	config.Profile = RepositoryRefCompatibility
	config.Engine, config.History, config.Append, config.Revisions, config.Notifications = nil, nil, nil, nil, nil
	input.Ref.Kind, input.Ref.Name, input.Ref.BranchID = domain.RefTag, "unused-capabilities", ""
	out, err := refOwnerCommands(test, config).UpdateRef(context.Background(), input)
	if err != nil || out.Result != inbound.RefFastForward || len(fixture.store.swaps) != 1 {
		test.Fatalf("unused optional ports blocked tag creation: out=%+v error=%v", out, err)
	}
}

func TestRepositoryRefOwnerCompleteErrorsClearSuccessOutputs(test *testing.T) {
	for _, scenario := range []string{"scope authorization", "scope completion", "notification", "pending reconciliation", "pending deletion", "target integrity", "snapshot capability", "ref capability"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			input := fixture.input()
			failure := errors.New("ref-owner injected failure")
			wantErr := error(failure)
			switch scenario {
			case "scope authorization":
				fixture.scope.beforeErr = failure
			case "scope completion":
				fixture.scope.afterErr = failure
			case "notification":
				fixture.notifications.err = failure
			case "pending reconciliation":
				fixture.store.failures["pendings"] = failure
			case "pending deletion":
				fixture.store.pendings = []domain.Pending{{SessionID: "reachable", Target: fixture.base}}
				fixture.store.pendingNow["reachable"] = fixture.base
				fixture.store.failures["delete-pending"] = failure
			case "target integrity":
				delete(fixture.store.snapshots, fixture.next)
				input.Force, wantErr = true, domain.ErrIntegrity
			case "snapshot capability":
				fixture.store.failures["snapshot"] = failure
			case "ref capability":
				fixture.store.failures["ref:"+refOwnerKey(domain.RefBranch, "topic")] = failure
			}
			out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
			if !errors.Is(err, wantErr) {
				test.Fatalf("error=%v want=%v", err, wantErr)
			}
			refOwnerAssertZero(test, out)
			if scenario == "scope authorization" && len(fixture.probe.calls) != 0 {
				test.Fatalf("denied scope invoked storage: %v", fixture.probe.calls)
			}
			if (scenario == "target integrity" || scenario == "snapshot capability" || scenario == "ref capability") && len(fixture.store.swaps)+len(fixture.appendPort.writes)+len(fixture.notifications.notices) != 0 {
				test.Fatal("failed read/integrity check reached a mutation")
			}
		})
	}
}

func TestRepositoryRefOwnerConditionalAtomicConflictDoesNotRetryOrFallback(test *testing.T) {
	fixture := newRefOwnerFixture(test)
	input := fixture.input()
	revision := fixture.revisions.revision
	input.Append, input.ExpectedTarget, input.ExpectedRevision = true, fixture.base, &revision
	fixture.engine.classes[fixture.next] = outbound.MoveDiverged
	fixture.appendPort.err = domain.ErrRefConflict
	out, err := refOwnerCommands(test, fixture.config()).UpdateRef(context.Background(), input)
	if !errors.Is(err, domain.ErrRefConflict) {
		test.Fatalf("conditional atomic error=%v", err)
	}
	refOwnerAssertZero(test, out)
	ref := input.Ref
	ref.RepoID = fixture.probe.repo
	if !reflect.DeepEqual(fixture.appendPort.writes, []refOwnerWrite{{Ref: ref, Expected: fixture.base}}) || len(fixture.store.swaps)+len(fixture.notifications.notices) != 0 || fixture.probe.count("revision") != 1 || fixture.probe.count("ref:"+refOwnerKey(ref.Kind, ref.Name)) != 1 || fixture.probe.count("pendings") != 0 {
		test.Fatalf("conditional atomic conflict retried or weakened: writes=%v calls=%v", fixture.appendPort.writes, fixture.probe.calls)
	}
}

func TestRepositoryRefOwnerBatchHasOneScopeAndReconcilesNaturalAndGraftParentsOnce(test *testing.T) {
	for _, empty := range []bool{false, true} {
		test.Run(fmt.Sprintf("empty=%v", empty), func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			graft, ancestor, privateTarget := hh("ref-owner-graft"), hh("ref-owner-graft-ancestor"), hh("ref-owner-private")
			root := fixture.store.snapshots[fixture.next]
			root.GraftParents = []domain.ContentHash{graft}
			fixture.store.snapshots[fixture.next] = root
			fixture.store.snapshots[graft] = domain.Snapshot{ID: graft, RepoID: fixture.probe.repo, DocHash: graft, Parents: []domain.ContentHash{ancestor}, GraftParents: []domain.ContentHash{fixture.base}}
			fixture.store.snapshots[ancestor] = domain.Snapshot{ID: ancestor, RepoID: fixture.probe.repo, DocHash: ancestor}
			fixture.store.snapshots[privateTarget] = domain.Snapshot{ID: privateTarget, RepoID: fixture.probe.repo, DocHash: privateTarget}
			fixture.store.refs[refOwnerKey(domain.RefTag, "not-a-shared-timeline")] = domain.Ref{RepoID: fixture.probe.repo, Kind: domain.RefTag, Name: "not-a-shared-timeline", Target: privateTarget}
			fixture.store.pendings = []domain.Pending{
				{SessionID: "natural", Target: fixture.base}, {SessionID: "graft", Target: graft},
				{SessionID: "graft-ancestor", Target: ancestor}, {SessionID: "root", Target: fixture.next},
				{SessionID: "newer-capture", Target: fixture.base}, {SessionID: "already-absent", Target: fixture.base},
				{SessionID: "private", Target: privateTarget},
			}
			for _, pending := range fixture.store.pendings {
				if pending.SessionID != "already-absent" {
					fixture.store.pendingNow[pending.SessionID] = pending.Target
				}
			}
			newerTarget := hh("ref-owner-concurrent-capture")
			fixture.store.beforeDelete = func() { fixture.store.pendingNow["newer-capture"] = newerTarget }
			input := inbound.UpdateRefsInput{RepoID: fixture.probe.repo}
			if empty {
				current := fixture.store.refs[refOwnerKey(domain.RefBranch, "topic")]
				current.Target = fixture.next
				fixture.store.refs[refOwnerKey(domain.RefBranch, "topic")] = current
			} else {
				update := fixture.input()
				update.RepoID, update.Ref.RepoID = hh("ignored-item-repo"), hh("ignored-item-ref-repo")
				tag := update
				tag.Ref.Kind, tag.Ref.Name, tag.Ref.BranchID = domain.RefTag, "published", ""
				input.Updates = []inbound.UpdateRefInput{update, tag}
			}
			before := fmt.Sprintf("%#v", fixture.store.snapshots)
			out, err := refOwnerCommands(test, fixture.config()).UpdateRefs(context.Background(), input)
			if err != nil || out.Applied != len(input.Updates) {
				test.Fatalf("out=%+v error=%v", out, err)
			}
			wantFrame := refOwnerScopeFrame{Batch: true, Empty: empty, Audit: "context.refs.updated", Revision: "graph"}
			if empty {
				wantFrame.Revision = "pending"
			}
			if !reflect.DeepEqual(fixture.scope.frames, []refOwnerScopeFrame{wantFrame}) {
				test.Fatalf("batch split its revision/audit scope: %v", fixture.scope.frames)
			}
			for _, operation := range []string{"pendings", "refs", "snapshots"} {
				if fixture.probe.count(operation) != 1 {
					test.Fatalf("%s did not reconcile once: %v", operation, fixture.probe.calls)
				}
			}
			if !reflect.DeepEqual(fixture.store.deletions, fixture.store.pendings[:6]) {
				test.Fatalf("pending deletion lost exact session/target qualification: got=%v want=%v", fixture.store.deletions, fixture.store.pendings[:6])
			}
			wantPending := map[string]domain.ContentHash{"newer-capture": newerTarget, "private": privateTarget}
			if !reflect.DeepEqual(fixture.store.pendingNow, wantPending) || fmt.Sprintf("%#v", fixture.store.snapshots) != before {
				test.Fatalf("reconciliation deleted concurrent/private capture or immutable snapshots: pending=%v", fixture.store.pendingNow)
			}
			if _, broad := any(fixture.store).(outbound.MetadataStore); broad {
				test.Fatal("ref store fake accidentally implements broad metadata/deletion access")
			}
			if _, broad := any(fixture.engine).(outbound.GitEngine); broad {
				test.Fatal("ref engine fake accidentally implements broad engine access")
			}
		})
	}
}

func TestRepositoryRefOwnerBatchBoundIsEnforcedBeforeItemWork(test *testing.T) {
	for _, count := range []int{inbound.MaxRefBatchUpdates, inbound.MaxRefBatchUpdates + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			update := fixture.input()
			update.Ref.Kind, update.Ref.Name, update.Ref.BranchID = domain.RefTag, "batch-bound", ""
			current := update.Ref
			current.RepoID = fixture.probe.repo
			fixture.store.refs[refOwnerKey(current.Kind, current.Name)] = current
			input := inbound.UpdateRefsInput{RepoID: fixture.probe.repo, Updates: make([]inbound.UpdateRefInput, count)}
			for index := range input.Updates {
				input.Updates[index] = update
			}
			out, err := refOwnerCommands(test, fixture.config()).UpdateRefs(context.Background(), input)
			if count > inbound.MaxRefBatchUpdates {
				if !errors.Is(err, domain.ErrValidation) || out != (inbound.UpdateRefsOutput{}) || len(fixture.probe.calls) != 0 {
					test.Fatalf("oversized batch reached item IO: out=%+v error=%v calls=%v", out, err, fixture.probe.calls)
				}
			} else if err != nil || out.Applied != count || len(fixture.scope.frames) != 1 || fixture.probe.count("pendings") != 1 || len(fixture.store.swaps) != 0 {
				test.Fatalf("exact bound rejected or split: out=%+v error=%v scopes=%v", out, err, fixture.scope.frames)
			}
		})
	}
}

func TestRepositoryRefOwnerBatchAggregatesMixedRejectionsAndPreservesCompatibilityCounts(test *testing.T) {
	for _, complete := range []bool{false, true} {
		test.Run(fmt.Sprintf("complete=%v", complete), func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			config := fixture.config()
			if !complete {
				config.Profile, config.Transactional = RepositoryRefCompatibility, false
			}
			first, second := fixture.input(), fixture.input()
			first.Ref.Kind, first.Ref.Name, first.Ref.BranchID = domain.RefTag, "reject-first", ""
			second.Ref.Kind, second.Ref.Name, second.Ref.BranchID = domain.RefTag, "reject-second", ""
			for _, update := range []inbound.UpdateRefInput{first, second} {
				current := update.Ref
				current.RepoID, current.Target = fixture.probe.repo, fixture.base
				fixture.store.refs[refOwnerKey(current.Kind, current.Name)] = current
			}
			last := fixture.input()
			last.Ref.Name, last.Ref.BranchID = "accepted-after-rejections", "accepted-identity"
			input := inbound.UpdateRefsInput{RepoID: fixture.probe.repo, Updates: []inbound.UpdateRefInput{first, fixture.input(), second, last}}
			out, err := refOwnerCommands(test, config).UpdateRefs(context.Background(), input)
			wantApplied := 2
			if complete {
				wantApplied = 0
			}
			if !errors.Is(err, domain.ErrNonFastForward) || !strings.Contains(err.Error(), "tag/reject-first, tag/reject-second") || out.Applied != wantApplied {
				test.Fatalf("mixed batch output=%+v error=%v want applied=%d", out, err, wantApplied)
			}
			if len(fixture.store.swaps) != 2 || fixture.store.swaps[0].Expected != fixture.base || fixture.store.swaps[1].Expected != "" || fixture.store.swaps[1].Ref.Name != last.Ref.Name || len(fixture.scope.frames) != 1 || fixture.probe.count("pendings") != 0 {
				test.Fatalf("batch stopped aggregation, retried heads, or reconciled a rejection: writes=%v scopes=%v calls=%v", fixture.store.swaps, fixture.scope.frames, fixture.probe.calls)
			}
		})
	}
}

func TestRepositoryRefOwnerBatchErrorsClearCompleteButKeepNonTransactionalDiagnostics(test *testing.T) {
	for _, complete := range []bool{false, true} {
		for _, stage := range []string{"append", "reconcile", "scope completion"} {
			test.Run(fmt.Sprintf("complete=%v/%s", complete, stage), func(test *testing.T) {
				fixture := newRefOwnerFixture(test)
				config := fixture.config()
				if !complete {
					config.Profile, config.Transactional = RepositoryRefCompatibility, false
				}
				first, second := fixture.input(), fixture.input()
				first.Ref.Kind, first.Ref.Name, first.Ref.BranchID = domain.RefTag, "first-applied", ""
				second.Append = true
				fixture.engine.classes[fixture.next] = outbound.MoveDiverged
				failure := errors.New("ref-owner batch failure")
				wantApplied, wantReconcile := 2, 1
				switch stage {
				case "append":
					fixture.appendPort.err, wantApplied, wantReconcile = failure, 1, 0
				case "reconcile":
					fixture.store.failures["pendings"] = failure
				case "scope completion":
					fixture.scope.afterErr = failure
				}
				if complete {
					wantApplied = 0
				}
				out, err := refOwnerCommands(test, config).UpdateRefs(context.Background(), inbound.UpdateRefsInput{RepoID: fixture.probe.repo, Updates: []inbound.UpdateRefInput{first, second}})
				if !errors.Is(err, failure) || out.Applied != wantApplied || len(fixture.scope.frames) != 1 || fixture.probe.count("pendings") != wantReconcile || len(fixture.store.swaps) != 1 || len(fixture.appendPort.writes) != 1 {
					test.Fatalf("batch error output=%+v error=%v want applied=%d calls=%v", out, err, wantApplied, fixture.probe.calls)
				}
				written := second.Ref
				written.RepoID = fixture.probe.repo
				if !reflect.DeepEqual(fixture.appendPort.writes, []refOwnerWrite{{Ref: written, Expected: fixture.base}}) {
					test.Fatalf("atomic append failure changed its expected head: %v", fixture.appendPort.writes)
				}
			})
		}
	}
}
