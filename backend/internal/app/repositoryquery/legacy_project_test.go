package repositoryquery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var _ ContributionEvidence = (*evidencequery.Queries)(nil)

type legacyProjectScope struct {
	pinned bool
	checks int
}

func (scope *legacyProjectScope) IsPinnedRead(context.Context) bool {
	scope.checks++
	return scope.pinned
}

type legacyProjectGeneration struct {
	value  domain.RepositoryViewSourceCheckpoint
	err    error
	calls  int
	repo   domain.ContentHash
	cancel context.CancelFunc
}

func (reader *legacyProjectGeneration) ReadRepositoryGraphSourceGeneration(_ context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	reader.calls++
	reader.repo = repo
	if reader.cancel != nil {
		reader.cancel()
	}
	return reader.value, reader.err
}

type legacyProjectMetadata struct {
	value domain.Repo
	err   error
	calls int
	repo  domain.ContentHash
	bound context.Context
}

func (reader *legacyProjectMetadata) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	reader.calls++
	reader.repo, reader.bound = repo, ctx
	return reader.value, reader.err
}

type legacyProjectEvidence struct {
	prs        []evidencequery.VerifiedPRContribution
	git        []evidencequery.VerifiedGitContribution
	prErr      error
	gitErr     error
	prCalls    int
	gitCalls   int
	repo       domain.ContentHash
	history    []domain.HistoryEvent
	bound      context.Context
	cancelOnPR context.CancelFunc
}

func (reader *legacyProjectEvidence) VerifiedPR(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]evidencequery.VerifiedPRContribution, error) {
	reader.prCalls++
	reader.repo, reader.history, reader.bound = repo, history, ctx
	if reader.cancelOnPR != nil {
		reader.cancelOnPR()
	}
	return reader.prs, reader.prErr
}

func (reader *legacyProjectEvidence) VerifiedGit(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]evidencequery.VerifiedGitContribution, error) {
	reader.gitCalls++
	reader.repo, reader.history, reader.bound = repo, history, ctx
	return reader.git, reader.gitErr
}

type legacyProjectTrees struct {
	commits map[string]domain.GitCommitTree
	err     error
	calls   int
	repo    domain.ContentHash
	origin  string
}

func (reader *legacyProjectTrees) GetGitCommitTree(_ context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	reader.calls++
	reader.repo, reader.origin = repo, origin
	if reader.err != nil {
		return domain.GitCommitTree{}, reader.err
	}
	commit, found := reader.commits[identifier]
	if !found {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	return commit, nil
}

func (reader *legacyProjectTrees) GetGitTreeNode(context.Context, domain.ContentHash, string, string) (domain.GitTreeNode, error) {
	return domain.GitTreeNode{}, domain.ErrNotFound
}

type legacyProjectBuildKey struct{}

type legacyProjectResources struct {
	test      *testing.T
	err       error
	attempts  int
	acquires  int
	reentries int
	releases  int
	active    bool
	onAcquire func()
}

func (resources *legacyProjectResources) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	resources.test.Helper()
	resources.attempts++
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if ctx.Value(legacyProjectBuildKey{}) == resources {
		resources.reentries++
		return ctx, func() {}, nil
	}
	if resources.err != nil {
		return ctx, nil, resources.err
	}
	if resources.active {
		resources.test.Fatal("projection introduced a non-reentrant build scope")
	}
	resources.active = true
	resources.acquires++
	if resources.onAcquire != nil {
		resources.onAcquire()
	}
	return context.WithValue(ctx, legacyProjectBuildKey{}, resources), func() {
		if !resources.active {
			resources.test.Fatal("build permit released twice")
		}
		resources.active = false
		resources.releases++
	}, nil
}

func legacyProjectHash(value string) domain.ContentHash {
	return domain.HashContent([]byte(value))
}

func legacyProjectOID(number int) string {
	return fmt.Sprintf("%040x", number)
}

func legacyProjectCheckpoint(repo domain.ContentHash) domain.RepositoryViewSourceCheckpoint {
	return domain.RepositoryViewSourceCheckpoint{
		Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
		RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7,
	}
}

type legacyProjectFixture struct {
	projection *LegacyProjection
	scope      *legacyProjectScope
	generation *legacyProjectGeneration
	metadata   *legacyProjectMetadata
	evidence   *legacyProjectEvidence
	trees      *legacyProjectTrees
	resources  *legacyProjectResources
	view       domain.RepositoryView
	repo       domain.ContentHash
	position   string
	prCapture  domain.ContentHash
	gitCapture domain.ContentHash
}

func newLegacyProjectFixture(test *testing.T) *legacyProjectFixture {
	test.Helper()
	repo := legacyProjectHash("legacy owner repo")
	tip, common := legacyProjectHash("tip"), legacyProjectHash("common")
	fixture := &legacyProjectFixture{
		repo: repo, position: fmt.Sprintf("%032x", 1),
		prCapture: legacyProjectHash("pr capture"), gitCapture: legacyProjectHash("git capture"),
		scope: &legacyProjectScope{pinned: true}, generation: &legacyProjectGeneration{value: legacyProjectCheckpoint(repo)},
		metadata: &legacyProjectMetadata{value: domain.Repo{ID: repo, GitRemoteURL: "https://example.invalid/legacy.git"}},
		evidence: &legacyProjectEvidence{}, trees: &legacyProjectTrees{commits: map[string]domain.GitCommitTree{}},
		resources: &legacyProjectResources{test: test},
	}
	fixture.view = domain.RepositoryView{
		DefaultBranch: "main", Revision: domain.RepositoryRevision{Graph: 3, Evidence: 2},
		Refs: []domain.Ref{
			{RepoID: legacyProjectHash("tag is not the repository owner"), Kind: domain.RefTag, Name: "pr", Target: fixture.prCapture},
			{RepoID: repo, Kind: domain.RefTag, Name: "git", Target: fixture.gitCapture},
			{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: tip},
		},
		Snapshots: []domain.Snapshot{
			{RepoID: repo, ID: tip, DocHash: tip, Branch: "main", Parents: []domain.ContentHash{common}},
			{RepoID: repo, ID: fixture.prCapture, DocHash: fixture.prCapture, Parents: []domain.ContentHash{common}},
			{RepoID: repo, ID: fixture.gitCapture, DocHash: fixture.gitCapture, Parents: []domain.ContentHash{common}},
			{RepoID: repo, ID: common, DocHash: common},
		},
		History: []domain.HistoryEvent{
			{ID: fixture.position, RepoID: string(repo), BranchID: "main-id", Branch: "main", Kind: "position", Target: tip, GitAfter: legacyProjectOID(4)},
			{ID: fmt.Sprintf("%032x", 2), RepoID: string(repo), BranchID: "main-id", Branch: "main", Kind: "birth", Target: tip},
		},
	}
	fixture.evidence.prs = []evidencequery.VerifiedPRContribution{{
		Record: domain.PRContribution{
			ID: "pr-contribution", RepoID: repo, DestinationBranchID: "main-id", Capture: fixture.prCapture,
			PR: domain.PullRequestMerge{Number: 17, BaseBranch: "old-main", HeadBranch: "feature-pr", MergeSHA: legacyProjectOID(2)},
		},
		Valid: true, MergedAt: time.Unix(100, 0),
	}}
	fixture.evidence.git = []evidencequery.VerifiedGitContribution{{
		Record: domain.GitContribution{
			ID: "git-contribution", RepoID: repo, DestinationBranchID: "main-id", Capture: fixture.gitCapture, IntegrationSHA: legacyProjectOID(3),
		},
		Valid: true, SourceBranch: "feature-git", DestinationBranch: "old-main",
	}}
	for number := 1; number <= 4; number++ {
		commit := domain.GitCommitTree{Commit: legacyProjectOID(number), Parents: []string{}}
		if number > 1 {
			commit.Parents = []string{legacyProjectOID(number - 1)}
		}
		fixture.trees.commits[commit.Commit] = commit
	}
	fixture.projection = &LegacyProjection{
		scope: fixture.scope, resources: fixture.resources, metadata: fixture.metadata,
		contributions: fixture.evidence, generations: fixture.generation, trees: fixture.trees,
		graphs: &GraphCache{}, branches: &BranchCache{},
	}
	return fixture
}

func legacyProjectGraphKey(test *testing.T, fixture *legacyProjectFixture, position string) GraphProjectionKey {
	test.Helper()
	fingerprint := sha256.New()
	if err := json.NewEncoder(fingerprint).Encode(fixture.view); err != nil {
		test.Fatal(err)
	}
	key := GraphProjectionKey{
		Repo: fixture.repo, Revision: fixture.view.Revision, Source: fixture.generation.value,
		Origin: fixture.metadata.value.GitRemoteURL, Position: position,
	}
	copy(key.Inputs[:], fingerprint.Sum(nil))
	return key
}

func TestLegacyProjectionContributionAdapterIsZeroCopy(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	contributions := legacyProjectionContributions{prs: fixture.evidence.prs, git: fixture.evidence.git}
	if contributions.Len() != 2 {
		test.Fatal("verified contribution families were not concatenated")
	}
	prExpected := contextprojection.Contribution{
		ID: "pr-contribution", DestinationBranchID: "main-id", Capture: fixture.prCapture, IntegrationSHA: legacyProjectOID(2),
		Valid: true, SourceBranch: "feature-pr", DestinationBranch: "old-main", PRNumber: 17, MergedAt: time.Unix(100, 0),
	}
	gitExpected := contextprojection.Contribution{
		ID: "git-contribution", DestinationBranchID: "main-id", Capture: fixture.gitCapture, IntegrationSHA: legacyProjectOID(3),
		Valid: true, SourceBranch: "feature-git", DestinationBranch: "old-main",
	}
	if contributions.At(0) != prExpected || contributions.At(1) != gitExpected {
		test.Fatal("verified fields changed at the context policy boundary")
	}
	fixture.evidence.prs[0].Valid = false
	fixture.evidence.git[0].SourceBranch = "renamed-source"
	if contributions.At(0).Valid || contributions.At(1).SourceBranch != "renamed-source" {
		test.Fatal("adapter copied a normalized ledger instead of wrapping verified slices")
	}
}

func TestLegacyProjectionDirectOwnerProjectsIntegrationFacts(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	graph, err := fixture.projection.Project(context.Background(), fixture.view, "")
	if err != nil {
		test.Fatal(err)
	}
	inclusion := graph.BranchContexts["main"]
	if inclusion.CodeCommit != legacyProjectOID(4) || inclusion.BranchID != "main-id" || len(inclusion.Merges) != 2 {
		test.Fatalf("branch policy not projected: %+v", inclusion)
	}
	for _, merge := range inclusion.Merges {
		if merge.State != "included" || merge.Reason != "verified_git_order" || !merge.DeliveryAcknowledged {
			test.Fatalf("verified contribution lost inclusion: %+v", merge)
		}
	}
	if inclusion.Merges[0].Source != fixture.prCapture || inclusion.Merges[1].Source != fixture.gitCapture || !slices.Contains(inclusion.SnapshotIDs, fixture.prCapture) || !slices.Contains(inclusion.SnapshotIDs, fixture.gitCapture) {
		test.Fatal("Git order or included context roots were lost")
	}
	if len(graph.Operations.Merges) != 2 || len(graph.Integrations) != 1 {
		test.Fatalf("graph operations/integrations missing: %+v %+v", graph.Operations, graph.Integrations)
	}
	for _, operation := range graph.Operations.Merges {
		if operation.Branch != "main" || !operation.HistoricalOnly {
			test.Fatalf("current branch identity not applied: %+v", operation)
		}
	}
	integration := graph.Integrations[0]
	if !slices.Equal(integration.Parents["graph:merge:pr-contribution"], []string{string(fixture.prCapture)}) || !slices.Equal(integration.Parents["graph:merge:git-contribution"], []string{"graph:merge:pr-contribution", string(fixture.gitCapture)}) {
		test.Fatalf("verified integrations not chained: %+v", integration)
	}
	if fixture.metadata.repo != fixture.repo || fixture.evidence.repo != fixture.repo || fixture.trees.repo != fixture.repo || fixture.trees.origin != fixture.metadata.value.GitRemoteURL || fixture.trees.calls != 4 {
		test.Fatal("project did not bind one Git evidence owner to the first branch repository")
	}
	if fixture.evidence.prCalls != 1 || fixture.evidence.gitCalls != 1 || &fixture.evidence.history[0] != &fixture.view.History[0] {
		test.Fatal("verified evidence was reloaded or history was copied")
	}
}

func TestLegacyProjectionBranchHitRetainsCurrentDisplayAndOperations(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	first, err := fixture.projection.Project(context.Background(), fixture.view, "")
	if err != nil {
		test.Fatal(err)
	}
	fixture.trees.err = domain.ErrResourceOverloaded
	fixture.evidence.prs[0].MergedAt = time.Unix(900, 0)
	fixture.evidence.prs[0].Record.PR.HeadBranch = "current-source-label"
	pending := legacyProjectHash("current pending")
	fixture.view.Snapshots = append(fixture.view.Snapshots, domain.Snapshot{RepoID: fixture.repo, ID: pending, Parents: []domain.ContentHash{first.BranchContexts["main"].SnapshotID}, Message: "hook: current"})
	fixture.view.Pending = []domain.Pending{{RepoID: fixture.repo, SessionID: "session", Target: pending}}
	fixture.view.Revision.Pending++
	fixture.view.Snapshots[1].Parents = append(fixture.view.Snapshots[1].Parents, first.BranchContexts["main"].SnapshotID)
	next, err := fixture.projection.Project(context.Background(), fixture.view, "")
	if err != nil {
		test.Fatal(err)
	}
	if fixture.trees.calls != 4 || fixture.evidence.prCalls != 2 || fixture.evidence.gitCalls != 2 || !reflect.DeepEqual(first.BranchContexts, next.BranchContexts) {
		test.Fatal("branch hit rebuilt evidence or skipped current contribution queries")
	}
	if !slices.Contains(next.UncommittedIDs, pending) || next.Revision.Pending != fixture.view.Revision.Pending {
		test.Fatal("branch hit reused a stale pending display")
	}
	if next.Operations.Merges[0].From != "current-source-label" || !next.Operations.Merges[0].CreatedAt.Equal(time.Unix(900, 0)) {
		test.Fatal("contribution operations were not appended on a branch hit")
	}
	if first.Integrations[0].Head == next.Integrations[0].Head || next.Integrations[0].Head != "graph:merge:git-contribution" {
		test.Fatal("integration display did not use the current snapshot view")
	}
}

func TestReadProjectionGraphSourceGenerationRequiresOnlyExplicitPorts(test *testing.T) {
	repo := legacyProjectHash("standalone generation")
	for _, scenario := range []struct {
		name      string
		pinned    bool
		captured  bool
		noReader  bool
		change    func(*domain.RepositoryViewSourceCheckpoint)
		readErr   error
		wantErr   error
		wantReady bool
		wantReads int
	}{
		{name: "committed", pinned: true, wantReady: true, wantReads: 1},
		{name: "not pinned"},
		{name: "captured", pinned: true, captured: true},
		{name: "explicit nil generation", pinned: true, noReader: true},
		{name: "missing", pinned: true, readErr: fmt.Errorf("generation: %w", domain.ErrNotFound), wantReads: 1},
		{name: "unsupported", pinned: true, readErr: fmt.Errorf("generation: %w", domain.ErrCatalogUnsupported), wantReads: 1},
		{name: "reset", pinned: true, readErr: fmt.Errorf("generation: %w", domain.ErrCatalogResetRequired), wantReads: 1},
		{name: "storage failure", pinned: true, readErr: domain.ErrResourceOverloaded, wantErr: domain.ErrResourceOverloaded, wantReads: 1},
		{name: "foreign repo", pinned: true, change: func(generation *domain.RepositoryViewSourceCheckpoint) {
			generation.RepoID = legacyProjectHash("foreign")
		}, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "metadata scope", pinned: true, change: func(generation *domain.RepositoryViewSourceCheckpoint) {
			generation.Scope = domain.RepositoryViewSourceScope
		}, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "version", pinned: true, change: func(generation *domain.RepositoryViewSourceCheckpoint) { generation.Version++ }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "zero epoch", pinned: true, change: func(generation *domain.RepositoryViewSourceCheckpoint) {
			generation.Epoch = "00000000-0000-0000-0000-000000000000"
		}, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "invalid epoch", pinned: true, change: func(generation *domain.RepositoryViewSourceCheckpoint) { generation.Epoch = "invalid" }, wantErr: domain.ErrIntegrity, wantReads: 1},
		{name: "negative sequence", pinned: true, change: func(generation *domain.RepositoryViewSourceCheckpoint) { generation.Sequence = -1 }, wantErr: domain.ErrIntegrity, wantReads: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for _, useMethod := range []bool{false, true} {
				scope := &legacyProjectScope{pinned: scenario.pinned}
				reader := &legacyProjectGeneration{value: legacyProjectCheckpoint(repo), err: scenario.readErr}
				if scenario.change != nil {
					scenario.change(&reader.value)
				}
				var generations GraphGenerationReader = reader
				if scenario.noReader {
					generations = nil
				}
				read := func() (domain.RepositoryViewSourceCheckpoint, bool, error) {
					return ReadProjectionGraphSourceGeneration(context.Background(), repo, scope, generations, scenario.captured)
				}
				if useMethod {
					projection := &LegacyProjection{scope: scope, generations: generations, captured: scenario.captured}
					read = func() (domain.RepositoryViewSourceCheckpoint, bool, error) {
						return projection.SourceGeneration(context.Background(), repo)
					}
				}
				generation, ready, err := read()
				if !errors.Is(err, scenario.wantErr) || ready != scenario.wantReady || reader.calls != scenario.wantReads {
					test.Fatalf("method=%t: generation=%+v ready=%t err=%v reads=%d", useMethod, generation, ready, err, reader.calls)
				}
				if ready && generation != reader.value || !ready && generation != (domain.RepositoryViewSourceCheckpoint{}) {
					test.Fatal("unavailable or invalid generation escaped its fence")
				}
				if reader.calls != 0 && reader.repo != repo {
					test.Fatal("generation reader received the wrong repository")
				}
				if scenario.captured && scope.checks != 0 {
					test.Fatal("captured projection consulted the committed scope")
				}
			}
		})
	}
}

func TestProjectionGenerationHelpersPreserveCancellationAndClassifiers(test *testing.T) {
	repo := legacyProjectHash("generation helper fences")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ready, err := ReadProjectionGraphSourceGeneration(ctx, repo, nil, nil, true); ready || !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation did not precede optional dependency checks", ready, err)
	}
	var typedNil *legacyProjectScope
	for _, scope := range []ProjectionCacheScope{nil, typedNil} {
		if _, ready, err := ReadProjectionGraphSourceGeneration(context.Background(), repo, scope, nil, false); ready || !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("absent standalone scope did not fail closed", ready, err)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader := &legacyProjectGeneration{value: legacyProjectCheckpoint(repo), err: domain.ErrNotFound, cancel: cancel}
	if generation, ready, err := ReadProjectionGraphSourceGeneration(ctx, repo, &legacyProjectScope{pinned: true}, reader, false); generation != (domain.RepositoryViewSourceCheckpoint{}) || ready || !errors.Is(err, context.Canceled) {
		test.Fatal("read cancellation was hidden by the unavailable fallback", generation, ready, err)
	}
	for _, err := range []error{domain.ErrNotFound, domain.ErrCatalogUnsupported, domain.ErrCatalogResetRequired} {
		if !ProjectionGenerationUnavailable(fmt.Errorf("wrapped: %w", err)) {
			test.Fatal("wrapped unavailable generation rejected", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, domain.ErrIntegrity, domain.ErrResourceOverloaded} {
		if ProjectionGenerationUnavailable(err) {
			test.Fatal("fatal generation error became a cache miss", err)
		}
	}
	if !ValidProjectionGraphGeneration(repo, legacyProjectCheckpoint(repo)) || ValidProjectionGraphGeneration(legacyProjectHash("different"), legacyProjectCheckpoint(repo)) || ValidProjectionGraphGeneration(repo, domain.RepositoryViewSourceCheckpoint{}) {
		test.Fatal("pure committed-generation validator changed")
	}
}

func TestLegacyProjectionBranchCacheFencesEveryCommittedIdentity(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*legacyProjectFixture)
	}{
		{name: "sequence", change: func(fixture *legacyProjectFixture) { fixture.generation.value.Sequence++ }},
		{name: "epoch", change: func(fixture *legacyProjectFixture) {
			fixture.generation.value.Epoch = "22345678-abcd-4321-9876-123456789abc"
		}},
		{name: "graph", change: func(fixture *legacyProjectFixture) { fixture.view.Revision.Graph++ }},
		{name: "evidence", change: func(fixture *legacyProjectFixture) { fixture.view.Revision.Evidence++ }},
		{name: "origin", change: func(fixture *legacyProjectFixture) {
			fixture.metadata.value.GitRemoteURL = "https://example.invalid/rebound.git"
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); err != nil {
				test.Fatal(err)
			}
			scenario.change(fixture)
			fixture.trees.err = domain.ErrMemoryProjectionLimit
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
				test.Fatal("branch cache crossed a committed identity fence", err)
			}
		})
	}
}

func TestLegacyProjectionUncommittedSourcesNeverReuseBranchFacts(test *testing.T) {
	for _, scenario := range []string{"captured", "not pinned", "nil generation", "generation unavailable"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			switch scenario {
			case "captured":
				fixture.projection.captured = true
			case "not pinned":
				fixture.scope.pinned = false
			case "nil generation":
				fixture.projection.generations = nil
			case "generation unavailable":
				fixture.generation.err = domain.ErrCatalogResetRequired
			}
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); err != nil {
				test.Fatal(err)
			}
			fixture.trees.err = domain.ErrResourceOverloaded
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); !errors.Is(err, domain.ErrResourceOverloaded) {
				test.Fatal("uncommitted projection reused branch facts", err)
			}
			if scenario != "generation unavailable" && fixture.generation.calls != 0 {
				test.Fatal("optional generation reader was used")
			}
		})
	}
}

func TestLegacyProjectionExactPositionAndNoBranchBypassEvidence(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	projection := &LegacyProjection{}
	expected, err := domain.ProjectGraphState(fixture.view, fixture.view.DefaultBranch, fixture.position)
	if err != nil {
		test.Fatal(err)
	}
	expected.BranchContexts = map[string]domain.BranchContext{}
	expected.Integrations = []domain.GraphIntegration{}
	actual, err := projection.Project(context.Background(), fixture.view, fixture.position)
	if err != nil || !reflect.DeepEqual(actual, expected) {
		test.Fatal("exact position consulted enrichment ports or changed historical scope", err)
	}
	if _, err := projection.Project(context.Background(), fixture.view, "missing-position"); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("missing position did not preserve its domain error", err)
	}
	fixture.view.Refs = fixture.view.Refs[:2]
	actual, err = projection.Project(context.Background(), fixture.view, "")
	if err != nil || actual.BranchContexts == nil || actual.Integrations == nil || len(actual.BranchContexts) != 0 || len(actual.Integrations) != 0 {
		test.Fatal("branchless view required enrichment dependencies", err)
	}
}

func TestLegacyProjectionMetadataFallbackAndSharedGitEvidence(test *testing.T) {
	for _, scenario := range []string{"missing metadata", "missing trees", "missing branch identity"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			switch scenario {
			case "missing metadata":
				fixture.metadata.err = fmt.Errorf("legacy metadata: %w", domain.ErrNotFound)
			case "missing trees":
				fixture.projection.trees = nil
			case "missing branch identity":
				fixture.view.Refs[2].BranchID = ""
			}
			graph, err := fixture.projection.Project(context.Background(), fixture.view, "")
			if err != nil || graph.BranchContexts["main"].BranchID != "main-id" {
				test.Fatal("optional evidence or ref identity fallback failed", err)
			}
			if scenario == "missing metadata" && (graph.BranchContexts["main"].CodeCommit != "" || fixture.trees.calls != 0) {
				test.Fatal("missing metadata bound a Git origin")
			}
			if scenario == "missing trees" && graph.BranchContexts["main"].Merges[0].State != "review" {
				test.Fatal("absent Git reader fabricated inclusion")
			}
		})
	}
	fixture := newLegacyProjectFixture(test)
	fixture.projection.generations = nil
	mirror := fixture.view.Refs[2]
	mirror.Name = "mirror"
	fixture.view.Refs = append(fixture.view.Refs, mirror)
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); err != nil {
			test.Fatal(err)
		}
		if fixture.trees.calls != 4*attempt || fixture.evidence.prCalls != attempt || fixture.evidence.gitCalls != attempt {
			test.Fatal("Git evidence was not shared within a project and fresh between projects")
		}
	}
}

func TestLegacyProjectionErrorsCannotHideBehindBranchHits(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*legacyProjectFixture)
		want   error
	}{
		{name: "metadata mismatch", change: func(fixture *legacyProjectFixture) { fixture.metadata.value.ID = legacyProjectHash("foreign metadata") }, want: domain.ErrIntegrity},
		{name: "metadata error", change: func(fixture *legacyProjectFixture) { fixture.metadata.err = domain.ErrUnauthorized }, want: domain.ErrUnauthorized},
		{name: "PR error", change: func(fixture *legacyProjectFixture) { fixture.evidence.prErr = domain.ErrIntegrity }, want: domain.ErrIntegrity},
		{name: "Git error", change: func(fixture *legacyProjectFixture) { fixture.evidence.gitErr = domain.ErrResourceOverloaded }, want: domain.ErrResourceOverloaded},
		{name: "generation error", change: func(fixture *legacyProjectFixture) { fixture.generation.err = domain.ErrConflict }, want: domain.ErrConflict},
		{name: "foreign generation", change: func(fixture *legacyProjectFixture) {
			fixture.generation.value.RepoID = legacyProjectHash("foreign generation")
		}, want: domain.ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); err != nil {
				test.Fatal(err)
			}
			scenario.change(fixture)
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); !errors.Is(err, scenario.want) {
				test.Fatal("branch hit hid an explicit port error", err)
			}
		})
	}
	fixture := newLegacyProjectFixture(test)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.evidence.cancelOnPR = cancel
	if _, err := fixture.projection.Project(ctx, fixture.view, ""); !errors.Is(err, context.Canceled) || fixture.evidence.gitCalls != 0 {
		test.Fatal("canceled PR read continued to Git verification", err)
	}
}

func TestLegacyProjectionDirectOwnerRejectsForeignAndIncompleteInputs(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*legacyProjectFixture)
		want   error
	}{
		{name: "foreign snapshot", change: func(fixture *legacyProjectFixture) {
			fixture.view.Snapshots[0].RepoID = legacyProjectHash("foreign snapshot")
		}, want: domain.ErrIntegrity},
		{name: "foreign history", change: func(fixture *legacyProjectFixture) {
			fixture.view.History[0].RepoID = string(legacyProjectHash("foreign history"))
		}, want: domain.ErrIntegrity},
		{name: "duplicate snapshot", change: func(fixture *legacyProjectFixture) {
			fixture.view.Snapshots = append(fixture.view.Snapshots, fixture.view.Snapshots[0])
		}, want: domain.ErrIntegrity},
		{name: "missing branch target", change: func(fixture *legacyProjectFixture) {
			fixture.view.Refs[2].Target = legacyProjectHash("missing branch target")
		}, want: domain.ErrNotFound},
		{name: "tree reader failure", change: func(fixture *legacyProjectFixture) { fixture.trees.err = domain.ErrResourceOverloaded }, want: domain.ErrResourceOverloaded},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			scenario.change(fixture)
			if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); !errors.Is(err, scenario.want) {
				test.Fatal("projection accepted foreign or incomplete evidence", err)
			}
		})
	}
}

func TestLegacyProjectionActualGitEvidencePreservesStrictTraversalLimit(test *testing.T) {
	for _, strict := range []bool{false, true} {
		test.Run(fmt.Sprintf("strict=%t", strict), func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			fixture.projection.requireComplete = strict
			fixture.view.History[0].GitAfter = legacyProjectOID(1)
			fixture.trees.commits = make(map[string]domain.GitCommitTree, contextprojection.MaxProjectionSnapshots)
			for number := 1; number <= contextprojection.MaxProjectionSnapshots; number++ {
				fixture.trees.commits[legacyProjectOID(number)] = domain.GitCommitTree{Commit: legacyProjectOID(number), Parents: []string{legacyProjectOID(number + 1)}}
			}
			_, err := fixture.projection.Project(context.Background(), fixture.view, "")
			if strict && !errors.Is(err, domain.ErrMemoryProjectionLimit) || !strict && err != nil || fixture.trees.calls != contextprojection.MaxProjectionSnapshots {
				test.Fatal("actual Git evidence traversal limit changed", err, fixture.trees.calls)
			}
		})
	}
}

func TestLegacyProjectionCachedUsesSharedReentrantBuildAndWarmBypass(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	ctx := context.Background()
	first, err := fixture.projection.Cached(ctx, fixture.repo, fixture.view, "")
	if err != nil {
		test.Fatal(err)
	}
	if fixture.resources.acquires != 1 || fixture.resources.reentries != 1 || fixture.resources.releases != 1 || fixture.resources.active || fixture.evidence.bound.Value(legacyProjectBuildKey{}) != fixture.resources {
		test.Fatal("cached/admitted did not reuse the same shared build scope")
	}
	expected, err := json.Marshal(first)
	if err != nil {
		test.Fatal(err)
	}
	first.SnapshotIDs[0] = legacyProjectHash("caller changed result")
	inclusion := first.BranchContexts["main"]
	inclusion.SnapshotIDs[0] = legacyProjectHash("caller changed inclusion")
	fixture.resources.err = domain.ErrResourceOverloaded
	fixture.evidence.prErr = domain.ErrIntegrity
	fixture.trees.err = domain.ErrIntegrity
	second, err := fixture.projection.Cached(ctx, fixture.repo, fixture.view, "")
	if err != nil {
		test.Fatal("warm graph cache required a build or evidence query", err)
	}
	actual, err := json.Marshal(second)
	if err != nil || string(actual) != string(expected) || fixture.resources.attempts != 2 || fixture.evidence.prCalls != 1 || fixture.evidence.gitCalls != 1 {
		test.Fatal("graph cache did not own its decoded results", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fixture.projection.Cached(canceled, fixture.repo, fixture.view, ""); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled warm cache read returned data", err)
	}
	fixture.metadata.err = domain.ErrUnauthorized
	if _, err := fixture.projection.Cached(ctx, fixture.repo, fixture.view, ""); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("warm graph cache hid a metadata read error", err)
	}
}

func TestLegacyProjectionGraphCacheIdentityIncludesWholeViewAndSource(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		change   func(*legacyProjectFixture)
		position bool
	}{
		{name: "snapshot upload", change: func(fixture *legacyProjectFixture) {
			identifier := legacyProjectHash("same revision upload")
			fixture.view.Snapshots = append(fixture.view.Snapshots, domain.Snapshot{RepoID: fixture.repo, ID: identifier, DocHash: identifier})
		}},
		{name: "default branch", change: func(fixture *legacyProjectFixture) { fixture.view.DefaultBranch = "other-default" }},
		{name: "reflog", change: func(fixture *legacyProjectFixture) {
			fixture.view.Reflog = []domain.RefLogEntry{{Name: "main", Kind: domain.RefBranch, New: fixture.view.Refs[2].Target}}
		}},
		{name: "pending", change: func(fixture *legacyProjectFixture) { fixture.view.Revision.Pending++ }},
		{name: "source sequence", change: func(fixture *legacyProjectFixture) { fixture.generation.value.Sequence++ }},
		{name: "source epoch", change: func(fixture *legacyProjectFixture) {
			fixture.generation.value.Epoch = "22345678-abcd-4321-9876-123456789abc"
		}},
		{name: "origin", change: func(fixture *legacyProjectFixture) {
			fixture.metadata.value.GitRemoteURL = "https://example.invalid/new-origin.git"
		}},
		{name: "exact position", position: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); err != nil {
				test.Fatal(err)
			}
			if scenario.change != nil {
				scenario.change(fixture)
			}
			position := ""
			if scenario.position {
				position = fixture.position
			}
			if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, position); err != nil || fixture.resources.acquires != 2 || fixture.resources.releases != 2 {
				test.Fatal("graph cache crossed an input or source fence", err)
			}
		})
	}
}

func TestLegacyProjectionCachedFallbacksStillRequireSharedAdmission(test *testing.T) {
	for _, scenario := range []string{"not pinned", "zero revision", "nil generation", "captured", "generation unavailable"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newLegacyProjectFixture(test)
			switch scenario {
			case "not pinned":
				fixture.scope.pinned = false
			case "zero revision":
				fixture.view.Revision = domain.RepositoryRevision{}
			case "nil generation":
				fixture.projection.generations = nil
			case "captured":
				fixture.projection.captured = true
			case "generation unavailable":
				fixture.generation.err = domain.ErrNotFound
			}
			fixture.resources.err = domain.ErrResourceOverloaded
			if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); !errors.Is(err, domain.ErrResourceOverloaded) || fixture.resources.attempts != 1 || fixture.evidence.prCalls != 0 {
				test.Fatal("noncacheable graph bypassed build admission", err)
			}
		})
	}
	fixture := newLegacyProjectFixture(test)
	fixture.metadata.err = domain.ErrNotFound
	if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); err != nil {
		test.Fatal("cached path lost metadata-not-found compatibility", err)
	}
}

func TestLegacyProjectionCacheReservationFollowsSharedAdmissionAndReleases(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	key := legacyProjectGraphKey(test, fixture, "")
	_, finish, err := fixture.projection.graphs.Lookup(key)
	if err != nil || finish == nil {
		test.Fatal("failed to reserve graph build", err)
	}
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	fixture.resources.err = domain.ErrUnauthorized
	if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("cache reservation preceded shared build admission", err)
	}
	fixture.resources.err = nil
	if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); !errors.Is(err, domain.ErrResourceOverloaded) || fixture.resources.active || fixture.resources.releases != 1 {
		test.Fatal("busy cache miss did not release its build scope", err)
	}
	finish()
	finish = nil
	fixture.evidence.prErr = domain.ErrConflict
	if graph, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(graph, domain.GraphState{}) || fixture.resources.active {
		test.Fatal("failed build published partial graph state or leaked admission", err)
	}
	fixture.evidence.prErr = nil
	if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); err != nil || fixture.resources.active || fixture.resources.acquires != fixture.resources.releases {
		test.Fatal("failed build retained its cache reservation", err)
	}
}

func TestLegacyProjectionCacheRecheckAndDecodeErrorsDoNotProject(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	key := legacyProjectGraphKey(test, fixture, "")
	encoded, err := EncodeGraphProjection(context.Background(), domain.GraphState{Version: 1, PrimaryBranch: "concurrent-fill"})
	if err != nil {
		test.Fatal(err)
	}
	fixture.resources.onAcquire = func() { fixture.projection.graphs.Put(key, encoded) }
	graph, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, "")
	if err != nil || graph.PrimaryBranch != "concurrent-fill" || fixture.evidence.prCalls != 0 || fixture.resources.reentries != 0 || fixture.resources.acquires != fixture.resources.releases {
		test.Fatal("cache miss did not recheck after shared admission", err)
	}
	fixture.projection.graphs.Put(key, []byte("invalid compressed graph"))
	fixture.resources.err = domain.ErrResourceOverloaded
	if _, err := fixture.projection.Cached(context.Background(), fixture.repo, fixture.view, ""); !errors.Is(err, domain.ErrIntegrity) || fixture.resources.attempts != 1 {
		test.Fatal("corrupt warm cache was rebuilt or hidden by admission", err)
	}
}

func TestLegacyProjectionAdmittedReusesCallerBuildScope(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	bound, release, err := fixture.resources.AcquireBuild(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	if _, err := fixture.projection.Admitted(bound, fixture.view, ""); err != nil || fixture.resources.acquires != 1 || fixture.resources.reentries != 1 || !fixture.resources.active || fixture.metadata.bound.Value(legacyProjectBuildKey{}) != fixture.resources {
		test.Fatal("admitted graph introduced a separate gate or lost the caller scope", err)
	}
}

func TestLegacyProjectionImplementationHasNoServiceOrOutboundDependency(test *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "legacy_project.go", nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	for _, imported := range source.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			test.Fatal(err)
		}
		if path == "github.com/wnsdy95/cxthub/backend/internal/app" || strings.Contains(path, "/ports/outbound") {
			test.Fatal("projector imports a root service or outbound adapter", path)
		}
	}
	ast.Inspect(source, func(node ast.Node) bool {
		switch candidate := node.(type) {
		case *ast.TypeAssertExpr:
			test.Error("projector performs a runtime capability assertion")
		case *ast.Ident:
			if candidate.Name == "Service" {
				test.Error("projector depends on the root Service")
			}
		}
		return true
	})
}
