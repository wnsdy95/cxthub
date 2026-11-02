package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type legacyReadPinKey struct{}
type legacyReadBuildKey struct{}

type legacyReadProbe struct {
	test        *testing.T
	repo        domain.ContentHash
	metadata    domain.Repo
	view        domain.RepositoryView
	archives    []domain.SessionArchive
	generation  domain.RepositoryViewSourceCheckpoint
	errors      map[string]error
	calls       map[string]int
	order       []string
	scopeErr    error
	finishErr   error
	cancel      context.CancelFunc
	cancelOn    string
	cancelBound bool
	cancelExit  bool
	active      bool
	building    bool
	pins        int
	releases    int
	builds      int
	built       int
}

func (probe *legacyReadProbe) ReadGraph(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	probe.test.Helper()
	probe.pins++
	if repo != probe.repo || probe.active {
		probe.test.Fatal("read scope recursed or changed repository", repo)
	}
	if probe.scopeErr != nil {
		return probe.scopeErr
	}
	owner, system := inbound.RepositoryActor(ctx)
	if owner != "owner" && !system {
		return domain.ErrForbidden
	}
	probe.active = true
	defer func() {
		if probe.building {
			probe.test.Fatal("build permit escaped read scope")
		}
		probe.active = false
		probe.releases++
	}()
	bound := context.WithValue(ctx, legacyReadPinKey{}, probe)
	if probe.cancelBound {
		var cancel context.CancelFunc
		bound, cancel = context.WithCancel(bound)
		cancel()
	}
	if err := read(bound); err != nil {
		return err
	}
	if probe.cancelExit {
		probe.cancel()
	}
	return probe.finishErr
}

func (probe *legacyReadProbe) read(ctx context.Context, repo domain.ContentHash, operation string) error {
	probe.test.Helper()
	if !probe.active || ctx.Value(legacyReadPinKey{}) != probe || repo != probe.repo {
		probe.test.Fatal("narrow reader escaped pinned context", operation, repo)
	}
	probe.calls[operation]++
	probe.order = append(probe.order, operation)
	if operation == probe.cancelOn {
		probe.cancel()
	}
	return probe.errors[operation]
}

func (probe *legacyReadProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return probe.metadata, probe.read(ctx, repo, "repo")
}

func (probe *legacyReadProbe) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return probe.view.Refs, probe.read(ctx, repo, "refs")
}

func (probe *legacyReadProbe) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if branch != "" {
		probe.test.Fatal("loader filtered raw snapshots", branch)
	}
	return probe.view.Snapshots, probe.read(ctx, repo, "snapshots")
}

func (probe *legacyReadProbe) ReadReflog(ctx context.Context, repo domain.ContentHash) ([]domain.RefLogEntry, error) {
	if probe.calls["reflog"] != probe.calls["snapshots"]-1 {
		probe.test.Fatal("reflog was read twice for one metadata generation")
	}
	return probe.view.Reflog, probe.read(ctx, repo, "reflog")
}

func (probe *legacyReadProbe) ListPendings(ctx context.Context, repo domain.ContentHash) ([]domain.Pending, error) {
	return probe.view.Pending, probe.read(ctx, repo, "pending")
}

func (probe *legacyReadProbe) ListUnsyncs(ctx context.Context, repo domain.ContentHash) ([]domain.Unsync, error) {
	return probe.view.Unsync, probe.read(ctx, repo, "unsync")
}

func (probe *legacyReadProbe) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return probe.view.History, probe.read(ctx, repo, "history")
}

func (probe *legacyReadProbe) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	return probe.view.Revision, probe.read(ctx, repo, "revision")
}

func (probe *legacyReadProbe) ListSessionArchives(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	return probe.archives, probe.read(ctx, repo, "archives")
}

func (probe *legacyReadProbe) IsPinnedRead(ctx context.Context) bool {
	return probe.active && ctx.Value(legacyReadPinKey{}) == probe
}

func (probe *legacyReadProbe) ReadRepositoryGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	return probe.generation, probe.read(ctx, repo, "generation")
}

func (probe *legacyReadProbe) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	if err := probe.read(ctx, probe.repo, "build"); err != nil {
		return ctx, nil, err
	}
	if ctx.Value(legacyReadBuildKey{}) == probe {
		return ctx, func() {}, nil
	}
	if probe.building {
		probe.test.Fatal("shared build gate was not reused")
	}
	probe.building = true
	probe.builds++
	return context.WithValue(ctx, legacyReadBuildKey{}, probe), func() {
		if !probe.building || !probe.active {
			probe.test.Fatal("build permit released outside its owner")
		}
		probe.building = false
		probe.built++
	}, nil
}

func (probe *legacyReadProbe) VerifiedPR(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]evidencequery.VerifiedPRContribution, error) {
	if !reflect.DeepEqual(history, probe.view.History) {
		probe.test.Fatal("projection replaced captured history")
	}
	return nil, probe.read(ctx, repo, "pr")
}

func (probe *legacyReadProbe) VerifiedGit(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]evidencequery.VerifiedGitContribution, error) {
	return nil, probe.read(ctx, repo, "git")
}

type legacyReadFixture struct {
	queries    *LegacyGraphQueries
	projection *LegacyProjection
	probe      *legacyReadProbe
	ctx        context.Context
	position   string
}

func newLegacyReadFixture(test *testing.T, profile Profile) *legacyReadFixture {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name() + "/repo"))
	first := domain.HashContent([]byte(test.Name() + "/first"))
	second := domain.HashContent([]byte(test.Name() + "/pending"))
	stamp := time.Unix(1234567890, 0).UTC()
	position := fmt.Sprintf("%032x", 2)
	probe := &legacyReadProbe{test: test, repo: repo, metadata: domain.Repo{ID: repo, DefaultBranch: "main"},
		generation: legacyProjectCheckpoint(repo), errors: map[string]error{}, calls: map[string]int{}}
	probe.view = domain.RepositoryView{
		Revision: domain.RepositoryRevision{Graph: 8, Pending: 4, Evidence: 2},
		Refs:     []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: first}},
		Snapshots: []domain.Snapshot{
			{RepoID: repo, ID: first, DocHash: first, Branch: "main", Branches: []string{"stale"}, CreatedAt: stamp},
			{RepoID: repo, ID: second, DocHash: second, Branch: "topic", Branches: []string{"stale"}, Parents: []domain.ContentHash{first},
				Provider: domain.ProviderCodex, SessionID: "live", Message: "hook: pending", CreatedAt: stamp.Add(time.Second)},
		},
		Reflog: []domain.RefLogEntry{
			{Kind: domain.RefBranch, Name: "zeta", Old: first, New: second},
			{Kind: domain.RefBranch, Name: "alpha", New: second},
			{Kind: domain.RefHead, Name: "HEAD", New: second},
			{Kind: domain.RefTag, Name: "not-membership", New: second},
		},
		History: []domain.HistoryEvent{
			{RepoID: string(repo), ID: fmt.Sprintf("%032x", 1), BranchID: "main-id", Branch: "main", Kind: "birth", Source: first, Target: first, CreatedAt: stamp},
			{RepoID: string(repo), ID: position, BranchID: "main-id", Branch: "main", Kind: "position", Target: first, CreatedAt: stamp.Add(time.Second)},
		},
		Pending: []domain.Pending{
			{RepoID: repo, SessionID: "live", Provider: domain.ProviderCodex, Branch: "main", Target: second, UpdatedAt: stamp.Add(time.Hour)},
			{RepoID: repo, SessionID: "same-target", Branch: "main", Target: second, Dismissed: true, UpdatedAt: stamp.Add(time.Hour)},
		},
	}
	projection, err := NewLegacyProjection(LegacyProjectionConfig{Scope: probe, Resources: probe, Metadata: probe,
		Contributions: probe, Generations: probe, Graphs: &GraphCache{}, Branches: &BranchCache{}})
	if err != nil {
		test.Fatal(err)
	}
	queries, err := NewLegacyGraph(LegacyGraphConfig{Profile: profile, Scope: probe, Metadata: probe,
		History: probe, Revisions: probe, Archives: probe, Projection: projection})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if probe.active || probe.building || probe.builds != probe.built {
			test.Fatal("legacy query leaked its owner scope or gate")
		}
	})
	return &legacyReadFixture{queries: queries, projection: projection, probe: probe,
		ctx: inbound.WithRepositoryActor(context.Background(), "owner"), position: position}
}

func (fixture *legacyReadFixture) load(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	fixture.probe.active = true
	defer func() { fixture.probe.active = false }()
	return fixture.queries.LoadRepositoryView(context.WithValue(ctx, legacyReadPinKey{}, fixture.probe), repo)
}

func (fixture *legacyReadFixture) endpoint(ctx context.Context, repo domain.ContentHash, endpoint string) (any, error) {
	switch endpoint {
	case "load":
		return fixture.load(ctx, repo)
	case "view":
		return fixture.queries.GetRepositoryView(ctx, repo)
	case "pending":
		return fixture.queries.GetPendingView(ctx, repo)
	case "position":
		return fixture.queries.QueryGraphState(ctx, repo, fixture.position)
	default:
		return fixture.queries.QueryGraphState(ctx, repo, "")
	}
}

func legacyReadZero(endpoint string) any {
	switch endpoint {
	case "load", "view":
		return domain.RepositoryView{}
	case "pending":
		return domain.PendingView{}
	default:
		return domain.GraphState{}
	}
}

func TestLegacyGraphLoaderOwnsOneCoherentMetadataRead(test *testing.T) {
	fixture := newLegacyReadFixture(test, PinnedSnapshots)
	probe := fixture.probe
	anchor := probe.view.Snapshots[0]
	probe.archives = []domain.SessionArchive{{RepoID: probe.repo, Key: domain.SessionArchiveKey(anchor), SnapshotID: anchor.ID}}
	view, err := fixture.load(fixture.ctx, probe.repo)
	if err != nil {
		test.Fatal(err)
	}
	if !slices.Equal(probe.order, []string{"repo", "refs", "snapshots", "reflog", "history", "pending", "unsync", "archives", "revision"}) || probe.pins != 0 || probe.builds != 0 || len(fixture.projection.graphs.Keys()) != 0 {
		test.Fatal("metadata loader entered admission, projection, or a second read", probe.order)
	}
	if !slices.Equal(view.Snapshots[0].Branches, []string{"main", "zeta"}) || !slices.Equal(view.Snapshots[1].Branches, []string{"alpha", "topic", "zeta"}) || !reflect.DeepEqual(view.Reflog, probe.view.Reflog) {
		test.Fatal("membership was not derived from the returned reflog", view.Snapshots)
	}
	if !view.Pending[0].UpdatedAt.Equal(probe.view.Snapshots[1].CreatedAt) || !view.Pending[1].Dismissed || !slices.Equal(probe.view.Snapshots[1].Branches, []string{"stale"}) || !probe.view.Pending[0].UpdatedAt.Equal(probe.view.Snapshots[0].CreatedAt.Add(time.Hour)) {
		test.Fatal("capture time fallback mutated raw storage or lost pending flags")
	}
	if view.Graph != nil || view.ReadGeneration != nil || view.DefaultBranch != "main" || view.Revision != probe.view.Revision || !reflect.DeepEqual(view.Semantics, domain.ProjectContextSemantics(view.Snapshots, view.History)) || !reflect.DeepEqual(view.ArchivedSessions, domain.ProjectSessionArchives(probe.archives, view.Snapshots, view.History, view.DefaultBranch)) {
		test.Fatal("metadata loader changed public domain projection semantics")
	}
}

func TestLegacyGraphEndpointsReuseBoundPortsCacheAndGate(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, PinnedSnapshots} {
		test.Run(fmt.Sprint(profile), func(test *testing.T) {
			fixture := newLegacyReadFixture(test, profile)
			queries, probe := fixture.queries, fixture.probe
			if queries.scope != probe || queries.metadata != probe || queries.history != probe || queries.revisions != probe || queries.archives != probe || queries.projection != fixture.projection {
				test.Fatal("constructor replaced explicit narrow ports")
			}
			full, err := queries.GetRepositoryView(fixture.ctx, probe.repo)
			if err != nil || full.Graph == nil || full.Graph.Revision != full.Revision || full.Graph.BranchContexts["main"].SnapshotID != full.Refs[0].Target {
				test.Fatal("legacy full view did not execute its actual projection", full.Graph, err)
			}
			pending, err := queries.GetPendingView(fixture.ctx, probe.repo)
			expectedPending, expectedErr := domain.ProjectPendingView(full)
			if err != nil || expectedErr != nil || !reflect.DeepEqual(pending, expectedPending) || len(pending.Snapshots) != 1 || pending.Snapshots[0].Branches != nil {
				test.Fatal("pending endpoint did not reuse domain filtering", pending, err)
			}
			graph, err := queries.QueryGraphState(fixture.ctx, probe.repo, "")
			if err != nil || !reflect.DeepEqual(graph, *full.Graph) || probe.calls["pr"] != 1 || probe.calls["git"] != 1 || probe.builds != 1 || probe.pins != 3 || probe.releases != 3 || probe.calls["reflog"] != 3 {
				test.Fatal("endpoints replaced shared cache/gate or recursed through the scope", probe.calls, err)
			}
			historical, err := queries.QueryGraphState(fixture.ctx, probe.repo, fixture.position)
			expectedGraph, expectedErr := ProjectGraphPosition(full, fixture.position)
			if err != nil || expectedErr != nil || !reflect.DeepEqual(historical, expectedGraph) || historical.PositionEvent != fixture.position || probe.calls["pr"] != 1 {
				test.Fatal("position endpoint selected current contribution context", err)
			}
			missing, err := queries.QueryGraphState(fixture.ctx, probe.repo, "missing")
			if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(missing, domain.GraphState{}) {
				test.Fatal("missing position published a partial graph", missing, err)
			}
		})
	}
}

func TestLegacyGraphCacheDoesNotReuseStalePendingOrMembership(test *testing.T) {
	fixture := newLegacyReadFixture(test, PinnedSnapshots)
	probe := fixture.probe
	first, err := fixture.queries.GetRepositoryView(fixture.ctx, probe.repo)
	if err != nil {
		test.Fatal(err)
	}
	probe.view.Reflog = append(probe.view.Reflog, domain.RefLogEntry{Kind: domain.RefBranch, Name: "later", New: probe.view.Snapshots[1].ID})
	second, err := fixture.queries.GetRepositoryView(fixture.ctx, probe.repo)
	if err != nil || !slices.Contains(second.Snapshots[1].Branches, "later") || slices.Contains(first.Snapshots[1].Branches, "later") || probe.calls["pr"] != 2 || probe.builds != 2 {
		test.Fatal("same-revision metadata change reused a stale cache fingerprint", probe.calls, err)
	}
	latest := probe.view.Snapshots[1]
	latest.ID, latest.DocHash = domain.HashContent([]byte("latest pending")), domain.HashContent([]byte("latest pending"))
	latest.CreatedAt = latest.CreatedAt.Add(time.Minute)
	probe.view.Snapshots = append(probe.view.Snapshots, latest)
	for index := range probe.view.Pending {
		probe.view.Pending[index].Target = latest.ID
	}
	probe.view.Revision.Pending++
	probe.generation.Sequence++
	pending, err := fixture.queries.GetPendingView(fixture.ctx, probe.repo)
	if err != nil || pending.Revision != probe.view.Revision || len(pending.Snapshots) != 1 || pending.Snapshots[0].ID != latest.ID || !pending.Pending[0].UpdatedAt.Equal(latest.CreatedAt) || pending.Graph.Revision != pending.Revision {
		test.Fatal("pending endpoint mixed metadata generations", pending, err)
	}
}

func TestRepositoryViewLoaderNeedsOnlyFourReaders(test *testing.T) {
	fixture := newLegacyReadFixture(test, LegacyCompatibility)
	probe := fixture.probe
	probe.view = domain.RepositoryView{}
	probe.errors["repo"] = fmt.Errorf("legacy repository: %w", domain.ErrNotFound)
	probe.active = true
	defer func() { probe.active = false }()
	bound := context.WithValue(context.Background(), legacyReadPinKey{}, probe)
	view, err := LoadRepositoryView(bound, probe.repo, RepositoryViewReaders{Metadata: probe, History: probe})
	if err != nil || view.DefaultBranch != "" || view.Revision != (domain.RepositoryRevision{}) || view.Graph != nil || view.ReadGeneration != nil {
		test.Fatal("compatibility loader required projection, scope, auth, or optional readers", view, err)
	}
	for _, array := range []any{view.Refs, view.Snapshots, view.Reflog, view.History, view.Pending, view.Unsync, view.ArchivedSessions, view.Semantics.Merges} {
		if reflect.ValueOf(array).IsNil() {
			test.Fatal("empty loader published a null output array", array)
		}
	}
	if probe.calls["revision"] != 0 || probe.calls["archives"] != 0 || probe.calls["generation"] != 0 || probe.calls["build"] != 0 || probe.pins != 0 {
		test.Fatal("standalone loader discovered an undeclared capability", probe.calls)
	}
	fixture.queries.projection, fixture.queries.revisions, fixture.queries.archives = nil, nil, nil
	delegated, err := fixture.queries.LoadRepositoryView(bound, probe.repo)
	if err != nil || !reflect.DeepEqual(view, delegated) {
		test.Fatal("bound method did not delegate to standalone loader", delegated, err)
	}
	missing, err := LoadRepositoryView(bound, probe.repo, RepositoryViewReaders{Metadata: probe})
	if err == nil || err.Error() != "context history storage unavailable" || !reflect.DeepEqual(missing, domain.RepositoryView{}) {
		test.Fatal("missing legacy history did not preserve its compatibility error", missing, err)
	}
}

func TestRepositoryViewLoaderRejectsMissingAndDeclaredTypedNilReaders(test *testing.T) {
	for _, name := range []string{"Metadata", "History", "Revisions", "Archives"} {
		for _, typedNil := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/typed=%t", name, typedNil), func(test *testing.T) {
				fixture := newLegacyReadFixture(test, LegacyCompatibility)
				probe := fixture.probe
				readers := RepositoryViewReaders{Metadata: probe, History: probe, Revisions: probe, Archives: probe}
				field := reflect.ValueOf(&readers).Elem().FieldByName(name)
				field.SetZero()
				if typedNil {
					var missing *legacyReadProbe
					field.Set(reflect.ValueOf(missing))
				}
				probe.active = true
				defer func() { probe.active = false }()
				view, err := LoadRepositoryView(context.WithValue(fixture.ctx, legacyReadPinKey{}, probe), probe.repo, readers)
				if name == "Metadata" || typedNil {
					if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(view, domain.RepositoryView{}) || len(probe.order) != 0 {
						test.Fatal("invalid reader declaration performed I/O or published metadata", probe.order, err)
					}
				} else if name == "History" {
					if err == nil || err.Error() != "context history storage unavailable" || !reflect.DeepEqual(view, domain.RepositoryView{}) {
						test.Fatal("missing history fallback changed", err)
					}
				} else if err != nil || name == "Revisions" && view.Revision != (domain.RepositoryRevision{}) || name == "Archives" && view.ArchivedSessions == nil {
					test.Fatal("optional reader became mandatory", view, err)
				}
			})
		}
	}
}

func TestLegacyGraphOwnerFlagsAndCompatibilityFallbacks(test *testing.T) {
	for _, captured := range []bool{false, true} {
		for _, strict := range []bool{false, true} {
			test.Run(fmt.Sprintf("captured=%t/strict=%t", captured, strict), func(test *testing.T) {
				fixture := newLegacyReadFixture(test, LegacyCompatibility)
				fixture.projection.captured, fixture.projection.requireComplete = captured, strict
				fixture.probe.errors["generation"] = domain.ErrCatalogUnsupported
				view, err := fixture.queries.GetRepositoryView(fixture.ctx, fixture.probe.repo)
				if err != nil || view.Graph == nil || fixture.queries.projection != fixture.projection || fixture.projection.captured != captured || fixture.projection.requireComplete != strict || len(fixture.projection.graphs.Keys()) != 0 || captured && fixture.probe.calls["generation"] != 0 {
					test.Fatal("legacy query replaced owner projection flags or fabricated a cache generation", err)
				}
			})
		}
	}
	fixture := newLegacyReadFixture(test, LegacyCompatibility)
	fixture.probe.view.Snapshots[1].CreatedAt = time.Time{}
	expected := fixture.probe.view.Pending[0].UpdatedAt
	view, err := fixture.load(fixture.ctx, fixture.probe.repo)
	if err != nil || !view.Pending[0].UpdatedAt.Equal(expected) {
		test.Fatal("zero snapshot creation overwrote the legacy pending timestamp", err)
	}
}

func TestLegacyGraphErrorsNeverPublishPartialResults(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, PinnedSnapshots} {
		for _, endpoint := range []string{"load", "view", "pending", "graph", "position"} {
			for _, operation := range []string{"repo", "refs", "snapshots", "reflog", "history", "pending", "unsync", "archives", "revision", "generation", "build", "pr", "git", "scope", "finish"} {
				if endpoint == "load" && slices.Contains([]string{"generation", "build", "pr", "git", "scope", "finish"}, operation) || endpoint == "position" && slices.Contains([]string{"pr", "git"}, operation) {
					continue
				}
				test.Run(fmt.Sprintf("%d/%s/%s", profile, endpoint, operation), func(test *testing.T) {
					fixture := newLegacyReadFixture(test, profile)
					failure := errors.New("injected " + operation)
					switch operation {
					case "scope":
						fixture.probe.scopeErr = failure
					case "finish":
						fixture.probe.finishErr = failure
					default:
						fixture.probe.errors[operation] = failure
					}
					result, err := fixture.endpoint(fixture.ctx, fixture.probe.repo, endpoint)
					if !errors.Is(err, failure) || !reflect.DeepEqual(result, legacyReadZero(endpoint)) {
						test.Fatal("failed input or scope published a partial result", result, err)
					}
				})
			}
		}
	}
}

func TestLegacyGraphScopeValidatesEveryRawRowBeforeDeriving(test *testing.T) {
	for _, endpoint := range []string{"load", "view", "pending", "graph", "position"} {
		for _, branchless := range []bool{false, true} {
			for _, collection := range []string{"metadata", "refs", "snapshots", "history", "pending", "unsync", "archives"} {
				test.Run(fmt.Sprintf("%s/branchless=%t/%s", endpoint, branchless, collection), func(test *testing.T) {
					fixture := newLegacyReadFixture(test, PinnedSnapshots)
					probe := fixture.probe
					foreign := domain.HashContent([]byte("foreign repository"))
					if branchless {
						probe.view.Refs = nil
					}
					switch collection {
					case "metadata":
						probe.metadata.ID = foreign
					case "refs":
						probe.view.Refs = append(probe.view.Refs, domain.Ref{RepoID: foreign, Kind: domain.RefTag, Name: "foreign"})
					case "snapshots":
						probe.view.Snapshots[1].RepoID = foreign
					case "history":
						probe.view.History[1].RepoID = string(foreign)
					case "pending":
						probe.view.Pending[1].RepoID = foreign
					case "unsync":
						probe.view.Unsync = []domain.Unsync{{RepoID: probe.repo}, {RepoID: foreign}}
					case "archives":
						probe.archives = []domain.SessionArchive{{RepoID: probe.repo}, {RepoID: foreign}}
					}
					result, err := fixture.endpoint(fixture.ctx, probe.repo, endpoint)
					if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, legacyReadZero(endpoint)) || probe.builds != 0 || probe.calls["pr"] != 0 || len(fixture.projection.graphs.Keys()) != 0 {
						test.Fatal("foreign raw row escaped scope validation before projection", result, err)
					}
				})
			}
		}
	}
}

func TestLegacyGraphValidationAuthorizationAndCancellation(test *testing.T) {
	for _, endpoint := range []string{"load", "view", "pending", "graph", "position"} {
		test.Run(endpoint+"/invalid", func(test *testing.T) {
			fixture := newLegacyReadFixture(test, PinnedSnapshots)
			result, err := fixture.endpoint(fixture.ctx, "invalid", endpoint)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, legacyReadZero(endpoint)) || fixture.probe.pins != 0 || len(fixture.probe.order) != 0 {
				test.Fatal("invalid repository reached a reader or scope", result, err)
			}
		})
		for _, phase := range []string{"before", "bound", "revision", "generation", "build", "pr", "git", "exit"} {
			if endpoint == "load" && phase != "before" && phase != "revision" || endpoint == "position" && (phase == "pr" || phase == "git") {
				continue
			}
			test.Run(endpoint+"/cancel="+phase, func(test *testing.T) {
				fixture := newLegacyReadFixture(test, PinnedSnapshots)
				ctx, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				fixture.probe.cancel = cancel
				switch phase {
				case "before":
					cancel()
				case "bound":
					fixture.probe.cancelBound = true
				case "exit":
					fixture.probe.cancelExit = true
				default:
					fixture.probe.cancelOn = phase
				}
				result, err := fixture.endpoint(ctx, fixture.probe.repo, endpoint)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, legacyReadZero(endpoint)) {
					test.Fatal("canceled read published metadata or graph", result, err)
				}
				if phase == "before" && (fixture.probe.pins != 0 || len(fixture.probe.order) != 0) || phase == "bound" && len(fixture.probe.order) != 0 {
					test.Fatal("already canceled scope reached a reader")
				}
			})
		}
		if endpoint != "load" {
			test.Run(endpoint+"/owner", func(test *testing.T) {
				fixture := newLegacyReadFixture(test, PinnedSnapshots)
				result, err := fixture.endpoint(context.Background(), fixture.probe.repo, endpoint)
				if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(result, legacyReadZero(endpoint)) || len(fixture.probe.order) != 0 {
					test.Fatal("legacy endpoint bypassed the injected owner scope", result, err)
				}
				if _, err := fixture.endpoint(inbound.WithSystemActor(context.Background()), fixture.probe.repo, endpoint); err != nil {
					test.Fatal("trusted worker identity was lost", err)
				}
			})
		}
	}
}

func TestLegacyPendingMissingTargetFailsAtomically(test *testing.T) {
	fixture := newLegacyReadFixture(test, PinnedSnapshots)
	fixture.probe.view.Pending[0].Target = domain.HashContent([]byte("missing pending snapshot"))
	view, err := fixture.queries.GetPendingView(fixture.ctx, fixture.probe.repo)
	if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(view, domain.PendingView{}) || fixture.probe.builds != fixture.probe.built {
		test.Fatal("pending filtering published an incomplete graph generation", view, err)
	}
}

func TestLegacyGraphBranchlessCompatibilityPublishesNonNilArrays(test *testing.T) {
	fixture := newLegacyReadFixture(test, LegacyCompatibility)
	probe := fixture.probe
	probe.view = domain.RepositoryView{}
	probe.errors["repo"] = domain.ErrNotFound
	fixture.queries.revisions, fixture.queries.archives = nil, nil
	view, err := fixture.queries.GetRepositoryView(fixture.ctx, probe.repo)
	if err != nil || view.Graph == nil || view.DefaultBranch != "" || view.Revision != (domain.RepositoryRevision{}) || view.Graph.PrimaryBranch != "" || probe.calls["revision"] != 0 || probe.calls["archives"] != 0 {
		test.Fatal("branchless compatibility required revision/archive capabilities", view, err)
	}
	pending, err := fixture.queries.GetPendingView(fixture.ctx, probe.repo)
	if err != nil || pending.Graph == nil || pending.Pending == nil || pending.Snapshots == nil || pending.ArchivedSessions == nil {
		test.Fatal("empty pending view published null arrays", pending, err)
	}
	if probe.calls["pr"] != 0 || probe.calls["git"] != 0 || probe.calls["reflog"] != 2 || probe.pins != 2 {
		test.Fatal("branchless graph fabricated an evidence owner or a second metadata read", probe.calls)
	}
}

func TestPendingStateFallbackDelegatesWithoutGraphConstruction(test *testing.T) {
	fixture := newStateTestFixture(test, false)
	retained := append([]string{}, fixture.metadata.view.Snapshots[1].Branches...)
	view, err := fixture.queries.GetPendingState(context.Background(), fixture.repo)
	if err != nil || !reflect.DeepEqual(view, fixture.want) || len(view.Snapshots) != 1 || view.Snapshots[0].Branches != nil || !slices.Equal(fixture.metadata.view.Snapshots[1].Branches, retained) || fixture.scope.pins != 1 || fixture.resources.acquired != fixture.resources.released {
		test.Fatal("shared loader changed pending-state filtering or read ownership", view, err)
	}
	if !slices.Equal(fixture.metadata.calls, []string{"repo", "refs", "snapshots", "reflog", "pending", "unsync"}) || !slices.Equal(fixture.ports.calls, []string{"history", "revision"}) {
		test.Fatal("pending fallback read outside the four explicit metadata ports", fixture.metadata.calls, fixture.ports.calls)
	}
	fixture.queries.archives = fixture.ports
	fixture.ports.archives = []domain.SessionArchive{{RepoID: domain.HashContent([]byte("foreign archive"))}}
	failed, err := fixture.queries.GetPendingState(context.Background(), fixture.repo)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(failed, domain.PendingStateView{}) {
		test.Fatal("pending fallback published a foreign raw archive", failed, err)
	}
}
