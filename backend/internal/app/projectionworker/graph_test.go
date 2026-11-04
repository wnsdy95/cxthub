package projectionworker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphOwnerWorkerKey struct{}
type graphOwnerWorkKey struct{}
type graphOwnerReadKey struct{}
type graphOwnerBuildKey struct{}

type graphOwnerScope struct {
	repo          domain.ContentHash
	workerCalls   int
	workCalls     int
	readCalls     int
	checks        int
	builds        int
	releases      int
	activeWork    int
	activeRead    int
	activeBuild   int
	beforeWork    func(context.Context) error
	afterWork     func()
	step          func(context.Context, string) error
	readFinishErr error
}

func (scope *graphOwnerScope) WorkerContext(ctx context.Context) context.Context {
	scope.workerCalls++
	identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
	ctx = inbound.WithDocumentIdentities(ctx, identities)
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, identities, identities)
	return context.WithValue(ctx, graphOwnerWorkerKey{}, true)
}

func (scope *graphOwnerScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	scope.workCalls++
	if scope.beforeWork != nil {
		if err := scope.beforeWork(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scope.activeWork++
	defer func() {
		scope.activeWork--
		if scope.afterWork != nil {
			scope.afterWork()
		}
	}()
	return work(context.WithValue(ctx, graphOwnerWorkKey{}, true))
}

func (scope *graphOwnerScope) CheckRepository(ctx context.Context, repo domain.ContentHash) error {
	scope.checks++
	if repo != scope.repo {
		return domain.ErrIntegrity
	}
	if scope.step != nil {
		if err := scope.step(ctx, "identity"); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (scope *graphOwnerScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.readCalls++
	if repo != scope.repo {
		return domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scope.activeRead++
	defer func() { scope.activeRead-- }()
	if err := read(context.WithValue(ctx, graphOwnerReadKey{}, true)); err != nil {
		return err
	}
	return scope.readFinishErr
}

func (scope *graphOwnerScope) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	if scope.step != nil {
		if err := scope.step(ctx, "build"); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	scope.builds++
	scope.activeBuild++
	return context.WithValue(ctx, graphOwnerBuildKey{}, true), func() {
		scope.activeBuild--
		scope.releases++
	}, nil
}

type graphOwnerFailure struct {
	job    domain.GraphProjectionLease
	reason string
	retry  bool
}

type graphOwnerStore struct {
	outbound.GraphProjectionStore
	repo       domain.ContentHash
	jobs       []domain.GraphProjectionLease
	pages      []domain.RepositoryViewSourcePage
	inputs     []domain.RepositoryViewSourceEntry
	requests   []domain.RepositoryViewSourceRequest
	saved      []domain.GraphProjectionLease
	loaded     []domain.GraphProjectionLease
	published  []domain.GraphProjectionPart
	publishJob domain.GraphProjectionLease
	failures   []graphOwnerFailure
	calls      map[string]int
	checkpoint domain.RepositoryViewSourceCheckpoint
	metadata   domain.Repo
	proof      []domain.RepositoryViewSourceEntry
	step       func(context.Context, string) error
}

func (storage *graphOwnerStore) visit(ctx context.Context, stage string) error {
	storage.calls[stage]++
	if storage.step != nil {
		if err := storage.step(ctx, stage); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (storage *graphOwnerStore) ClaimGraphProjection(ctx context.Context, duration time.Duration) (domain.GraphProjectionLease, error) {
	if duration != time.Minute {
		return domain.GraphProjectionLease{}, domain.ErrValidation
	}
	if err := storage.visit(ctx, "claim"); err != nil {
		return domain.GraphProjectionLease{}, err
	}
	if len(storage.jobs) == 0 {
		return domain.GraphProjectionLease{}, domain.ErrNotFound
	}
	job := storage.jobs[0]
	storage.jobs = storage.jobs[1:]
	return job, nil
}

func (storage *graphOwnerStore) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	if err := storage.visit(ctx, "source"); err != nil {
		return domain.RepositoryViewSourcePage{}, err
	}
	if repo != storage.repo || request.Validate() != nil {
		return domain.RepositoryViewSourcePage{}, domain.ErrIntegrity
	}
	storage.requests = append(storage.requests, request)
	if len(storage.pages) == 0 {
		return domain.RepositoryViewSourcePage{}, domain.ErrNotFound
	}
	page := storage.pages[0]
	storage.pages = storage.pages[1:]
	return page, nil
}

func (storage *graphOwnerStore) SaveGraphProjectionPage(ctx context.Context, job domain.GraphProjectionLease, page domain.RepositoryViewSourcePage) error {
	if err := storage.visit(ctx, "save"); err != nil {
		return err
	}
	storage.saved = append(storage.saved, job)
	return nil
}

func (storage *graphOwnerStore) LoadGraphProjectionInputs(ctx context.Context, job domain.GraphProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
	if err := storage.visit(ctx, "load"); err != nil {
		return nil, err
	}
	storage.loaded = append(storage.loaded, job)
	return storage.inputs, nil
}

func (storage *graphOwnerStore) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, metadata domain.Repo, proof []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	if err := storage.visit(ctx, "factory"); err != nil {
		return nil, err
	}
	storage.checkpoint, storage.metadata, storage.proof = checkpoint, metadata, proof
	reader, err := store.NewGraphSourceReader(ctx, checkpoint, metadata, proof)
	if err != nil {
		return nil, err
	}
	return &graphOwnerEvidence{GraphEvidenceReader: reader, storage: storage}, nil
}

type graphOwnerEvidence struct {
	outbound.GraphEvidenceReader
	storage *graphOwnerStore
}

func (reader *graphOwnerEvidence) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if err := reader.storage.visit(ctx, "evidence"); err != nil {
		return domain.Repo{}, err
	}
	return reader.GraphEvidenceReader.GetRepo(ctx, repo)
}

func (storage *graphOwnerStore) PublishGraphProjection(ctx context.Context, job domain.GraphProjectionLease, parts []domain.GraphProjectionPart) error {
	if err := storage.visit(ctx, "publish"); err != nil {
		return err
	}
	storage.publishJob, storage.published = job, parts
	return nil
}

func (storage *graphOwnerStore) FailGraphProjection(ctx context.Context, job domain.GraphProjectionLease, reason string, retry bool) error {
	storage.failures = append(storage.failures, graphOwnerFailure{job: job, reason: reason, retry: retry})
	return storage.visit(ctx, "fail")
}

type graphOwnerReferences struct {
	storage    *graphOwnerStore
	jobs       []domain.GraphProjectionLease
	pages      []domain.RepositoryViewSourcePage
	references []domain.GraphProjectionReference
}

func (references *graphOwnerReferences) SaveGraphProjectionPageWithReference(ctx context.Context, job domain.GraphProjectionLease, page domain.RepositoryViewSourcePage, reference domain.GraphProjectionReference) error {
	if err := references.storage.visit(ctx, "save-reference"); err != nil {
		return err
	}
	references.jobs = append(references.jobs, job)
	references.pages = append(references.pages, page)
	references.references = append(references.references, reference)
	return nil
}

type graphOwnerGeneration struct {
	storage    *graphOwnerStore
	checkpoint domain.RepositoryViewSourceCheckpoint
}

func (generation *graphOwnerGeneration) ReadRepositoryGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	if ctx.Value(graphOwnerReadKey{}) != true || repo != generation.storage.repo {
		return domain.RepositoryViewSourceCheckpoint{}, domain.ErrConflict
	}
	if err := generation.storage.visit(ctx, "generation"); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, err
	}
	return generation.checkpoint, nil
}

type graphOwnerReference struct {
	storage  *graphOwnerStore
	expected domain.ContentHash
}

func (reference *graphOwnerReference) Reference(ctx context.Context, repo domain.ContentHash) (domain.ContentHash, error) {
	if ctx.Value(graphOwnerReadKey{}) != true || repo != reference.storage.repo {
		return "", domain.ErrConflict
	}
	if err := reference.storage.visit(ctx, "reference"); err != nil {
		return "", err
	}
	return reference.expected, nil
}

type graphOwnerFixture struct {
	config     GraphConfig
	scope      *graphOwnerScope
	storage    *graphOwnerStore
	job        domain.GraphProjectionLease
	checkpoint domain.RepositoryViewSourceCheckpoint
	references *graphOwnerReferences
	generation *graphOwnerGeneration
	reference  *graphOwnerReference
}

func newGraphOwnerFixture(test *testing.T) *graphOwnerFixture {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
		RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 7}
	job := domain.GraphProjectionLease{RepoID: repo, Dataset: "22222222-2222-4222-8222-222222222222", Epoch: checkpoint.Epoch,
		Fence: 3, Phase: "project", AppliedSequence: -1, InputSequence: checkpoint.Sequence, LeaseUntil: time.Now().Add(time.Minute)}
	metadata := domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/graph.git", ContextProtocol: 1}
	marshal := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		return raw
	}
	snapshot := domain.Snapshot{ID: domain.HashContent([]byte("snapshot")), RepoID: repo, DocHash: domain.HashContent([]byte("snapshot")),
		Provider: domain.ProviderCodex, SessionID: "session", CreatedAt: time.Unix(123, 0).UTC()}
	tree, err := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	if err != nil {
		test.Fatal(err)
	}
	storage := &graphOwnerStore{repo: repo, jobs: []domain.GraphProjectionLease{job}, calls: make(map[string]int)}
	storage.inputs = []domain.RepositoryViewSourceEntry{
		{Sequence: 2, Kind: "git_tree", Key: string(marshal([]string{metadata.GitRemoteURL, tree.OID})), Value: marshal(map[string]any{"repo_id": repo, "origin": metadata.GitRemoteURL, "oid": tree.OID, "payload": tree})},
		{Sequence: 1, Kind: "pending", Key: "session", Value: marshal(domain.Pending{RepoID: repo, SessionID: "session", Target: snapshot.ID, Branch: "main"})},
		{Sequence: 1, Kind: "protocol", Key: string(repo), Value: marshal(map[string]any{"context_protocol": 1})},
		{Sequence: 1, Kind: "repository", Key: string(repo), Value: marshal(struct {
			domain.Repo
			RemoteURLSupported    bool `json:"remote_url_supported"`
			GitRemoteURLSupported bool `json:"git_remote_url_supported"`
		}{metadata, true, true})},
		{Sequence: 4, Kind: "revision", Key: string(repo), Value: marshal(domain.RepositoryRevision{Graph: 4, Pending: 3})},
		{Sequence: 1, Kind: "snapshot", Key: string(snapshot.ID), Value: marshal(snapshot)},
	}
	scope := &graphOwnerScope{repo: repo, step: storage.visit}
	return &graphOwnerFixture{scope: scope, storage: storage, job: job, checkpoint: checkpoint,
		config:     GraphConfig{Profile: Compatibility, Scope: scope, Jobs: storage, Source: storage, Factory: storage},
		references: &graphOwnerReferences{storage: storage}, generation: &graphOwnerGeneration{storage: storage, checkpoint: checkpoint},
		reference: &graphOwnerReference{storage: storage, expected: domain.HashContent([]byte("independent graph reference"))}}
}

func (fixture *graphOwnerFixture) worker(test *testing.T) *Graph {
	test.Helper()
	worker, err := NewGraph(fixture.config)
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

func (fixture *graphOwnerFixture) withReferences() {
	fixture.config.References, fixture.config.Generations, fixture.config.Reference = fixture.references, fixture.generation, fixture.reference
}

func (fixture *graphOwnerFixture) capture(cursor string, applied int64) domain.GraphProjectionLease {
	job := fixture.job
	job.Phase, job.InputSequence, job.Cursor, job.AppliedSequence = "capture", -1, cursor, applied
	return job
}

func graphOwnerPage(checkpoint domain.RepositoryViewSourceCheckpoint, mode string, entries []domain.RepositoryViewSourceEntry, cursor string) domain.RepositoryViewSourcePage {
	page := domain.RepositoryViewSourcePage{Version: checkpoint.Version, Scope: checkpoint.Scope, RepoID: checkpoint.RepoID,
		Epoch: checkpoint.Epoch, Through: checkpoint.Sequence, Mode: mode, Entries: entries, NextCursor: cursor}
	if cursor == "" {
		page.Checkpoint = &checkpoint
	}
	return page
}

func assertGraphOwnerReleased(test *testing.T, fixture *graphOwnerFixture) {
	test.Helper()
	if fixture.scope.activeWork != 0 || fixture.scope.activeRead != 0 || fixture.scope.activeBuild != 0 || fixture.scope.builds != fixture.scope.releases {
		test.Fatal("graph worker retained work, read, or build ownership", fixture.scope)
	}
}

type graphOwnerRunObservation struct {
	at          time.Time
	claims      int
	saved       int
	failures    int
	workCalls   int
	activeWork  int
	activeRead  int
	activeBuild int
	builds      int
	releases    int
}

func observeGraphOwnerRun(ctx context.Context, fixture *graphOwnerFixture) <-chan graphOwnerRunObservation {
	observations := make(chan graphOwnerRunObservation)
	fixture.scope.afterWork = func() {
		observation := graphOwnerRunObservation{at: time.Now(), claims: fixture.storage.calls["claim"],
			saved: len(fixture.storage.saved), failures: len(fixture.storage.failures), workCalls: fixture.scope.workCalls,
			activeWork: fixture.scope.activeWork, activeRead: fixture.scope.activeRead, activeBuild: fixture.scope.activeBuild,
			builds: fixture.scope.builds, releases: fixture.scope.releases}
		select {
		case observations <- observation:
		case <-ctx.Done():
		}
	}
	return observations
}

func nextGraphOwnerRunObservation(test *testing.T, observations <-chan graphOwnerRunObservation, done <-chan struct{}) graphOwnerRunObservation {
	test.Helper()
	select {
	case observation := <-observations:
		if observation.activeWork != 0 || observation.activeRead != 0 || observation.activeBuild != 0 || observation.builds != observation.releases {
			test.Fatal("graph worker retained ownership after a polled lease", observation)
		}
		return observation
	case <-done:
		test.Fatal("graph worker stopped before the expected poll")
		return graphOwnerRunObservation{}
	}
}

func assertGraphOwnerRunIdle(test *testing.T, observations <-chan graphOwnerRunObservation) {
	test.Helper()
	synctest.Wait()
	select {
	case observation := <-observations:
		test.Fatal("graph worker exceeded its batch or polled before the interval", observation)
	default:
	}
}

func TestGraphOwnerConstructorContracts(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*GraphConfig)
		want   error
	}{
		{name: "missing profile", change: func(config *GraphConfig) { config.Profile = "" }, want: domain.ErrValidation},
		{name: "unknown profile", change: func(config *GraphConfig) { config.Profile = "other" }, want: domain.ErrValidation},
		{name: "missing scope", change: func(config *GraphConfig) { config.Scope = nil }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil scope", change: func(config *GraphConfig) { config.Scope = (*graphOwnerScope)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "missing jobs", change: func(config *GraphConfig) { config.Jobs = nil }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil jobs", change: func(config *GraphConfig) { config.Jobs = (*graphOwnerStore)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "missing source", change: func(config *GraphConfig) { config.Source = nil }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil source", change: func(config *GraphConfig) { config.Source = (*graphOwnerStore)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "missing factory", change: func(config *GraphConfig) { config.Factory = nil }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil factory", change: func(config *GraphConfig) { config.Factory = (*graphOwnerStore)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil references", change: func(config *GraphConfig) { config.References = (*graphOwnerReferences)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil generation", change: func(config *GraphConfig) { config.Generations = (*graphOwnerGeneration)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "typed nil reference", change: func(config *GraphConfig) { config.Reference = (*graphOwnerReference)(nil) }, want: domain.ErrCatalogUnsupported},
		{name: "missing generation for references", change: func(config *GraphConfig) { config.Generations = nil }, want: domain.ErrCatalogUnsupported},
		{name: "missing reference for references", change: func(config *GraphConfig) { config.Reference = nil }, want: domain.ErrCatalogUnsupported},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			fixture.withReferences()
			fixture.config.Profile = Complete
			scenario.change(&fixture.config)
			worker, err := NewGraph(fixture.config)
			if worker != nil || !errors.Is(err, scenario.want) || len(fixture.storage.calls) != 0 || fixture.scope.workerCalls != 0 {
				test.Fatal("invalid declaration was accepted or performed work", worker, err, fixture.storage.calls)
			}
		})
	}
	fixture := newGraphOwnerFixture(test)
	fixture.config.Profile = Complete
	fixture.worker(test).Run(context.Background())
	fixture.withReferences()
	fixture.worker(test).Run(context.Background())
	if len(fixture.storage.calls) != 0 || fixture.scope.workerCalls != 0 {
		test.Fatal("construction or disabled Run started graph work")
	}
}

func TestGraphOwnerCompatibilityKeepsLazyCapabilitiesAndLimitPriority(test *testing.T) {
	for _, scenario := range []string{"no jobs", "no source", "no factory"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			switch scenario {
			case "no jobs":
				fixture.config.Jobs, fixture.config.Source, fixture.config.Factory = nil, nil, nil
			case "no source":
				fixture.config.Source = nil
			case "no factory":
				fixture.config.Factory = nil
			}
			worker := fixture.worker(test)
			for _, limit := range []int{-1, 0, 129} {
				if err := worker.Process(context.Background(), limit); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("capability check displaced batch validation", limit, err)
				}
			}
			err := worker.Process(context.Background(), 1)
			if scenario == "no jobs" && err != nil || scenario != "no jobs" && !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("compatibility capability selection changed", err)
			}
			if len(fixture.storage.calls) != 0 || fixture.scope.workerCalls != 0 || fixture.scope.workCalls != 0 {
				test.Fatal("missing capability started admitted work", fixture.storage.calls)
			}
		})
	}
	for _, scenario := range []string{"bad page", "missing generation", "missing reference", "continuation", "project"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			fixture.withReferences()
			fixture.config.Reference = nil
			job := fixture.capture("", -1)
			page := graphOwnerPage(fixture.checkpoint, "baseline", fixture.storage.inputs, "")
			want := domain.ErrCatalogUnsupported
			switch scenario {
			case "bad page":
				fixture.config.Generations, page.Mode, want = nil, "delta", domain.ErrIntegrity
			case "missing generation":
				fixture.config.Generations = nil
			case "continuation":
				fixture.config.Generations, job.Cursor, want = nil, "continuation", nil
			case "project":
				fixture.config.Generations, job, want = nil, fixture.job, nil
			}
			fixture.storage.jobs, fixture.storage.pages = []domain.GraphProjectionLease{job}, []domain.RepositoryViewSourcePage{page}
			err := fixture.worker(test).Process(context.Background(), 1)
			if !errors.Is(err, want) || len(fixture.references.references) != 0 {
				test.Fatal("optional reference dependencies were checked eagerly or bypassed", err)
			}
			if scenario == "bad page" && fixture.storage.calls["generation"] != 0 || fixture.storage.calls["reference"] != 0 {
				test.Fatal("invalid or continuation page computed a reference", fixture.storage.calls)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerCapturesOnePagePerLeaseAndPublishesFrozenInputs(test *testing.T) {
	fixture := newGraphOwnerFixture(test)
	first, second := fixture.capture("", -1), fixture.capture("next", -1)
	second.Fence++
	fixture.job.Fence = second.Fence + 1
	fixture.storage.jobs = []domain.GraphProjectionLease{first, second, fixture.job}
	fixture.storage.pages = []domain.RepositoryViewSourcePage{
		graphOwnerPage(fixture.checkpoint, "baseline", fixture.storage.inputs[:2], "next"),
		graphOwnerPage(fixture.checkpoint, "baseline", fixture.storage.inputs[2:], ""),
	}
	original, err := json.Marshal(fixture.storage.inputs)
	if err != nil {
		test.Fatal(err)
	}
	fixture.storage.step = func(ctx context.Context, stage string) error {
		if ctx.Value(graphOwnerWorkKey{}) != true || fixture.scope.activeWork != 1 {
			test.Fatal("capture, factory, or evidence escaped admitted work ownership", stage)
		}
		return nil
	}
	worker := fixture.worker(test)
	for processed := 1; processed <= 2; processed++ {
		if err := worker.Process(context.Background(), 1); err != nil || len(fixture.storage.saved) != processed || fixture.storage.calls["source"] != processed || fixture.storage.calls["load"] != 0 || fixture.storage.calls["factory"] != 0 {
			test.Fatal("capture projected inputs or drained multiple pages", err, fixture.storage.calls)
		}
		assertGraphOwnerReleased(test, fixture)
	}
	for index, request := range fixture.storage.requests {
		if request.Limit != domain.DefaultRepositoryViewSourceLimit || request.After != nil || index == 1 && request.Cursor != "next" {
			test.Fatal("capture lost the durable graph cursor or page bound", request)
		}
	}
	if err := fixture.worker(test).Process(context.Background(), 128); err != nil || fixture.storage.calls["claim"] != 4 || fixture.storage.calls["publish"] != 1 {
		test.Fatal("restarted owner did not publish once and stop at the empty queue", err, fixture.storage.calls)
	}
	got, err := domain.DecodeGraphViewProjection(context.Background(), fixture.job.RepoID, fixture.storage.published)
	if err != nil || got.Graph == nil || got.Revision.Graph != 4 || len(got.Graph.SnapshotIDs) != 1 || got.Graph.HoldCounts["main"] != 1 || got.Graph.PositionEvent != "" {
		test.Fatal("published graph does not contain the captured generation", got, err)
	}
	if fixture.storage.metadata.DefaultBranch != "main" || fixture.storage.checkpoint != fixture.checkpoint || len(fixture.storage.proof) != 1 || fixture.storage.proof[0].Kind != "git_tree" || fixture.storage.publishJob != fixture.job || !reflect.DeepEqual(fixture.storage.loaded, []domain.GraphProjectionLease{fixture.job}) {
		test.Fatal("projection lost source generation, evidence, or lease fencing", fixture.storage)
	}
	metadata, view, proof, err := repositoryquery.DecodeCapturedGraphInputs(context.Background(), fixture.checkpoint, fixture.storage.inputs)
	if err != nil {
		test.Fatal(err)
	}
	evidence, err := store.NewGraphSourceReader(context.Background(), fixture.checkpoint, metadata, proof)
	if err != nil {
		test.Fatal(err)
	}
	want, err := repositoryquery.ProjectCapturedRepositoryView(context.Background(), fixture.checkpoint, view, evidence, fixture.scope)
	if err != nil || !reflect.DeepEqual(got, want) {
		test.Fatal("worker differs from the captured graph projection", err, got, want)
	}
	after, err := json.Marshal(fixture.storage.inputs)
	if err != nil || string(original) != string(after) || fixture.scope.checks != 3 || len(fixture.storage.failures) != 0 || fixture.scope.builds != 0 {
		test.Fatal("worker mutated captured inputs or changed repository/work ownership", err, fixture.storage.calls)
	}
	assertGraphOwnerReleased(test, fixture)
}

func TestGraphOwnerCapturesFirstPageReferenceWithinOnePinnedRead(test *testing.T) {
	for _, applied := range []int64{-1, 3} {
		test.Run(map[int64]string{-1: "baseline", 3: "delta"}[applied], func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			fixture.withReferences()
			fixture.config.Profile = Complete
			first, second := fixture.capture("", applied), fixture.capture("next", applied)
			second.Fence++
			mode := map[int64]string{-1: "baseline", 3: "delta"}[applied]
			firstPage := graphOwnerPage(fixture.checkpoint, mode, fixture.storage.inputs[:1], "next")
			fixture.storage.jobs = []domain.GraphProjectionLease{first, second}
			fixture.storage.pages = []domain.RepositoryViewSourcePage{firstPage, graphOwnerPage(fixture.checkpoint, mode, nil, "")}
			fixture.storage.step = func(ctx context.Context, stage string) error {
				if stage == "generation" || stage == "reference" || stage == "source" && fixture.storage.calls["source"] == 1 {
					if ctx.Value(graphOwnerReadKey{}) != true || fixture.scope.activeRead != 1 {
						test.Fatal("first page escaped its pinned read", stage)
					}
				}
				if stage == "save-reference" && (ctx.Value(graphOwnerReadKey{}) != nil || fixture.scope.activeRead != 0) {
					test.Fatal("reference publication retained the source read")
				}
				return nil
			}
			if err := fixture.worker(test).Process(context.Background(), 2); err != nil {
				test.Fatal(err)
			}
			want := domain.GraphProjectionReference{GraphQualificationKey: domain.GraphQualificationKey{
				RepoID: first.RepoID, Dataset: first.Dataset, Applied: fixture.checkpoint,
				Rules: domain.GraphViewProjectionRules, ComparisonRules: domain.GraphQualificationRules}, Expected: fixture.reference.expected}
			if !reflect.DeepEqual(fixture.references.references, []domain.GraphProjectionReference{want}) || !reflect.DeepEqual(fixture.references.jobs, []domain.GraphProjectionLease{first}) || !reflect.DeepEqual(fixture.references.pages, []domain.RepositoryViewSourcePage{firstPage}) || !reflect.DeepEqual(fixture.storage.saved, []domain.GraphProjectionLease{second}) {
				test.Fatal("first page and independent reference were not saved atomically", fixture.references)
			}
			if fixture.scope.readCalls != 1 || fixture.storage.calls["generation"] != 1 || fixture.storage.calls["reference"] != 1 || fixture.storage.requests[1].After != nil || fixture.storage.requests[1].Cursor != "next" {
				test.Fatal("continuation recaptured generation or lost its cursor", fixture.storage.calls, fixture.storage.requests)
			}
			request := fixture.storage.requests[0]
			if applied < 0 && request.After != nil || applied >= 0 && (request.After == nil || request.After.Scope != domain.RepositoryGraphSourceScope || request.After.Sequence != applied) || request.Limit != domain.DefaultRepositoryViewSourceLimit {
				test.Fatal("first page lost baseline/delta graph source semantics", request)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerFirstPageRejectsIncoherentGenerationAndReturnsNoPartialCapture(test *testing.T) {
	for _, scenario := range []string{"phase", "cursor", "source", "page", "missing generation", "generation", "version", "scope", "repo", "epoch", "sequence", "missing reference", "reference", "invalid digest", "invalid dataset", "read finish"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			job := fixture.capture("", -1)
			page := graphOwnerPage(fixture.checkpoint, "baseline", nil, "")
			generation := outbound.RepositoryGraphSourceGenerationStore(fixture.generation)
			reference := ReferenceReader(fixture.reference)
			want := domain.ErrConflict
			switch scenario {
			case "phase":
				job.Phase, want = "project", domain.ErrValidation
			case "cursor":
				job.Cursor, want = "continuation", domain.ErrValidation
			case "source":
				want = domain.ErrNotFound
				fixture.storage.step = func(context.Context, string) error { return want }
			case "page":
				page.Mode, want = "delta", domain.ErrIntegrity
			case "missing generation":
				generation, want = nil, domain.ErrCatalogUnsupported
			case "generation":
				want = domain.ErrCatalogResetRequired
				fixture.storage.step = func(_ context.Context, stage string) error {
					if stage == "generation" {
						return want
					}
					return nil
				}
			case "version":
				fixture.generation.checkpoint.Version++
			case "scope":
				fixture.generation.checkpoint.Scope = domain.RepositoryViewSourceScope
			case "repo":
				fixture.generation.checkpoint.RepoID = domain.HashContent([]byte("other repository"))
			case "epoch":
				fixture.generation.checkpoint.Epoch = "33333333-3333-4333-8333-333333333333"
			case "sequence":
				fixture.generation.checkpoint.Sequence++
			case "missing reference":
				reference, want = nil, domain.ErrCatalogUnsupported
			case "reference":
				want = domain.ErrRepositoryViewSourceLimit
				fixture.storage.step = func(_ context.Context, stage string) error {
					if stage == "reference" {
						return want
					}
					return nil
				}
			case "invalid digest":
				fixture.reference.expected, want = "invalid", domain.ErrValidation
			case "invalid dataset":
				job.Dataset, want = "invalid", domain.ErrValidation
			case "read finish":
				fixture.scope.readFinishErr = want
			}
			fixture.storage.pages = []domain.RepositoryViewSourcePage{page}
			gotPage, gotReference, err := CaptureFirstPage(context.Background(), fixture.scope, fixture.storage, generation, reference, job)
			if !errors.Is(err, want) || !reflect.DeepEqual(gotPage, domain.RepositoryViewSourcePage{}) || gotReference != (domain.GraphProjectionReference{}) {
				test.Fatal("failed first page exposed a partial capture", gotPage, gotReference, err)
			}
			if scenario == "phase" || scenario == "cursor" {
				if fixture.scope.readCalls != 0 || len(fixture.storage.calls) != 0 {
					test.Fatal("noninitial capture opened a source read")
				}
			} else if scenario != "reference" && scenario != "invalid digest" && scenario != "invalid dataset" && scenario != "read finish" && fixture.storage.calls["reference"] != 0 {
				test.Fatal("reference was computed before page/generation validation", fixture.storage.calls)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerPageValidationPreservesBoundsAndErrorPriority(test *testing.T) {
	for _, scenario := range []string{"version", "scope", "epoch", "repo", "sequence", "mode", "missing checkpoint", "wrong checkpoint", "empty nonfinal", "both cursors", "repeated cursor", "rows", "cursor size", "key size", "image size", "page bytes", "canceled"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			job := fixture.capture("", -1)
			page := graphOwnerPage(fixture.checkpoint, "baseline", fixture.storage.inputs, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := domain.ErrIntegrity
			switch scenario {
			case "version":
				page.Version, want = 2, domain.ErrCatalogResetRequired
				cancel()
			case "scope":
				page.Scope, want = domain.RepositoryViewSourceScope, domain.ErrCatalogResetRequired
			case "epoch":
				page.Epoch, want = "33333333-3333-4333-8333-333333333333", domain.ErrCatalogResetRequired
			case "repo":
				page.RepoID = domain.HashContent([]byte("other"))
			case "sequence":
				job.AppliedSequence, page.Through = fixture.checkpoint.Sequence+1, fixture.checkpoint.Sequence
			case "mode":
				page.Mode = "delta"
			case "missing checkpoint":
				page.Checkpoint = nil
			case "wrong checkpoint":
				page.Checkpoint.Scope = domain.RepositoryViewSourceScope
			case "empty nonfinal":
				page.Entries, page.NextCursor, page.Checkpoint = nil, "next", nil
			case "both cursors":
				page.NextCursor = "next"
			case "repeated cursor":
				job.Cursor, page.NextCursor, page.Checkpoint = "next", "next", nil
			case "rows":
				page.Entries, want = make([]domain.RepositoryViewSourceEntry, domain.DefaultRepositoryViewSourceLimit+1), domain.ErrRepositoryViewSourceLimit
			case "cursor size":
				page.NextCursor, want = strings.Repeat("x", (16<<10)+1), domain.ErrRepositoryViewSourceLimit
			case "key size":
				page.Entries[0].Key, want = strings.Repeat("x", domain.MaxRepositoryViewSourceKeyBytes+1), domain.ErrRepositoryViewSourceLimit
			case "image size":
				page.Entries[0].Value, want = json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes+1)), domain.ErrRepositoryViewSourceLimit
			case "page bytes":
				image := json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes))
				page.Entries, want = make([]domain.RepositoryViewSourceEntry, 4), domain.ErrRepositoryViewSourceLimit
				for index := range page.Entries {
					page.Entries[index].Value = image
				}
			case "canceled":
				page.Entries, want = nil, context.Canceled
				cancel()
			}
			if err := ValidateGraphProjectionPage(ctx, job, page); !errors.Is(err, want) {
				test.Fatal("capture validation changed its bounds or priority", err, want)
			}
		})
	}
}

func TestGraphOwnerSourcePageBudgetRetainsKeyValueAndFixedOverhead(test *testing.T) {
	fixture := newGraphOwnerFixture(test)
	entry := domain.RepositoryViewSourceEntry{Kind: strings.Repeat("kind", 1024), Key: "key", Value: json.RawMessage(`{}`)}
	if got := graphSourceEntryBytes(entry); got != len(entry.Kind)+len(entry.Key)+len(entry.Value)+128 {
		test.Fatal("graph project cost omitted kind or lost fixed overhead", got)
	}
	page := graphOwnerPage(fixture.checkpoint, "baseline", make([]domain.RepositoryViewSourceEntry, 4), "")
	for index := range page.Entries {
		page.Entries[index].Value = json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourcePageBytes/4-128))
	}
	job := fixture.capture("", -1)
	if err := ValidateGraphProjectionPage(context.Background(), job, page); err != nil {
		test.Fatal("exact source page byte budget was rejected", err)
	}
	page.Entries[0].Value = append(page.Entries[0].Value, 'x')
	if err := ValidateGraphProjectionPage(context.Background(), job, page); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		test.Fatal("source page byte budget allowed an extra byte", err)
	}
	page = graphOwnerPage(fixture.checkpoint, "baseline", make([]domain.RepositoryViewSourceEntry, domain.DefaultRepositoryViewSourceLimit), "")
	if err := ValidateGraphProjectionPage(context.Background(), job, page); err != nil {
		test.Fatal("exact source page row budget was rejected", err)
	}
	page.NextCursor, page.Checkpoint = strings.Repeat("x", 16<<10), nil
	if err := ValidateGraphProjectionPage(context.Background(), job, page); err != nil {
		test.Fatal("exact source cursor budget was rejected", err)
	}
}

func TestGraphOwnerRejectsIncompleteCapturedInputsBeforePublish(test *testing.T) {
	for _, scenario := range []string{"cursor", "negative generation", "invalid applied", "already applied", "missing metadata", "future metadata", "future evidence", "negative row", "tombstone", "unknown kind", "invalid proof", "duplicate metadata", "key size", "image size", "row count", "input bytes"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			want := domain.ErrIntegrity
			switch scenario {
			case "cursor":
				fixture.job.Cursor = "unfinished"
			case "negative generation":
				fixture.job.InputSequence, want = -1, domain.ErrValidation
			case "invalid applied":
				fixture.job.AppliedSequence = -2
			case "already applied":
				fixture.job.AppliedSequence = fixture.job.InputSequence
			case "missing metadata":
				fixture.storage.inputs = fixture.storage.inputs[:1]
			case "future metadata":
				fixture.storage.inputs[1].Sequence = fixture.job.InputSequence + 1
			case "future evidence":
				fixture.storage.inputs[0].Sequence = fixture.job.InputSequence + 1
			case "negative row":
				fixture.storage.inputs[0].Sequence = -1
			case "tombstone":
				fixture.storage.inputs[1].Deleted = true
			case "unknown kind":
				fixture.storage.inputs[0].Kind = "unknown"
			case "invalid proof":
				fixture.storage.inputs[0].Value = json.RawMessage(`{}`)
			case "duplicate metadata":
				fixture.storage.inputs = append(fixture.storage.inputs, fixture.storage.inputs[3])
			case "key size":
				fixture.storage.inputs[0].Key, want = strings.Repeat("x", domain.MaxRepositoryViewSourceKeyBytes+1), domain.ErrRepositoryViewSourceLimit
			case "image size":
				fixture.storage.inputs[0].Value, want = json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes+1)), domain.ErrRepositoryViewSourceLimit
			case "row count":
				fixture.storage.inputs, want = make([]domain.RepositoryViewSourceEntry, domain.MaxPendingProjectionInputRows+1), domain.ErrRepositoryViewSourceLimit
			case "input bytes":
				image := json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes))
				fixture.storage.inputs, want = make([]domain.RepositoryViewSourceEntry, domain.MaxPendingProjectionInputBytes/domain.MaxRepositoryViewSourceImageBytes+1), domain.ErrRepositoryViewSourceLimit
				for index := range fixture.storage.inputs {
					fixture.storage.inputs[index] = domain.RepositoryViewSourceEntry{Kind: "snapshot", Sequence: 1, Value: image}
				}
			}
			err := ProjectGraphProjection(context.Background(), fixture.storage, fixture.storage, fixture.job, fixture.scope)
			if !errors.Is(err, want) || fixture.storage.calls["publish"] != 0 || len(fixture.storage.failures) != 0 {
				test.Fatal("invalid captured inputs published or bypassed their error class", err, want, fixture.storage.calls)
			}
			if (scenario == "cursor" || scenario == "negative generation" || scenario == "invalid applied" || scenario == "already applied") && fixture.storage.calls["load"] != 0 {
				test.Fatal("invalid lease loaded captured inputs", fixture.storage.calls)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerProjectBudgetIncludesKind(test *testing.T) {
	fixture := newGraphOwnerFixture(test)
	count := domain.MaxPendingProjectionInputBytes / domain.MaxRepositoryViewSourceImageBytes
	value := json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes-128))
	fixture.storage.inputs = make([]domain.RepositoryViewSourceEntry, count)
	for index := range fixture.storage.inputs {
		fixture.storage.inputs[index] = domain.RepositoryViewSourceEntry{Kind: "snapshot", Sequence: 1, Value: value}
	}
	entry := fixture.storage.inputs[0]
	if got := graphSourceEntryBytes(entry); got != len(entry.Kind)+len(entry.Key)+len(entry.Value)+128 {
		test.Fatal("project accounting omitted kind bytes before decoding", got)
	}
	if err := ProjectGraphProjection(context.Background(), fixture.storage, fixture.storage, fixture.job, fixture.scope); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		test.Fatal("project input kind bytes bypassed the original input budget", err)
	}
	if fixture.storage.calls["publish"] != 0 || fixture.storage.calls["factory"] != 0 {
		test.Fatal("oversized project reached decoding or publication", fixture.storage.calls)
	}
	assertGraphOwnerReleased(test, fixture)
}

func TestGraphOwnerFailureClassificationPreservesCausesAndLeaseFence(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		cause  error
		reason string
		retry  bool
	}{
		{name: "temporary", cause: errors.New("temporary"), reason: "temporary_failure", retry: true},
		{name: "operation cancellation", cause: context.Canceled, reason: "temporary_failure", retry: true},
		{name: "operation deadline", cause: context.DeadlineExceeded, reason: "temporary_failure", retry: true},
		{name: "reset", cause: domain.ErrCatalogResetRequired, reason: "source_reset", retry: true},
		{name: "conflict", cause: domain.ErrConflict, reason: "lease_lost", retry: true},
		{name: "ref conflict", cause: domain.ErrRefConflict, reason: "lease_lost", retry: true},
		{name: "limit", cause: domain.ErrRepositoryViewSourceLimit, reason: "resource_limit"},
		{name: "integrity", cause: domain.ErrIntegrity, reason: "invalid_source"},
		{name: "validation", cause: domain.ErrValidation, reason: "invalid_source"},
		{name: "missing claimed input", cause: domain.ErrNotFound, reason: "invalid_source"},
		{name: "forbidden", cause: domain.ErrForbidden, reason: "access_denied"},
		{name: "identity", cause: domain.ErrDocumentIdentityUpgradeRequired, reason: "identity_upgrade_required", retry: true},
		{name: "reset priority", cause: errors.Join(domain.ErrConflict, domain.ErrCatalogResetRequired), reason: "source_reset", retry: true},
		{name: "fence priority", cause: errors.Join(domain.ErrIntegrity, domain.ErrConflict), reason: "lease_lost", retry: true},
		{name: "limit priority", cause: errors.Join(domain.ErrIntegrity, domain.ErrRepositoryViewSourceLimit), reason: "resource_limit"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			finishErr := errors.New("failure fence refused")
			fixture.storage.step = func(ctx context.Context, stage string) error {
				if stage == "load" {
					return scenario.cause
				}
				if stage == "fail" {
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) > 5*time.Second || ctx.Err() != nil || ctx.Value(graphOwnerWorkKey{}) != true {
						test.Fatal("failure write lost independent budget or work ownership", ctx.Err())
					}
					return finishErr
				}
				return nil
			}
			err := fixture.worker(test).Process(context.Background(), 1)
			want := graphOwnerFailure{job: fixture.job, reason: scenario.reason, retry: scenario.retry}
			if !errors.Is(err, scenario.cause) || !errors.Is(err, finishErr) || !reflect.DeepEqual(fixture.storage.failures, []graphOwnerFailure{want}) || fixture.storage.calls["publish"] != 0 {
				test.Fatal("failure lost its original cause, classification, or lease fence", err, fixture.storage.failures)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerRunLeaseChecksIdentityAndDoesNotClaimOrReadLiveSource(test *testing.T) {
	for _, phase := range []string{"project", "unknown"} {
		test.Run(phase, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			fixture.job.Phase = phase
			fixture.storage.step = func(_ context.Context, stage string) error {
				if stage == "identity" {
					return domain.ErrDocumentIdentityUpgradeRequired
				}
				return nil
			}
			err := fixture.worker(test).RunLease(context.Background(), fixture.job)
			want := graphOwnerFailure{job: fixture.job, reason: "identity_upgrade_required", retry: true}
			if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(fixture.storage.failures, []graphOwnerFailure{want}) || fixture.scope.checks != 1 || fixture.storage.calls["claim"] != 0 || fixture.storage.calls["source"] != 0 || fixture.storage.calls["load"] != 0 {
				test.Fatal("direct lease execution bypassed identity or started a second claim", err, fixture.storage.calls)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
	fixture := newGraphOwnerFixture(test)
	fixture.job.Phase = "unknown"
	if err := fixture.worker(test).RunLease(context.Background(), fixture.job); !errors.Is(err, domain.ErrIntegrity) || len(fixture.storage.failures) != 1 || fixture.storage.failures[0].retry || fixture.storage.failures[0].reason != "invalid_source" {
		test.Fatal("unknown phase escaped invalid-source failure handling", err, fixture.storage.failures)
	}
}

func TestGraphOwnerFailuresPreserveOldPublicationAndReleaseResources(test *testing.T) {
	for _, stage := range []string{"identity", "source", "save", "generation", "reference", "save-reference", "load", "factory", "evidence", "publish"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			job := fixture.job
			if stage == "source" || stage == "save" || stage == "generation" || stage == "reference" || stage == "save-reference" {
				job = fixture.capture("", -1)
				fixture.storage.pages = []domain.RepositoryViewSourcePage{graphOwnerPage(fixture.checkpoint, "baseline", nil, "")}
				if stage == "generation" || stage == "reference" || stage == "save-reference" {
					fixture.withReferences()
				}
			}
			old := []domain.GraphProjectionPart{{Kind: "old", Key: "generation", Value: json.RawMessage(`{}`)}}
			fixture.storage.published, fixture.storage.jobs = old, []domain.GraphProjectionLease{job}
			fixture.storage.step = func(ctx context.Context, current string) error {
				if ctx.Value(graphOwnerWorkKey{}) != true || fixture.scope.activeWork != 1 {
					test.Fatal("failed graph phase escaped admitted work ownership", current)
				}
				if current == stage {
					return domain.ErrConflict
				}
				return nil
			}
			if err := fixture.worker(test).Process(context.Background(), 1); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(fixture.storage.published, old) || len(fixture.storage.failures) != 1 || fixture.storage.failures[0].job != job {
				test.Fatal("failed phase replaced old publication or lost its lease", err, fixture.storage.calls)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerCancellationDoesNotFailLease(test *testing.T) {
	for _, stage := range []string{"admission", "claim", "identity", "source", "save", "generation", "reference", "save-reference", "load", "factory", "evidence", "publish"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := fixture.job
			if stage == "source" || stage == "save" || stage == "generation" || stage == "reference" || stage == "save-reference" {
				job = fixture.capture("", -1)
				fixture.storage.pages = []domain.RepositoryViewSourcePage{graphOwnerPage(fixture.checkpoint, "baseline", nil, "")}
				if stage == "generation" || stage == "reference" || stage == "save-reference" {
					fixture.withReferences()
				}
			}
			fixture.storage.jobs = []domain.GraphProjectionLease{job}
			fixture.scope.beforeWork = func(context.Context) error {
				if stage == "admission" {
					cancel()
				}
				return ctx.Err()
			}
			fixture.storage.step = func(bound context.Context, current string) error {
				if bound.Value(graphOwnerWorkKey{}) != true || fixture.scope.activeWork != 1 {
					test.Fatal("canceled graph phase escaped admitted work ownership", current)
				}
				if current == stage {
					cancel()
				}
				return nil
			}
			if err := fixture.worker(test).Process(ctx, 1); !errors.Is(err, context.Canceled) || len(fixture.storage.failures) != 0 || len(fixture.storage.published) != 0 || len(fixture.references.references) != 0 || len(fixture.storage.saved) != 0 {
				test.Fatal("shutdown failed or advanced the claimed lease", err, fixture.storage.calls, fixture.storage.failures)
			}
			if stage == "admission" && fixture.storage.calls["claim"] != 0 {
				test.Fatal("shutdown claimed work before admission")
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerFirstPageCancellationReturnsNoReference(test *testing.T) {
	for _, stage := range []string{"source", "generation", "reference"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newGraphOwnerFixture(test)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture.storage.pages = []domain.RepositoryViewSourcePage{graphOwnerPage(fixture.checkpoint, "baseline", nil, "")}
			fixture.storage.step = func(_ context.Context, current string) error {
				if current == stage {
					cancel()
				}
				return nil
			}
			page, reference, err := CaptureFirstPage(ctx, fixture.scope, fixture.storage, fixture.generation, fixture.reference, fixture.capture("", -1))
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) || reference != (domain.GraphProjectionReference{}) {
				test.Fatal("canceled capture exposed a partial page or reference", page, reference, err)
			}
			assertGraphOwnerReleased(test, fixture)
		})
	}
}

func TestGraphOwnerWorkerContextAndDeadlinesRemainWithinWork(test *testing.T) {
	fixture := newGraphOwnerFixture(test)
	fixture.storage.step = func(ctx context.Context, stage string) error {
		if _, system := inbound.RepositoryActor(ctx); !system || ctx.Value(graphOwnerWorkerKey{}) != true || ctx.Value(graphOwnerWorkKey{}) != true || fixture.scope.activeWork != 1 {
			test.Fatal("worker actor, context, or admitted ownership was lost", stage)
		}
		if !reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) {
			test.Fatal("worker inherited caller document capabilities", stage)
		}
		if err := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityLegacy); err != nil {
			test.Fatal("worker lost document compatibility", stage, err)
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) > 30*time.Second {
			test.Fatal("unbounded graph worker operation", stage)
		}
		return nil
	}
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := fixture.worker(test).Process(ctx, 1); err != nil || fixture.storage.calls["publish"] != 1 || fixture.scope.workerCalls != 1 {
		test.Fatal("worker did not publish through its own context", err, fixture.storage.calls)
	}
	assertGraphOwnerReleased(test, fixture)
}

func TestGraphOwnerDeadlinesStartAfterAdmission(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		stage   string
		queue   time.Duration
		parent  time.Duration
		budget  time.Duration
		failure bool
	}{
		{name: "claim", stage: "claim", budget: 30 * time.Second},
		{name: "queued claim", stage: "claim", queue: 20 * time.Second, budget: 30 * time.Second},
		{name: "work", stage: "load", budget: 30 * time.Second, failure: true},
		{name: "queued work", stage: "load", queue: 20 * time.Second, budget: 30 * time.Second, failure: true},
		{name: "parent claim", stage: "claim", parent: 5 * time.Second, budget: 5 * time.Second},
		{name: "parent work", stage: "load", parent: 5 * time.Second, budget: 5 * time.Second},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newGraphOwnerFixture(test)
				ctx := context.Background()
				if scenario.parent != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, scenario.parent)
					defer cancel()
				}
				fixture.scope.beforeWork = func(context.Context) error {
					if fixture.storage.calls["claim"] != 0 {
						test.Fatal("lease claimed before admission")
					}
					time.Sleep(scenario.queue)
					return nil
				}
				var observed time.Duration
				fixture.storage.step = func(bound context.Context, stage string) error {
					if stage == scenario.stage {
						deadline, bounded := bound.Deadline()
						if !bounded {
							test.Fatal("missing operation deadline")
						}
						observed = time.Until(deadline)
						<-bound.Done()
						return bound.Err()
					}
					if stage == "fail" {
						deadline, bounded := bound.Deadline()
						if !bounded || time.Until(deadline) != 5*time.Second || bound.Err() != nil {
							test.Fatal("failure inherited expired operation budget", bound.Err())
						}
					}
					return nil
				}
				start := time.Now()
				if err := fixture.worker(test).Process(ctx, 1); !errors.Is(err, context.DeadlineExceeded) || observed != scenario.budget || time.Since(start) != scenario.queue+scenario.budget {
					test.Fatal("claim/work budget included admission wait or ignored parent", err, observed, time.Since(start))
				}
				if (len(fixture.storage.failures) == 1) != scenario.failure || scenario.failure && fixture.storage.failures[0].reason != "temporary_failure" {
					test.Fatal("operation timeout and parent cancellation were conflated", fixture.storage.failures)
				}
				assertGraphOwnerReleased(test, fixture)
			})
		})
	}
}

func TestGraphOwnerRunPreservesDisabledDefaultBatchAndPolling(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newGraphOwnerFixture(test)
		fixture.worker(test).Run(context.Background())
		if len(fixture.storage.calls) != 0 || fixture.scope.workCalls != 0 {
			test.Fatal("disabled worker performed work")
		}
		fixture.config.Enabled = true
		fixture.storage.jobs = nil
		for index := 0; index < 32; index++ {
			job := fixture.capture("", -1)
			job.Fence += int64(index)
			fixture.storage.jobs = append(fixture.storage.jobs, job)
			fixture.storage.pages = append(fixture.storage.pages, graphOwnerPage(fixture.checkpoint, "baseline", nil, ""))
		}
		worker := fixture.worker(test)
		ctx, cancel := context.WithCancel(context.Background())
		observations := observeGraphOwnerRun(ctx, fixture)
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		start := time.Now()
		go func() { worker.Run(ctx); close(done) }()
		for processed := 1; processed <= 16; processed++ {
			observation := nextGraphOwnerRunObservation(test, observations, done)
			if observation.claims != processed || observation.saved != processed || observation.workCalls != processed || observation.failures != 0 || !observation.at.Equal(start) {
				test.Fatal("worker did not poll immediately in a batch of sixteen", observation)
			}
		}
		assertGraphOwnerRunIdle(test, observations)
		time.Sleep(time.Second)
		for processed := 17; processed <= 32; processed++ {
			observation := nextGraphOwnerRunObservation(test, observations, done)
			if observation.claims != processed || observation.saved != processed || observation.workCalls != processed || observation.failures != 0 || !observation.at.Equal(start.Add(time.Second)) {
				test.Fatal("worker changed its one-second polling interval or batch of sixteen", observation)
			}
		}
		assertGraphOwnerRunIdle(test, observations)
		time.Sleep(time.Second)
		observation := nextGraphOwnerRunObservation(test, observations, done)
		if observation.claims != 33 || observation.saved != 32 || observation.workCalls != 33 || observation.failures != 0 || !observation.at.Equal(start.Add(2*time.Second)) {
			test.Fatal("empty queue did not stop the batch at the next one-second poll", observation)
		}
		assertGraphOwnerRunIdle(test, observations)
		cancel()
		<-done
		if fixture.storage.calls["claim"] != 33 {
			test.Fatal("worker claimed after shutdown", fixture.storage.calls)
		}
		assertGraphOwnerReleased(test, fixture)
	})
}

func TestGraphOwnerRunContinuesPollingAfterFailure(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newGraphOwnerFixture(test)
		fixture.config.Enabled, fixture.storage.jobs = true, nil
		fixture.storage.step = func(_ context.Context, stage string) error {
			if stage == "claim" && fixture.storage.calls["claim"] == 1 {
				return domain.ErrConflict
			}
			return nil
		}
		worker := fixture.worker(test)
		ctx, cancel := context.WithCancel(context.Background())
		observations := observeGraphOwnerRun(ctx, fixture)
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		start := time.Now()
		go func() { worker.Run(ctx); close(done) }()
		observation := nextGraphOwnerRunObservation(test, observations, done)
		if observation.claims != 1 || observation.saved != 0 || observation.workCalls != 1 || observation.failures != 0 || !observation.at.Equal(start) {
			test.Fatal("worker did not perform its first poll immediately", observation)
		}
		assertGraphOwnerRunIdle(test, observations)
		time.Sleep(time.Second)
		observation = nextGraphOwnerRunObservation(test, observations, done)
		if observation.claims != 2 || observation.saved != 0 || observation.workCalls != 2 || observation.failures != 0 || !observation.at.Equal(start.Add(time.Second)) {
			test.Fatal("claim failure changed retry timing or failed an unclaimed lease", observation)
		}
		assertGraphOwnerRunIdle(test, observations)
		cancel()
		<-done
		if fixture.storage.calls["claim"] != 2 || len(fixture.storage.failures) != 0 {
			test.Fatal("worker claimed or failed a lease after shutdown", fixture.storage.calls)
		}
		assertGraphOwnerReleased(test, fixture)
	})
}
