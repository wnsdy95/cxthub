package memoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type liveTestResourceKey struct{}
type liveTestSnapshotKey struct{}

type liveTestScope struct {
	test         *testing.T
	repo         domain.ContentHash
	resources    int
	snapshots    int
	active       int
	pinned       bool
	cacheable    bool
	exitError    error
	snapshotExit error
}

func (scope *liveTestScope) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.resources++
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo != scope.repo {
		return domain.ErrValidation
	}
	scope.active++
	defer func() { scope.active-- }()
	bound := inbound.WithRepositoryActor(context.WithValue(ctx, liveTestResourceKey{}, scope), "live-resource-actor")
	if err := read(bound); err != nil {
		return err
	}
	return scope.exitError
}

func (scope *liveTestScope) WithinMemorySnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.snapshots++
	actor, system := inbound.RepositoryActor(ctx)
	if repo != scope.repo || scope.active == 0 || ctx.Value(liveTestResourceKey{}) != scope || actor != "live-resource-actor" || system {
		scope.test.Fatal("snapshot lost the resource-bound authority")
	}
	current := inbound.WithRepositoryActor(context.WithValue(ctx, liveTestSnapshotKey{}, scope), "live-snapshot-actor")
	if err := read(current); err != nil {
		return err
	}
	return scope.snapshotExit
}

func (scope *liveTestScope) IsPinnedRead(ctx context.Context) bool {
	return scope.pinned && scope.active > 0 && ctx.Value(liveTestSnapshotKey{}) == scope
}

func (scope *liveTestScope) CacheableMemoryProof(ctx context.Context, repo domain.ContentHash) bool {
	return repo == scope.repo && scope.IsPinnedRead(ctx) && scope.cacheable
}

type liveTestSource struct {
	test            *testing.T
	scope           *liveTestScope
	scoped          bool
	repo            domain.ContentHash
	snapshot        domain.Snapshot
	digest          domain.MemoryDigest
	hash            domain.ContentHash
	origin          string
	revision        domain.RepositoryRevision
	generation      domain.RepositoryMemorySourceGeneration
	history         []domain.HistoryEvent
	reads           int
	repositoryReads int
	historyReads    int
	prLedgerReads   int
	gitLedgerReads  int
	graphReads      int
	generationReads int
	graphError      error
	generationError error
	snapshotError   error
	commits         map[string]domain.GitCommitTree
	nodes           map[string]domain.GitTreeNode
	deltas          map[string]domain.GitCommitDelta
}

func (source *liveTestSource) check(ctx context.Context, repo domain.ContentHash) error {
	source.reads++
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo != source.repo {
		return domain.ErrIntegrity
	}
	if source.scoped {
		actor, system := inbound.RepositoryActor(ctx)
		if source.scope.active == 0 || ctx.Value(liveTestSnapshotKey{}) != source.scope || actor != "live-snapshot-actor" || system {
			source.test.Fatal("reader escaped the current snapshot authority or response resources")
		}
	}
	return nil
}

func (source *liveTestSource) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	source.repositoryReads++
	return domain.Repo{ID: source.repo, GitRemoteURL: source.origin}, source.check(ctx, repo)
}

func (source *liveTestSource) GetSnapshot(ctx context.Context, repo, snapshot domain.ContentHash) (domain.Snapshot, error) {
	if err := source.check(ctx, repo); err != nil {
		return domain.Snapshot{}, err
	}
	if source.snapshotError != nil {
		return domain.Snapshot{}, source.snapshotError
	}
	if snapshot != source.snapshot.ID {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return source.snapshot, nil
}

func (source *liveTestSource) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	return []domain.Snapshot{source.snapshot}, source.check(ctx, repo)
}

func (source *liveTestSource) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, branch string) (domain.Ref, error) {
	return domain.Ref{RepoID: repo, Kind: kind, Name: branch, BranchID: "main-identity", Target: source.snapshot.ID}, source.check(ctx, repo)
}

func (source *liveTestSource) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := source.check(ctx, repo); err != nil {
		return domain.MemoryDigest{}, err
	}
	if hash != source.hash {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return source.digest, nil
}

func (source *liveTestSource) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	source.historyReads++
	return append([]domain.HistoryEvent(nil), source.history...), source.check(ctx, repo)
}

func (source *liveTestSource) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	return source.revision, source.check(ctx, repo)
}

func (source *liveTestSource) ReadRepositoryGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	source.graphReads++
	if err := source.check(ctx, repo); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, err
	}
	return source.generation.Graph, source.graphError
}

func (source *liveTestSource) ReadRepositoryMemorySourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryMemorySourceGeneration, error) {
	source.generationReads++
	if err := source.check(ctx, repo); err != nil {
		return domain.RepositoryMemorySourceGeneration{}, err
	}
	return source.generation, source.generationError
}

func (source *liveTestSource) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	source.prLedgerReads++
	return nil, source.check(ctx, repo)
}

func (source *liveTestSource) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	source.gitLedgerReads++
	return nil, source.check(ctx, repo)
}

func (source *liveTestSource) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	if err := source.check(ctx, repo); err != nil {
		return domain.GitCommitTree{}, err
	}
	if origin != source.origin {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	value, found := source.commits[identifier]
	if !found {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	return value, nil
}

func (source *liveTestSource) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitTreeNode, error) {
	if err := source.check(ctx, repo); err != nil {
		return domain.GitTreeNode{}, err
	}
	value, found := source.nodes[identifier]
	if !found {
		return domain.GitTreeNode{}, domain.ErrNotFound
	}
	return value, nil
}

func (source *liveTestSource) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, identifier, parent string) (domain.GitCommitDelta, error) {
	if err := source.check(ctx, repo); err != nil {
		return domain.GitCommitDelta{}, err
	}
	value, found := source.deltas[identifier]
	if !found {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	return value, nil
}

func liveFixture(test *testing.T) (*liveTestSource, LiveConfig, domain.EffectiveMemoryRequest) {
	test.Helper()
	repo, snapshot := domain.HashContent([]byte(test.Name())), domain.HashContent([]byte("live-snapshot"))
	scope := &liveTestScope{test: test, repo: repo, pinned: true, cacheable: true}
	source := &liveTestSource{test: test, scope: scope, scoped: true, repo: repo,
		origin: "https://example.invalid/live.git", revision: domain.RepositoryRevision{Graph: 4, Evidence: 5},
		snapshot: domain.Snapshot{ID: snapshot, RepoID: repo},
		digest:   domain.MemoryDigest{SnapshotID: snapshot, Summary: strings.Repeat("가", 2800), KeyFacts: []string{"historical fact"}},
		generation: domain.RepositoryMemorySourceGeneration{Version: 1, Epoch: "23456789-abcd-4321-9876-123456789abc", Sequence: 9,
			Graph: domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 8}},
		commits: map[string]domain.GitCommitTree{}, nodes: map[string]domain.GitTreeNode{}, deltas: map[string]domain.GitCommitDelta{},
	}
	source.rehash()
	evidence, err := evidencequery.New(evidencequery.Config{Profile: evidencequery.Compatibility, Metadata: source, PRLedger: source, GitLedger: source})
	if err != nil {
		test.Fatal(err)
	}
	config := LiveConfig{Mode: LiveCurrent, Scope: scope, Metadata: source, Projection: source, History: source, Revisions: source,
		Repository: source, Evidence: evidence, Trees: source, Deltas: source, GraphScope: scope, GraphGenerations: source,
		MemoryGenerations: source, ProofScope: scope, Delivery: &DeliveryCache{}}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: source.hash, CodeCommit: fmt.Sprintf("%040x", 1)}, Limit: 1}
	return source, config, request
}

func (source *liveTestSource) rehash() {
	source.test.Helper()
	var err error
	source.hash, err = domain.MemoryDigestHash(source.digest)
	if err != nil {
		source.test.Fatal(err)
	}
	source.snapshot.MemoryHash = source.hash
}

func newLiveTestQueries(test *testing.T, config LiveConfig) *LiveQueries {
	test.Helper()
	queries, err := NewLive(config)
	if err != nil {
		test.Fatal(err)
	}
	return queries
}

func TestLiveScopeAuthorityAndExitErrors(test *testing.T) {
	for _, operation := range []string{"effective", "branch", "positions"} {
		test.Run(operation, func(test *testing.T) {
			source, config, request := liveFixture(test)
			queries := newLiveTestQueries(test, config)
			failure := errors.New("snapshot exit failed")
			source.scope.snapshotExit = failure
			switch operation {
			case "effective":
				page, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
				if !errors.Is(err, failure) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) {
					test.Fatalf("effective leaked a failed snapshot: %+v %v", page, err)
				}
			case "branch":
				projection, err := queries.QueryBranchMemory(context.Background(), source.repo, "main", source.snapshot.ID, "")
				if !errors.Is(err, failure) || !reflect.DeepEqual(projection, domain.MemoryProjection{}) {
					test.Fatalf("branch leaked a failed snapshot: %+v %v", projection, err)
				}
			case "positions":
				positions, err := queries.QueryMemoryPositions(context.Background(), source.repo, source.snapshot.ID, "")
				if !errors.Is(err, failure) || !reflect.DeepEqual(positions, domain.MemoryPositions{}) {
					test.Fatalf("positions leaked a failed snapshot: %+v %v", positions, err)
				}
			}
			if source.scope.resources != 1 || source.scope.snapshots != 1 || source.scope.active != 0 {
				test.Fatalf("unbalanced read resources: %+v", source.scope)
			}
		})
	}
}

func TestLiveFrozenPrimitivesCannotBypassPublicScope(test *testing.T) {
	source, config, request := liveFixture(test)
	live := newLiveTestQueries(test, config)
	if _, err := live.Project(context.Background(), source.repo, request); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("current projector exposed an unscoped read", err)
	}
	if _, err := live.SelectMemory(context.Background(), source.repo, request.Selection); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("current selector exposed an unscoped read", err)
	}
	config.Mode, config.Scope, config.GraphScope, config.ProofScope = LiveFrozen, nil, nil, nil
	config.FrozenGeneration, config.CapturedEvidence = &source.generation, true
	source.scoped = false
	frozen := newLiveTestQueries(test, config)
	source.generation.Sequence++
	if frozen.frozenGeneration.Sequence == source.generation.Sequence {
		test.Fatal("frozen generation retained a mutable caller pointer")
	}
	before := source.reads
	if _, err := frozen.QueryEffectiveMemory(context.Background(), source.repo, request); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("scope-free public effective read", err)
	}
	if _, err := frozen.QueryBranchMemory(context.Background(), source.repo, "main", source.snapshot.ID, ""); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("scope-free public branch read", err)
	}
	if _, err := frozen.QueryMemoryPositions(context.Background(), source.repo, source.snapshot.ID, ""); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("scope-free public positions read", err)
	}
	if source.reads != before {
		test.Fatal("rejected public read touched dependencies")
	}
	page, err := frozen.Project(context.Background(), source.repo, request)
	if err != nil || page.StateHash == "" || page.DeliveryStateHash == "" || config.Delivery.Len() != 0 {
		test.Fatalf("frozen projection/cache: %+v %v", page, err)
	}
	if _, err := frozen.SelectMemory(context.Background(), domain.HashContent([]byte("foreign")), request.Selection); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("frozen selector accepted another repository", err)
	}
}

func TestLiveStateDeliveryAndCursorContracts(test *testing.T) {
	source, config, request := liveFixture(test)
	queries := newLiveTestQueries(test, config)
	first, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
	if err != nil || first.Total != 3 || len(first.Items) != 1 || first.NextCursor == "" {
		test.Fatalf("first page: %+v %v", first, err)
	}
	basis := struct {
		Version          int
		Repo             domain.ContentHash
		Origin           string
		Selection        domain.EffectiveMemorySelection
		Lineage, History domain.ContentHash
		Graph, Evidence  uint64
		Content          string                                   `json:"Content,omitempty"`
		Source           *domain.RepositoryViewSourceCheckpoint   `json:"source,omitempty"`
		MemorySource     *domain.RepositoryMemorySourceGeneration `json:"memory_source,omitempty"`
	}{3, source.repo, source.origin, request.Selection, source.hash, domain.HashContent([]byte("null")), source.revision.Graph, source.revision.Evidence, "", &source.generation.Graph, &source.generation}
	raw, err := json.Marshal(basis)
	if err != nil || first.StateHash != domain.HashContent(raw) || first.LineageHash != source.hash {
		test.Fatalf("original state basis changed: %+v %v", first, err)
	}
	items, err := EffectiveItems(source.digest, "")
	if err != nil {
		test.Fatal(err)
	}
	proof, err := domain.EffectiveMemoryDeliveryStateHash(context.Background(), source.repo, source.origin, first, items)
	if err != nil || proof != first.DeliveryStateHash {
		test.Fatal("proof did not cover all items", proof, first.DeliveryStateHash, err)
	}
	request.Cursor = first.NextCursor
	second, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
	if err != nil || second.Items[0].ID != items[1].ID || second.StateHash != first.StateHash || second.DeliveryStateHash != proof {
		test.Fatalf("continuation changed selection: %+v %v", second, err)
	}
	source.revision.Graph++
	if _, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("revision did not stale the cursor", err)
	}
	request.Cursor = ""
	drifted, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
	if err != nil || drifted.StateHash == first.StateHash || drifted.DeliveryStateHash != proof {
		test.Fatalf("revision contaminated delivery semantics: %+v %v", drifted, err)
	}
	for _, item := range items {
		if !utf8.ValidString(item.Text) || item.TextHash == "" || item.State != "review" {
			test.Fatalf("legacy text contract: %+v", item)
		}
	}
}

func TestLiveConstructorAndCancellation(test *testing.T) {
	_, config, _ := liveFixture(test)
	for _, mutate := range []func(*LiveConfig){
		func(config *LiveConfig) { config.Mode = 0 },
		func(config *LiveConfig) { config.Metadata = nil },
		func(config *LiveConfig) { config.Scope = nil },
		func(config *LiveConfig) { config.GraphScope = nil },
		func(config *LiveConfig) { config.Delivery = nil },
		func(config *LiveConfig) { config.Trees = (*liveTestSource)(nil) },
		func(config *LiveConfig) { config.Mode = LiveFrozen },
	} {
		broken := config
		mutate(&broken)
		if _, err := NewLive(broken); err == nil {
			test.Fatal("invalid dependency profile accepted")
		}
	}
	source, config, request := liveFixture(test)
	queries := newLiveTestQueries(test, config)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := queries.QueryEffectiveMemory(ctx, source.repo, request); !errors.Is(err, context.Canceled) || source.reads != 0 {
		test.Fatal("canceled request touched source", err, source.reads)
	}
	var missing *LiveQueries
	if err := missing.WithMemoryReadResources(context.Background(), source.repo, func(context.Context) error { return nil }); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("nil query scope did not fail closed", err)
	}
}

func liveCodeFixture(test *testing.T, source *liveTestSource, request *domain.EffectiveMemoryRequest) {
	test.Helper()
	commit, tree := fmt.Sprintf("%040x", 2), fmt.Sprintf("%040x", 100)
	entry := domain.GitEntry{OID: fmt.Sprintf("%040x", 101), Mode: "100644"}
	source.commits[commit] = domain.GitCommitTree{Commit: commit, Tree: tree, Parents: []string{}}
	source.commits[request.Selection.CodeCommit] = domain.GitCommitTree{Commit: request.Selection.CodeCommit, Tree: tree, Parents: []string{}}
	source.nodes[tree] = domain.GitTreeNode{OID: tree, Entries: map[string]domain.GitEntry{"main.go": entry}}
	source.deltas[commit] = domain.GitCommitDelta{Commit: commit, Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{{Path: "main.go", After: entry}}}
	source.digest = domain.MemoryDigest{SnapshotID: source.snapshot.ID, ClaimsVersion: 1,
		Fragments: []domain.MemoryFragment{{SourceSnapshot: source.snapshot.ID, Claims: []domain.MemoryClaim{{Kind: "code", Text: "feature", Code: &domain.MemoryCodeScope{Commit: commit, Paths: []string{"main.go"}}}}}}}
	source.rehash()
	request.Selection.MemoryHash = source.hash
	observation := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(source.repo), Kind: "position", Branch: "main", BranchID: "main-identity", Target: source.snapshot.ID, GitAfter: commit, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	publication := observation
	publication.ID, publication.Kind, publication.Source = strings.Repeat("2", 32), "publish", source.snapshot.ID
	source.history = []domain.HistoryEvent{publication, observation}
}
