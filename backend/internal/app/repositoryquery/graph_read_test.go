package repositoryquery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type publishedGraphScopeKey struct{}

type publishedGraphProbe struct {
	test               *testing.T
	repo               domain.ContentHash
	projection         domain.GraphProjectionRead
	readErr            error
	admissionErr       error
	finishErr          error
	repairAdmissionErr error
	repairErr          error
	buildErr           error
	buildCanceled      bool
	repairAllowed      bool
	reading            bool
	repairing          bool
	building           bool
	readScopes         int
	readReleases       int
	reads              int
	buildAttempts      int
	buildAcquires      int
	buildReleases      int
	allowedChecks      int
	repairScopes       int
	repairReleases     int
	servingRepairs     []domain.GraphProjectionCorruption
	projectionRepairs  []domain.GraphProjectionCorruption
}

func (probe *publishedGraphProbe) ReadGraph(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	probe.test.Helper()
	probe.readScopes++
	if repo != probe.repo || probe.reading || probe.repairing {
		probe.test.Fatal("invalid read scope", repo)
	}
	if probe.admissionErr != nil {
		return probe.admissionErr
	}
	probe.reading = true
	defer func() {
		if probe.building {
			probe.test.Fatal("build permit escaped the pinned read")
		}
		probe.reading = false
		probe.readReleases++
	}()
	if err := read(context.WithValue(ctx, publishedGraphScopeKey{}, "read")); err != nil {
		return err
	}
	return probe.finishErr
}

func (probe *publishedGraphProbe) GraphRepairAllowed(ctx context.Context) bool {
	probe.test.Helper()
	probe.allowedChecks++
	if probe.reading || probe.building || ctx.Value(publishedGraphScopeKey{}) != nil {
		probe.test.Fatal("repair eligibility retained the read scope")
	}
	return probe.repairAllowed
}

func (probe *publishedGraphProbe) RepairGraph(ctx context.Context, repo domain.ContentHash, repair func(context.Context) error) error {
	probe.test.Helper()
	probe.repairScopes++
	if repo != probe.repo || probe.reading || probe.building || ctx.Value(publishedGraphScopeKey{}) != nil {
		probe.test.Fatal("repair did not enter a fresh scope")
	}
	if probe.repairAdmissionErr != nil {
		return probe.repairAdmissionErr
	}
	probe.repairing = true
	defer func() {
		probe.repairing = false
		probe.repairReleases++
	}()
	return repair(context.WithValue(ctx, publishedGraphScopeKey{}, "repair"))
}

func (probe *publishedGraphProbe) ReadQualifiedGraphProjection(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	probe.test.Helper()
	if !probe.reading || probe.repairing || ctx.Value(publishedGraphScopeKey{}) != "read" || repo != probe.repo {
		probe.test.Fatal("projection read bypassed the pinned authorized scope")
	}
	probe.reads++
	return probe.projection, probe.readErr
}

func (probe *publishedGraphProbe) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	probe.test.Helper()
	probe.buildAttempts++
	if !probe.reading || probe.building || ctx.Value(publishedGraphScopeKey{}) != "read" {
		probe.test.Fatal("position projection bypassed the shared build gate")
	}
	if probe.buildErr != nil {
		return ctx, nil, probe.buildErr
	}
	probe.building = true
	probe.buildAcquires++
	if probe.buildCanceled {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	return ctx, func() {
		if !probe.building || !probe.reading {
			probe.test.Fatal("build permit released outside its owner")
		}
		probe.building = false
		probe.buildReleases++
	}, nil
}

func (probe *publishedGraphProbe) RequestGraphServingRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	probe.checkRepair(ctx, failure)
	if !failure.Serving {
		probe.test.Fatal("candidate failure reached serving repair")
	}
	probe.servingRepairs = append(probe.servingRepairs, failure)
	return probe.repairErr
}

func (probe *publishedGraphProbe) RequestGraphProjectionRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	probe.checkRepair(ctx, failure)
	if failure.Serving {
		probe.test.Fatal("serving failure reached candidate repair")
	}
	probe.projectionRepairs = append(probe.projectionRepairs, failure)
	return probe.repairErr
}

func (probe *publishedGraphProbe) checkRepair(ctx context.Context, failure domain.GraphProjectionCorruption) {
	probe.test.Helper()
	if !probe.repairing || probe.reading || ctx.Value(publishedGraphScopeKey{}) != "repair" || failure.RepoID != probe.repo || failure.Validate() != nil {
		probe.test.Fatal("repair bypassed its fresh authorized scope", failure)
	}
}

func (probe *publishedGraphProbe) assertReleased() {
	probe.test.Helper()
	if probe.reading || probe.building || probe.repairing || probe.buildAcquires != probe.buildReleases {
		probe.test.Fatal("graph read retained owned resources")
	}
}

func newPublishedGraphFixture(test *testing.T, profile Profile) (*PublishedGraphQueries, *publishedGraphProbe, domain.RepositoryView) {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	first := domain.HashContent([]byte(test.Name() + "/first"))
	second := domain.HashContent([]byte(test.Name() + "/second"))
	stamp := time.Unix(1234567890, 0).UTC()
	view := domain.RepositoryView{
		DefaultBranch: "main", Revision: domain.RepositoryRevision{Graph: 19, Pending: 7, Evidence: 3},
		Snapshots: []domain.Snapshot{
			{ID: first, RepoID: repo, DocHash: first, Branch: "main", Branches: []string{"main"}, CreatedAt: stamp},
			{ID: second, RepoID: repo, DocHash: second, Branch: "main", Branches: []string{"main", "topic"}, Parents: []domain.ContentHash{first}, SessionID: "live", Provider: domain.ProviderCodex, CreatedAt: stamp.Add(time.Second)},
		},
		Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: first}},
		History: []domain.HistoryEvent{
			{ID: "birth", RepoID: string(repo), Kind: "birth", Branch: "main", BranchID: "main-id", Source: first, Target: first, CreatedAt: stamp},
			{ID: "position", RepoID: string(repo), Kind: "position", Branch: "main", BranchID: "main-id", Target: first, CreatedAt: stamp.Add(time.Second)},
		},
		Pending: []domain.Pending{
			{RepoID: repo, SessionID: "live", Branch: "main", Provider: domain.ProviderCodex, Target: second, UpdatedAt: stamp},
			{RepoID: repo, SessionID: "duplicate-target", Branch: "main", Target: second, UpdatedAt: stamp},
		},
		ArchivedSessions: []domain.SessionArchiveView{}, Reflog: []domain.RefLogEntry{}, Unsync: []domain.Unsync{},
	}
	graph, err := domain.ProjectGraphState(view, view.DefaultBranch, "")
	if err != nil {
		test.Fatal(err)
	}
	graph.BranchContexts = map[string]domain.BranchContext{"main": {BranchID: "main-id", SnapshotID: first}}
	graph.Integrations = []domain.GraphIntegration{{Branch: "main", Target: first}}
	view.Graph = &graph
	parts, err := domain.EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	digest, err := domain.GraphViewProjectionManifestDigest(context.Background(), repo, parts)
	if err != nil {
		test.Fatal(err)
	}
	source := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
		RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 4}
	projection := domain.GraphProjectionRead{Enabled: true, Ready: true, Complete: true, Rules: domain.GraphViewProjectionRules,
		Dataset: "22222222-2222-4222-8222-222222222222", Source: source, Applied: source, Parts: parts, ServingFence: 17}
	projection.Qualification = &domain.GraphQualificationResult{GraphQualificationKey: projection.QualificationKey(), Expected: digest, Actual: digest}
	probe := &publishedGraphProbe{test: test, repo: repo, projection: projection, repairAllowed: true}
	queries, err := NewPublishedGraph(PublishedGraphConfig{Profile: profile, Scope: probe, Resources: probe, Reader: probe,
		ServingRepairs: probe, ProjectionRepairs: probe, Repos: []domain.ContentHash{repo}})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(probe.assertReleased)
	return queries, probe, view
}

func publishedGraphEndpoint(queries *PublishedGraphQueries, endpoint string, repo domain.ContentHash) (any, error) {
	switch endpoint {
	case "view":
		return queries.GetRepositoryView(context.Background(), repo)
	case "pending":
		return queries.GetPendingView(context.Background(), repo)
	default:
		return queries.QueryGraphState(context.Background(), repo, "")
	}
}

func publishedGraphEmpty(endpoint string) any {
	switch endpoint {
	case "view":
		return domain.RepositoryView{}
	case "pending":
		return domain.PendingView{}
	default:
		return domain.GraphState{}
	}
}

func publishedGraphFailure(probe *publishedGraphProbe) *domain.GraphProjectionCorruption {
	return &domain.GraphProjectionCorruption{RepoID: probe.repo, Dataset: probe.projection.Dataset,
		Applied: probe.projection.Applied, Rules: probe.projection.Rules, Serving: true, ServingFence: probe.projection.ServingFence, Cause: domain.ErrIntegrity}
}

func TestPublishedGraphReadersServeCoherentCurrentAndRetainedBundles(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, PinnedSnapshots} {
		for _, current := range []bool{true, false} {
			test.Run(fmt.Sprintf("%d/current=%t", profile, current), func(test *testing.T) {
				queries, probe, expected := newPublishedGraphFixture(test, profile)
				before, err := probe.projection.Status()
				if err != nil {
					test.Fatal(err)
				}
				if !current {
					probe.projection.Source.Sequence++
					probe.projection.Ready = false
				}
				status, err := probe.projection.Status()
				if err != nil || status.Generation != before.Generation || status.AppliedEpoch != before.AppliedEpoch {
					test.Fatal("source progress changed the retained generation", status, err)
				}
				generation := domain.GraphReadGeneration{Generation: status.Generation, SourceEpoch: status.SourceEpoch,
					AppliedEpoch: status.AppliedEpoch, Rules: status.Rules, Complete: true, Current: current}
				full, err := queries.GetRepositoryView(context.Background(), probe.repo)
				if err != nil || !reflect.DeepEqual(full.ReadGeneration, &generation) || full.Graph == nil || !reflect.DeepEqual(full.Graph.ReadGeneration, &generation) {
					test.Fatal("repository generation differs", full.ReadGeneration, err)
				}
				full.ReadGeneration, full.Graph.ReadGeneration = nil, nil
				if !reflect.DeepEqual(full, expected) {
					test.Fatal("repository metadata differs from the published bundle")
				}
				graph, err := queries.QueryGraphState(context.Background(), probe.repo, "")
				if err != nil || !reflect.DeepEqual(graph.ReadGeneration, &generation) {
					test.Fatal("graph generation differs", graph.ReadGeneration, err)
				}
				graph.ReadGeneration = nil
				if !reflect.DeepEqual(graph, *expected.Graph) {
					test.Fatal("graph-only endpoint rebuilt the published graph")
				}
				pending, err := queries.GetPendingView(context.Background(), probe.repo)
				if err != nil || !reflect.DeepEqual(pending.ReadGeneration, &generation) || pending.Graph == nil || !reflect.DeepEqual(pending.Graph.ReadGeneration, &generation) {
					test.Fatal("pending generation differs", pending.ReadGeneration, err)
				}
				pending.ReadGeneration, pending.Graph.ReadGeneration = nil, nil
				snapshot := expected.Snapshots[1]
				snapshot.Branches = nil
				wantPending := domain.PendingView{Revision: expected.Revision, Pending: expected.Pending, ArchivedSessions: expected.ArchivedSessions,
					Graph: expected.Graph, Snapshots: []domain.Snapshot{snapshot}}
				if !reflect.DeepEqual(pending, wantPending) {
					test.Fatal("pending view lost metadata, retained duplicate targets, or included memberships")
				}
				if probe.readScopes != 3 || probe.readReleases != 3 || probe.reads != 3 || probe.buildAttempts != 0 || probe.repairScopes != 0 {
					test.Fatal("published endpoints bypassed scope or acquired build/repair work", probe)
				}
			})
		}
	}
}

func TestPublishedGraphPositionsUseSharedBuildGateAndLagSemantics(test *testing.T) {
	for _, current := range []bool{true, false} {
		test.Run(fmt.Sprintf("current=%t", current), func(test *testing.T) {
			queries, probe, view := newPublishedGraphFixture(test, PinnedSnapshots)
			if !current {
				probe.projection.Ready = false
				probe.projection.Source.Sequence++
			}
			graph, err := queries.QueryGraphState(context.Background(), probe.repo, "position")
			if err != nil || graph.ReadGeneration == nil || graph.ReadGeneration.Current != current || !graph.ReadGeneration.Complete {
				test.Fatal("position lost retained generation", graph, err)
			}
			expected, err := domain.ProjectGraphState(view, view.DefaultBranch, "position")
			if err != nil {
				test.Fatal(err)
			}
			expected.BranchContexts = map[string]domain.BranchContext{}
			expected.Integrations = []domain.GraphIntegration{}
			expected.ReadGeneration = graph.ReadGeneration
			if !reflect.DeepEqual(graph, expected) || graph.BranchContexts == nil || graph.Integrations == nil {
				test.Fatal("position projection retained current branch evidence or differs from the bundle projection")
			}
			missing, err := queries.QueryGraphState(context.Background(), probe.repo, "not-published")
			wantErr := domain.ErrNotFound
			if !current {
				wantErr = domain.ErrGraphProjectionNotReady
			}
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(missing, domain.GraphState{}) || probe.buildAttempts != 2 || probe.buildReleases != 2 || probe.readReleases != 2 || probe.repairScopes != 0 {
				test.Fatal("missing position leaked a partial graph, permit, or wrong lag error", missing, err)
			}
		})
	}
}

func TestPublishedGraphBuildFailuresReleaseAndReturnZero(test *testing.T) {
	for _, name := range []string{"gate", "canceled"} {
		test.Run(name, func(test *testing.T) {
			queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
			wantErr := domain.ErrResourceOverloaded
			if name == "gate" {
				probe.buildErr = wantErr
			} else {
				probe.buildCanceled = true
				wantErr = context.Canceled
			}
			graph, err := queries.QueryGraphState(context.Background(), probe.repo, "position")
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(graph, domain.GraphState{}) || probe.readReleases != 1 || probe.repairScopes != 0 {
				test.Fatal("build failure leaked a result or skipped cleanup", graph, err)
			}
			probe.assertReleased()
		})
	}
}

func TestPublishedGraphUnavailableOrUnqualifiedNeverReturnsData(test *testing.T) {
	cases := []struct {
		name   string
		change func(*publishedGraphProbe)
		want   error
	}{
		{"disabled", func(probe *publishedGraphProbe) { probe.projection.Enabled = false; probe.projection.Ready = false }, domain.ErrGraphProjectionNotReady},
		{"incomplete", func(probe *publishedGraphProbe) { probe.projection.Complete = false; probe.projection.Ready = false }, domain.ErrGraphProjectionNotReady},
		{"attention", func(probe *publishedGraphProbe) {
			probe.projection.NeedsAttention = true
			probe.projection.Ready = false
		}, domain.ErrGraphProjectionFailed},
		{"foreign-source", func(probe *publishedGraphProbe) {
			probe.projection.Source.RepoID = domain.HashContent([]byte("foreign"))
		}, domain.ErrIntegrity},
		{"foreign-applied", func(probe *publishedGraphProbe) {
			probe.projection.Applied.RepoID = domain.HashContent([]byte("foreign"))
		}, domain.ErrIntegrity},
		{"invalid-status", func(probe *publishedGraphProbe) { probe.projection.Source.Sequence++ }, domain.ErrIntegrity},
		{"missing-proof", func(probe *publishedGraphProbe) { probe.projection.Qualification = nil }, domain.ErrGraphProjectionNotReady},
		{"foreign-proof", func(probe *publishedGraphProbe) {
			probe.projection.Qualification.Dataset = "33333333-3333-4333-8333-333333333333"
		}, domain.ErrIntegrity},
		{"unmatched-proof", func(probe *publishedGraphProbe) {
			probe.projection.Qualification.Expected = domain.HashContent([]byte("mismatch"))
		}, domain.ErrGraphProjectionFailed},
		{"missing-fence", func(probe *publishedGraphProbe) { probe.projection.ServingFence = 0 }, domain.ErrIntegrity},
		{"reader-error", func(probe *publishedGraphProbe) { probe.readErr = domain.ErrIntegrity }, domain.ErrIntegrity},
		{"reader-canceled", func(probe *publishedGraphProbe) { probe.readErr = context.Canceled }, context.Canceled},
	}
	for _, endpoint := range []string{"view", "pending", "graph"} {
		for _, scenario := range cases {
			test.Run(endpoint+"/"+scenario.name, func(test *testing.T) {
				queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
				scenario.change(probe)
				result, err := publishedGraphEndpoint(queries, endpoint, probe.repo)
				if !errors.Is(err, scenario.want) || !reflect.DeepEqual(result, publishedGraphEmpty(endpoint)) || probe.reads != 1 || probe.readReleases != 1 || probe.buildAttempts != 0 || probe.repairScopes != 0 {
					test.Fatal("unavailable projection returned data or scheduled unrelated work", result, err)
				}
			})
		}
	}
}

func TestPublishedGraphCorruptionRepairsOnlyTheFailedGenerationAfterReadRelease(test *testing.T) {
	for _, endpoint := range []string{"view", "pending", "graph"} {
		for _, kind := range []string{"decode", "manifest", "serving", "candidate"} {
			test.Run(endpoint+"/"+kind, func(test *testing.T) {
				queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
				failure := publishedGraphFailure(probe)
				switch kind {
				case "decode":
					probe.projection.Parts = probe.projection.Parts[:1]
				case "manifest":
					probe.projection.Qualification.Actual = domain.HashContent([]byte("different matched manifest"))
					probe.projection.Qualification.Expected = probe.projection.Qualification.Actual
				case "candidate":
					failure.Serving, failure.ServingFence = false, 0
					probe.readErr = fmt.Errorf("qualified projection: %w", failure)
				case "serving":
					probe.readErr = failure
				}
				result, err := publishedGraphEndpoint(queries, endpoint, probe.repo)
				var actual *domain.GraphProjectionCorruption
				if !errors.As(err, &actual) || !reflect.DeepEqual(actual, failure) || !reflect.DeepEqual(result, publishedGraphEmpty(endpoint)) {
					test.Fatal("corruption lost serving identity or returned partial data", result, err)
				}
				if probe.readErr != nil && err != probe.readErr {
					test.Fatal("successful scheduling replaced the original read error", err)
				}
				if probe.allowedChecks != 1 || probe.repairScopes != 1 || probe.repairReleases != 1 || probe.readReleases != 1 {
					test.Fatal("repair bypassed eligibility or scope cleanup", probe)
				}
				if kind == "candidate" {
					if len(probe.servingRepairs) != 0 || !reflect.DeepEqual(probe.projectionRepairs, []domain.GraphProjectionCorruption{*failure}) {
						test.Fatal("candidate corruption reached the wrong requester")
					}
				} else if len(probe.projectionRepairs) != 0 || !reflect.DeepEqual(probe.servingRepairs, []domain.GraphProjectionCorruption{*failure}) {
					test.Fatal("serving corruption reached the wrong requester")
				}
			})
		}
	}
}

func TestPublishedGraphRepairErrorsPreserveTheReadFailure(test *testing.T) {
	for _, endpoint := range []string{"view", "pending", "graph"} {
		for _, kind := range []string{"authorization", "request"} {
			test.Run(endpoint+"/"+kind, func(test *testing.T) {
				queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
				failure := publishedGraphFailure(probe)
				probe.readErr = failure
				wantErr := domain.ErrForbidden
				if kind == "authorization" {
					probe.repairAdmissionErr = wantErr
				} else {
					wantErr = errors.New("durable repair request unavailable")
					probe.repairErr = wantErr
				}
				result, err := publishedGraphEndpoint(queries, endpoint, probe.repo)
				var actual *domain.GraphProjectionCorruption
				if !errors.Is(err, failure) || !errors.Is(err, domain.ErrIntegrity) || !errors.Is(err, wantErr) || !errors.As(err, &actual) || actual != failure ||
					!strings.Contains(err.Error(), "schedule graph projection repair: ") || !reflect.DeepEqual(result, publishedGraphEmpty(endpoint)) {
					test.Fatal("repair failure replaced the read error or returned data", result, err)
				}
				if kind == "authorization" && len(probe.servingRepairs) != 0 || probe.readReleases != 1 {
					test.Fatal("repair request bypassed fresh authorization or read cleanup")
				}
			})
		}
	}
}

func TestPublishedGraphRepairRejectsDisallowedUnidentifiedOrForeignFailures(test *testing.T) {
	for _, kind := range []string{"disallowed", "plain", "foreign", "dataset", "canceled", "fence", "typed-nil"} {
		test.Run(kind, func(test *testing.T) {
			queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
			failure := publishedGraphFailure(probe)
			switch kind {
			case "disallowed":
				probe.repairAllowed = false
			case "plain":
				probe.readErr = domain.ErrIntegrity
			case "foreign":
				failure.RepoID = domain.HashContent([]byte("foreign"))
				failure.Applied.RepoID = failure.RepoID
			case "dataset":
				failure.Dataset = ""
			case "canceled":
				failure.Cause = context.Canceled
			case "fence":
				failure.ServingFence = 0
			case "typed-nil":
				failure = nil
			}
			if probe.readErr == nil {
				probe.readErr = failure
			}
			result, err := queries.GetRepositoryView(context.Background(), probe.repo)
			if err != probe.readErr || !reflect.DeepEqual(result, domain.RepositoryView{}) || probe.repairScopes != 0 || len(probe.servingRepairs)+len(probe.projectionRepairs) != 0 {
				test.Fatal("unrepairable failure scheduled work or replaced the original error", result, err)
			}
		})
	}
}

func TestPublishedGraphScopeFailuresDiscardSuccessfulResults(test *testing.T) {
	for _, endpoint := range []string{"view", "pending", "graph"} {
		for _, phase := range []string{"admission", "finish"} {
			test.Run(endpoint+"/"+phase, func(test *testing.T) {
				queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
				wantErr := domain.ErrForbidden
				wantReads := 0
				if phase == "admission" {
					probe.admissionErr = wantErr
				} else {
					wantErr = errors.New("pinned read finalization failed")
					probe.finishErr = wantErr
					wantReads = 1
				}
				result, err := publishedGraphEndpoint(queries, endpoint, probe.repo)
				if err != wantErr || !reflect.DeepEqual(result, publishedGraphEmpty(endpoint)) || probe.reads != wantReads || probe.readReleases != wantReads || probe.repairScopes != 0 {
					test.Fatal("scope failure exposed a result or bypassed the scope", result, err)
				}
			})
		}
	}
}

func TestPublishedGraphConstructorRequiresExplicitPortsAndValidProfile(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, PinnedSnapshots} {
		for _, name := range []string{"Scope", "Resources", "Reader", "ServingRepairs", "ProjectionRepairs"} {
			for _, typedNil := range []bool{false, true} {
				test.Run(fmt.Sprintf("%d/%s/typed-nil=%t", profile, name, typedNil), func(test *testing.T) {
					probe := &publishedGraphProbe{test: test}
					config := PublishedGraphConfig{Profile: profile, Scope: probe, Resources: probe, Reader: probe, ServingRepairs: probe, ProjectionRepairs: probe}
					field := reflect.ValueOf(&config).Elem().FieldByName(name)
					if typedNil {
						var missing *publishedGraphProbe
						field.Set(reflect.ValueOf(missing))
					} else {
						field.SetZero()
					}
					if queries, err := NewPublishedGraph(config); queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) || probe.readScopes != 0 || probe.buildAttempts != 0 || probe.repairScopes != 0 {
						test.Fatal("constructor accepted or invoked an absent dependency", queries, err)
					}
				})
			}
		}
	}
	for _, profile := range []Profile{0, 99} {
		probe := &publishedGraphProbe{test: test}
		if queries, err := NewPublishedGraph(PublishedGraphConfig{Profile: profile, Scope: probe, Resources: probe, Reader: probe, ServingRepairs: probe, ProjectionRepairs: probe}); queries != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("constructor accepted an invalid profile", queries, err)
		}
	}
}

func TestPublishedGraphAllowlistIsCopiedAndEnforcedBeforeScope(test *testing.T) {
	queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
	repos := []domain.ContentHash{probe.repo}
	config := PublishedGraphConfig{Profile: PinnedSnapshots, Scope: probe, Resources: probe, Reader: probe,
		ServingRepairs: probe, ProjectionRepairs: probe, Repos: repos}
	queries, err := NewPublishedGraph(config)
	if err != nil || queries.scope != config.Scope || queries.resources != config.Resources || queries.reader != config.Reader || queries.servingRepairs != config.ServingRepairs || queries.projectionRepairs != config.ProjectionRepairs {
		test.Fatal("constructor replaced explicit ports", err)
	}
	foreign := domain.HashContent([]byte("unconfigured"))
	repos[0] = foreign
	for _, endpoint := range []string{"view", "pending", "graph"} {
		result, err := publishedGraphEndpoint(queries, endpoint, foreign)
		if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(result, publishedGraphEmpty(endpoint)) || probe.readScopes != 0 {
			test.Fatal("unconfigured repository reached the read scope", result, err)
		}
		result, err = publishedGraphEndpoint(queries, endpoint, "invalid")
		if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, publishedGraphEmpty(endpoint)) || probe.readScopes != 0 {
			test.Fatal("malformed repository reached the read scope", result, err)
		}
	}
	if _, err := queries.GetRepositoryView(context.Background(), probe.repo); err != nil || probe.reads != 1 {
		test.Fatal("allowlist mutation removed the configured repository", err)
	}
	config.Repos = []domain.ContentHash{"invalid"}
	if queries, err := NewPublishedGraph(config); queries != nil || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("constructor accepted a malformed repository", queries, err)
	}
}
