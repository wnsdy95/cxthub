package repositoryquery

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type statePinKey struct{}
type stateAdmissionKey struct{}
type stateAuthorizationKey struct{}

type stateTestResources struct {
	acquired  int
	released  int
	err       error
	onAcquire func()
}

func (resources *stateTestResources) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	resources.acquired++
	if resources.err != nil {
		return nil, nil, resources.err
	}
	if resources.onAcquire != nil {
		resources.onAcquire()
	}
	return context.WithValue(ctx, stateAdmissionKey{}, resources), func() { resources.released++ }, nil
}

type stateTestScope struct {
	test      *testing.T
	repo      domain.ContentHash
	resources *stateTestResources
	required  []bool
	pins      int
	entryErr  error
	finishErr error
}

func (scope *stateTestScope) AuthorizeRead(ctx context.Context, repo domain.ContentHash, required bool) (context.Context, error) {
	scope.required = append(scope.required, required)
	if repo != scope.repo || ctx.Value(stateAdmissionKey{}) != scope.resources || scope.resources.acquired <= scope.resources.released {
		scope.test.Fatal("authorization did not follow injected admission", repo)
	}
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		var err error
		ctx, err = authorize(ctx, repo)
		if err != nil {
			return nil, err
		}
	} else if _, system := inbound.RepositoryActor(ctx); required && !system {
		return nil, domain.ErrForbidden
	}
	return context.WithValue(ctx, stateAuthorizationKey{}, scope), nil
}

func (scope *stateTestScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if repo != scope.repo {
		scope.test.Fatal("pinned a different repository", repo)
	}
	if ctx.Value(statePinKey{}) == scope {
		return read(ctx)
	}
	scope.pins++
	if scope.entryErr != nil {
		return scope.entryErr
	}
	if ctx.Value(stateAdmissionKey{}) == scope.resources && ctx.Value(stateAuthorizationKey{}) != scope {
		scope.test.Fatal("authorization context was discarded before pinning")
	}
	if err := read(context.WithValue(ctx, statePinKey{}, scope)); err != nil {
		return err
	}
	return scope.finishErr
}

type stateTestPorts struct {
	scope    *stateTestScope
	calls    []string
	errors   map[string]error
	revision domain.RepositoryRevision
	history  []domain.HistoryEvent
	archives []domain.SessionArchive
	pending  domain.PendingProjectionRead
	graph    domain.GraphProjectionRead
}

func (ports *stateTestPorts) read(ctx context.Context, repo domain.ContentHash, operation string) error {
	if repo != ports.scope.repo || ctx.Value(statePinKey{}) != ports.scope {
		ports.scope.test.Fatal("port read escaped the captured repository", operation, repo)
	}
	ports.calls = append(ports.calls, operation)
	return ports.errors[operation]
}

func (ports *stateTestPorts) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	return ports.revision, ports.read(ctx, repo, "revision")
}

func (ports *stateTestPorts) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return ports.history, ports.read(ctx, repo, "history")
}

func (ports *stateTestPorts) ListSessionArchives(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	return ports.archives, ports.read(ctx, repo, "archives")
}

func (ports *stateTestPorts) ReadPendingProjection(ctx context.Context, repo domain.ContentHash) (domain.PendingProjectionRead, error) {
	return ports.pending, ports.read(ctx, repo, "pending-body")
}

func (ports *stateTestPorts) ReadPendingProjectionStatus(ctx context.Context, repo domain.ContentHash) (domain.PendingProjectionRead, error) {
	projection := ports.pending
	projection.Parts = nil
	return projection, ports.read(ctx, repo, "pending-status")
}

func (ports *stateTestPorts) ReadQualifiedGraphProjectionStatus(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return ports.graph, ports.read(ctx, repo, "graph-status")
}

type stateTestMetadata struct {
	*stateTestPorts
	view domain.RepositoryView
}

func (metadata *stateTestMetadata) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if err := metadata.read(ctx, repo, "repo"); err != nil {
		return domain.Repo{}, err
	}
	return domain.Repo{ID: repo, DefaultBranch: metadata.view.DefaultBranch}, nil
}

func (metadata *stateTestMetadata) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return metadata.view.Refs, metadata.read(ctx, repo, "refs")
}

func (metadata *stateTestMetadata) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if branch != "" {
		metadata.scope.test.Fatal("pending fallback filtered captured snapshots", branch)
	}
	return metadata.view.Snapshots, metadata.read(ctx, repo, "snapshots")
}

func (metadata *stateTestMetadata) ReadReflog(ctx context.Context, repo domain.ContentHash) ([]domain.RefLogEntry, error) {
	return metadata.view.Reflog, metadata.read(ctx, repo, "reflog")
}

func (metadata *stateTestMetadata) ListPendings(ctx context.Context, repo domain.ContentHash) ([]domain.Pending, error) {
	return metadata.view.Pending, metadata.read(ctx, repo, "pending")
}

func (metadata *stateTestMetadata) ListUnsyncs(ctx context.Context, repo domain.ContentHash) ([]domain.Unsync, error) {
	return metadata.view.Unsync, metadata.read(ctx, repo, "unsync")
}

type stateTestFixture struct {
	repo      domain.ContentHash
	queries   *Queries
	scope     *stateTestScope
	resources *stateTestResources
	metadata  *stateTestMetadata
	ports     *stateTestPorts
	want      domain.PendingStateView
}

func newStateTestFixture(test *testing.T, published bool) stateTestFixture {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	resources := &stateTestResources{}
	scope := &stateTestScope{test: test, repo: repo, resources: resources}
	revision := domain.RepositoryRevision{Graph: 3, Pending: 5, Evidence: 7}
	ports := &stateTestPorts{scope: scope, errors: map[string]error{}, revision: revision}
	metadata := &stateTestMetadata{stateTestPorts: &stateTestPorts{scope: scope, errors: map[string]error{}, revision: domain.RepositoryRevision{Graph: 99}}}
	capture := domain.HashContent([]byte("pending capture"))
	root := domain.HashContent([]byte("published root"))
	metadata.view = domain.RepositoryView{
		Revision: revision, DefaultBranch: "main",
		Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: root}},
		Snapshots: []domain.Snapshot{
			{ID: root, RepoID: repo, DocHash: root, Branch: "main", CreatedAt: time.Unix(100, 0).UTC()},
			{ID: capture, RepoID: repo, DocHash: capture, Branch: "topic", Branches: []string{"retained-main", "retained-topic"}, Provider: domain.ProviderCodex, SessionID: "live", CreatedAt: time.Unix(123, 0).UTC()},
		},
		Pending: []domain.Pending{{RepoID: repo, SessionID: "live", Target: capture, Branch: "topic", UpdatedAt: time.Unix(999, 0).UTC()}},
	}
	captured := metadata.view
	captured.Pending = append([]domain.Pending{}, captured.Pending...)
	captured.Pending[0].UpdatedAt = captured.Snapshots[1].CreatedAt
	want, err := domain.ProjectPendingState(context.Background(), captured)
	if err != nil {
		test.Fatal(err)
	}
	parts, err := domain.EncodePendingProjection(context.Background(), repo, want)
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryViewSourceScope, RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 4}
	ports.pending = domain.PendingProjectionRead{Enabled: true, Ready: true, Dataset: "22222222-2222-4222-8222-222222222222", Source: checkpoint, Applied: checkpoint, Parts: parts}
	checkpoint.Scope = domain.RepositoryGraphSourceScope
	ports.graph = domain.GraphProjectionRead{Enabled: true, Ready: true, Complete: true, Rules: domain.GraphViewProjectionRules, Dataset: ports.pending.Dataset, Source: checkpoint, Applied: checkpoint, ServingFence: 2}
	digest := domain.HashContent([]byte("retained graph proof"))
	ports.graph.Qualification = &domain.GraphQualificationResult{GraphQualificationKey: ports.graph.QualificationKey(), Expected: digest, Actual: digest}
	config := Config{Profile: PinnedSnapshots, Scope: scope, Resources: resources, Metadata: metadata, Revisions: ports, History: ports, Pending: ports, PendingState: ports, GraphState: ports}
	if published {
		config.PendingRepos, config.GraphRepos = []domain.ContentHash{repo}, []domain.ContentHash{repo}
	}
	queries, err := New(config)
	if err != nil {
		test.Fatal(err)
	}
	return stateTestFixture{repo: repo, queries: queries, scope: scope, resources: resources, metadata: metadata, ports: ports, want: want}
}

func TestRepositoryChangeStateUsesExplicitStatusPorts(test *testing.T) {
	fixture := newStateTestFixture(test, true)
	first, err := fixture.queries.RepositoryChangeState(context.Background(), fixture.repo)
	if err != nil || first.RepositoryRevision != fixture.ports.revision || first.PendingProjection == nil || first.PendingProjection.State != "ready" || first.GraphProjection == nil || first.GraphProjection.State != "ready" {
		test.Fatal("explicit status ports were not used", first, err)
	}
	body, err := fixture.queries.GetPendingState(inbound.WithSystemActor(context.Background()), fixture.repo)
	if err != nil || body.ReadGeneration == nil || body.ReadGeneration.Generation != first.PendingProjection.Generation || body.ReadGeneration.SourceEpoch != first.PendingProjection.SourceEpoch {
		test.Fatal("status and body generations differ", body, err)
	}
	fixture.ports.pending.Ready = false
	fixture.ports.pending.Source.Sequence++
	fixture.ports.graph.Ready = false
	fixture.ports.graph.Source.Sequence++
	second, err := fixture.queries.RepositoryChangeState(context.Background(), fixture.repo)
	if err != nil || first.Equal(second) || second.RepositoryRevision != first.RepositoryRevision || second.PendingProjection.State != "catching_up" || !second.GraphProjection.Complete || second.GraphProjection.Current || second.GraphProjection.Generation != first.GraphProjection.Generation {
		test.Fatal("source-only change lost retained graph generation", second, err)
	}
	wantCalls := []string{"revision", "pending-status", "graph-status", "pending-body", "revision", "pending-status", "graph-status"}
	if !reflect.DeepEqual(fixture.ports.calls, wantCalls) || len(fixture.metadata.calls) != 0 || fixture.resources.acquired != 1 || fixture.resources.released != 1 || fixture.scope.pins != 3 {
		test.Fatal("notice read payload or metadata capabilities", fixture.ports.calls, fixture.metadata.calls)
	}
	fixture.ports.errors["pending-status"] = domain.ErrForbidden
	if _, err := fixture.queries.RepositoryChangeState(context.Background(), fixture.repo); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("status error was suppressed", err)
	}
}

func TestRepositoryChangeStateQualificationStates(test *testing.T) {
	for _, kind := range []string{"missing", "mismatch", "foreign", "foreign-repository", "status-error"} {
		test.Run(kind, func(test *testing.T) {
			fixture := newStateTestFixture(test, true)
			wantState, wantErr := "", error(nil)
			switch kind {
			case "missing":
				fixture.ports.graph.Qualification = nil
				wantState = "catching_up"
			case "mismatch":
				fixture.ports.graph.Qualification.Actual = domain.HashContent([]byte("mismatch"))
				wantState = "needs_attention"
			case "foreign":
				fixture.ports.graph.Qualification.Dataset = "33333333-3333-4333-8333-333333333333"
				wantErr = domain.ErrIntegrity
			case "foreign-repository":
				fixture.ports.graph.Source.RepoID = domain.HashContent([]byte("foreign"))
				wantErr = domain.ErrIntegrity
			case "status-error":
				fixture.ports.errors["graph-status"] = domain.ErrForbidden
				wantErr = domain.ErrForbidden
			}
			state, err := fixture.queries.RepositoryChangeState(context.Background(), fixture.repo)
			if !errors.Is(err, wantErr) {
				test.Fatal(state, err)
			}
			if wantErr == nil && (state.GraphProjection == nil || state.GraphProjection.State != wantState || state.GraphProjection.Complete || state.GraphProjection.Current || state.GraphProjection.Generation != "") {
				test.Fatal("unqualified graph announced as complete", state)
			}
			if len(fixture.metadata.calls) != 0 {
				test.Fatal("qualification loaded live graph evidence", fixture.metadata.calls)
			}
		})
	}
}

func TestRepositoryStatePinnedAtomicFailure(test *testing.T) {
	for _, method := range []string{"revision", "change", "pending"} {
		test.Run(method, func(test *testing.T) {
			fixture := newStateTestFixture(test, true)
			failure := errors.New("captured read could not finish")
			fixture.scope.finishErr = failure
			var result, empty any
			var err error
			switch method {
			case "revision":
				result, err = fixture.queries.RepositoryRevision(context.Background(), fixture.repo)
				empty = domain.RepositoryRevision{}
			case "change":
				result, err = fixture.queries.RepositoryChangeState(context.Background(), fixture.repo)
				empty = domain.RepositoryChangeState{}
			case "pending":
				result, err = fixture.queries.GetPendingState(inbound.WithSystemActor(context.Background()), fixture.repo)
				empty = domain.PendingStateView{}
			}
			if !errors.Is(err, failure) || !reflect.DeepEqual(result, empty) || fixture.scope.pins != 1 || len(fixture.ports.calls) == 0 || fixture.resources.acquired != fixture.resources.released {
				test.Fatal("failed pinned read leaked its captured value", result, err, fixture.scope.pins)
			}
		})
	}
}

func TestPendingStatePublishedAndMissingReaders(test *testing.T) {
	for _, kind := range []string{"warm", "not-ready", "advanced-source", "failed", "disabled", "body-error", "corrupt", "missing-body", "body-only", "missing-graph-status"} {
		test.Run(kind, func(test *testing.T) {
			fixture := newStateTestFixture(test, true)
			wantErr := error(nil)
			switch kind {
			case "not-ready":
				fixture.ports.pending.Ready = false
				wantErr = domain.ErrPendingProjectionNotReady
			case "advanced-source":
				fixture.ports.pending.Source.Sequence++
				wantErr = domain.ErrPendingProjectionNotReady
			case "failed":
				fixture.ports.pending.NeedsAttention = true
				wantErr = domain.ErrPendingProjectionFailed
			case "disabled":
				fixture.ports.pending.Enabled = false
			case "body-error":
				fixture.ports.errors["pending-body"] = domain.ErrForbidden
				wantErr = domain.ErrForbidden
			case "corrupt":
				fixture.ports.pending.Parts = nil
			case "missing-body":
				fixture.queries.pending = nil
				wantErr = domain.ErrCatalogUnsupported
			case "body-only":
				queries, err := New(Config{Profile: LegacyCompatibility, Scope: fixture.scope, Resources: fixture.resources, Metadata: fixture.metadata, Revisions: fixture.ports, History: fixture.ports, Pending: fixture.ports, PendingRepos: []domain.ContentHash{fixture.repo}})
				if err != nil {
					test.Fatal(err)
				}
				fixture.queries = queries
			case "missing-graph-status":
				fixture.queries.graphState = nil
			}
			view, err := fixture.queries.GetPendingState(inbound.WithSystemActor(context.Background()), fixture.repo)
			if kind == "corrupt" {
				if err == nil || view.Projection != "" {
					test.Fatal("corrupt published body was accepted", view, err)
				}
			} else if !errors.Is(err, wantErr) {
				test.Fatal(view, err)
			} else if wantErr == nil {
				if (view.ReadGeneration == nil) != (kind == "disabled") {
					test.Fatal("wrong published generation contract", view)
				}
				if view.ReadGeneration != nil && (!view.ReadGeneration.Complete || view.ReadGeneration.SourceEpoch != view.ReadGeneration.AppliedEpoch || view.ReadGeneration.Rules != domain.PendingProjectionRules) {
					test.Fatal("incomplete pending generation", view.ReadGeneration)
				}
				view.ReadGeneration = nil
				if !reflect.DeepEqual(view, fixture.want) {
					test.Fatal("published body changed captured facts", view, fixture.want)
				}
			}
			if kind != "disabled" && len(fixture.metadata.calls) != 0 || fixture.resources.acquired != 1 || fixture.resources.released != 1 || !reflect.DeepEqual(fixture.scope.required, []bool{true}) {
				test.Fatal("published query fell back or escaped admission", fixture.metadata.calls, fixture.scope.required)
			}
			if kind == "body-only" || kind == "missing-graph-status" {
				if _, err := fixture.queries.RepositoryChangeState(context.Background(), fixture.repo); !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("missing status reader silently fell back", err)
				}
			}
		})
	}
}

func TestPendingStateFallbackRetainsCapturedMemberships(test *testing.T) {
	fixture := newStateTestFixture(test, false)
	retained := append([]string{}, fixture.metadata.view.Snapshots[1].Branches...)
	for range 2 {
		view, err := fixture.queries.GetPendingState(context.Background(), fixture.repo)
		if err != nil || !reflect.DeepEqual(view, fixture.want) || len(view.Snapshots) != 1 || len(view.Snapshots[0].Branches) != 0 || !view.Pending[0].UpdatedAt.Equal(fixture.metadata.view.Snapshots[1].CreatedAt) || !reflect.DeepEqual(fixture.metadata.view.Snapshots[1].Branches, retained) {
			test.Fatal("pending patch changed retained branch ownership", view, err)
		}
	}
	wantMetadata := []string{"repo", "refs", "snapshots", "reflog", "pending", "unsync", "repo", "refs", "snapshots", "reflog", "pending", "unsync"}
	if !reflect.DeepEqual(fixture.metadata.calls, wantMetadata) || !reflect.DeepEqual(fixture.ports.calls, []string{"history", "revision", "history", "revision"}) || fixture.scope.pins != 2 || !reflect.DeepEqual(fixture.scope.required, []bool{false, false}) {
		test.Fatal("fallback did not use only raw explicit metadata ports", fixture.metadata.calls, fixture.ports.calls)
	}
	fixture.metadata.view.Refs[0].Target = fixture.metadata.view.Snapshots[1].ID
	fixture.ports.revision.Pending++
	view, err := fixture.queries.GetPendingState(context.Background(), fixture.repo)
	if err != nil || view.Revision != fixture.ports.revision || len(view.OrphanSessions) != 0 || len(view.HoldCounts) != 0 || !reflect.DeepEqual(fixture.metadata.view.Snapshots[1].Branches, retained) {
		test.Fatal("fallback retained stale publication state", view, err)
	}
	fixture.queries.revisions = nil
	if revision, err := fixture.queries.RepositoryRevision(context.Background(), fixture.repo); err != nil || revision != (domain.RepositoryRevision{}) {
		test.Fatal("optional revisions were discovered from metadata", revision, err)
	}
}

func TestPendingStateFallbackFailuresAndOptionalArchives(test *testing.T) {
	for _, operation := range []string{"repo", "refs", "snapshots", "reflog", "history", "pending", "unsync", "archives", "revision", "missing-history", "missing-repository", "identity", "admission"} {
		test.Run(operation, func(test *testing.T) {
			fixture := newStateTestFixture(test, false)
			failure := errors.New("captured metadata unavailable")
			switch operation {
			case "missing-history":
				fixture.queries.history = nil
			case "missing-repository":
				fixture.metadata.errors["repo"] = domain.ErrNotFound
			case "identity":
				failure = domain.ErrDocumentIdentityUpgradeRequired
				fixture.scope.entryErr = failure
			case "admission":
				fixture.resources.err = failure
			case "archives":
				fixture.queries.archives = fixture.ports
				fixture.ports.errors[operation] = failure
			case "history", "revision":
				fixture.ports.errors[operation] = failure
			default:
				fixture.metadata.errors[operation] = failure
			}
			view, err := fixture.queries.GetPendingState(context.Background(), fixture.repo)
			switch operation {
			case "missing-history":
				if err == nil || err.Error() != "context history storage unavailable" || view.Projection != "" {
					test.Fatal("missing optional history contract changed", view, err)
				}
			case "missing-repository":
				if err != nil || view.Projection != "on-hold-v1" || view.DefaultBranch != "" || view.ArchivedSessions == nil {
					test.Fatal("legacy missing repository or archives stopped working", view, err)
				}
			default:
				if !errors.Is(err, failure) || !reflect.DeepEqual(view, domain.PendingStateView{}) {
					test.Fatal("metadata failure became a partial result", view, err)
				}
			}
			if operation != "admission" && fixture.resources.acquired != fixture.resources.released {
				test.Fatal("interactive resource was retained after failure")
			}
			if operation == "admission" && (len(fixture.scope.required) != 0 || fixture.scope.pins != 0 || len(fixture.metadata.calls) != 0 || len(fixture.ports.calls) != 0) {
				test.Fatal("failed admission reached repository data")
			}
		})
	}
	fixture := newStateTestFixture(test, false)
	fixture.queries.archives = fixture.ports
	snapshot := fixture.metadata.view.Snapshots[1]
	fixture.ports.archives = []domain.SessionArchive{{RepoID: fixture.repo, Key: domain.SessionArchiveKey(snapshot), SnapshotID: snapshot.ID, Provider: snapshot.Provider, SessionID: snapshot.SessionID, ArchivedAt: time.Unix(200, 0).UTC(), ArchivedBy: "maintainer"}}
	view, err := fixture.queries.GetPendingState(context.Background(), fixture.repo)
	if err != nil || len(view.ArchivedSessions) != 1 || view.ArchivedSessions[0].LatestSnapshotID != snapshot.ID || !reflect.DeepEqual(fixture.ports.calls, []string{"history", "archives", "revision"}) {
		test.Fatal("explicit archive reader was ignored", view, err)
	}
}

func TestPendingStateUsesCurrentRevokedCallbackAfterAdmission(test *testing.T) {
	fixture := newStateTestFixture(test, true)
	authorizations, revoked := 0, false
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		authorizations++
		if repo != fixture.repo || bound.Value(stateAdmissionKey{}) != fixture.resources {
			test.Fatal("callback lost admitted context", repo)
		}
		if revoked {
			return nil, domain.ErrForbidden
		}
		return bound, nil
	})
	if _, err := fixture.queries.GetPendingState(ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	fixture.resources.onAcquire = func() { revoked = true }
	if view, err := fixture.queries.GetPendingState(ctx, fixture.repo); !errors.Is(err, domain.ErrForbidden) || view.Projection != "" || authorizations != 2 || fixture.scope.pins != 1 || !reflect.DeepEqual(fixture.ports.calls, []string{"pending-body"}) {
		test.Fatal("warm body bypassed current revoked callback", view, err, authorizations)
	}
	if _, err := fixture.queries.GetPendingState(context.Background(), fixture.repo); !errors.Is(err, domain.ErrForbidden) || fixture.scope.pins != 1 || fixture.resources.acquired != 3 || fixture.resources.released != 3 {
		test.Fatal("required authorization or release was lost", err)
	}
}
