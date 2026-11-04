package evidencequery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var gitEvidenceTestRepo = domain.HashContent([]byte("git evidence owner repository"))

const gitEvidenceTestOrigin = "https://example.invalid/owner.git"

type gitEvidenceTestCall struct {
	repo   domain.ContentHash
	origin string
	id     string
	parent string
}

type gitEvidenceTestTrees struct {
	commits      map[string]domain.GitCommitTree
	nodes        map[string]domain.GitTreeNode
	commitErrors map[string]error
	nodeErrors   map[string]error
	commitCalls  []gitEvidenceTestCall
	nodeCalls    []gitEvidenceTestCall
	cancel       context.CancelFunc
	cancelAt     int
}

func (reader *gitEvidenceTestTrees) read(ctx context.Context) error {
	if reader.cancel != nil && (reader.cancelAt == 0 || len(reader.commitCalls)+len(reader.nodeCalls) == reader.cancelAt) {
		reader.cancel()
	}
	return ctx.Err()
}

func (reader *gitEvidenceTestTrees) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitCommitTree, error) {
	reader.commitCalls = append(reader.commitCalls, gitEvidenceTestCall{repo: repo, origin: origin, id: id})
	if err := reader.read(ctx); err != nil {
		return domain.GitCommitTree{}, err
	}
	value, found := reader.commits[id]
	if err := reader.commitErrors[id]; err != nil {
		return value, err
	}
	if !found {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	return value, nil
}

func (reader *gitEvidenceTestTrees) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitTreeNode, error) {
	reader.nodeCalls = append(reader.nodeCalls, gitEvidenceTestCall{repo: repo, origin: origin, id: id})
	if err := reader.read(ctx); err != nil {
		return domain.GitTreeNode{}, err
	}
	value, found := reader.nodes[id]
	if err := reader.nodeErrors[id]; err != nil {
		return value, err
	}
	if !found {
		return domain.GitTreeNode{}, domain.ErrNotFound
	}
	return value, nil
}

type gitEvidenceTestDeltas struct {
	values map[[2]string]domain.GitCommitDelta
	errors map[[2]string]error
	calls  []gitEvidenceTestCall
	cancel context.CancelFunc
}

func (reader *gitEvidenceTestDeltas) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, id, parent string) (domain.GitCommitDelta, error) {
	reader.calls = append(reader.calls, gitEvidenceTestCall{repo: repo, origin: origin, id: id, parent: parent})
	if reader.cancel != nil {
		reader.cancel()
	}
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	key := [2]string{id, parent}
	value, found := reader.values[key]
	if err := reader.errors[key]; err != nil {
		return value, err
	}
	if !found {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	return value, nil
}

type gitEvidenceTestReaders struct {
	*gitEvidenceTestTrees
	*gitEvidenceTestDeltas
}

var _ contextprojection.CodeEvidence = (*GitEvidence)(nil)
var _ GitTreeReader = (*gitEvidenceTestTrees)(nil)
var _ GitDeltaReader = (*gitEvidenceTestDeltas)(nil)

func gitEvidenceTestOwner(test testing.TB, config GitEvidenceConfig) *GitEvidence {
	test.Helper()
	evidence, err := NewGitEvidence(config)
	if err != nil || evidence == nil {
		test.Fatal("construct Git evidence owner", evidence, err)
	}
	return evidence
}

func gitEvidenceTestRead(ctx context.Context, evidence *GitEvidence, repo domain.ContentHash, origin, kind, id string) (any, error) {
	switch kind {
	case "commit":
		value, err := evidence.GetGitCommitTree(ctx, repo, origin, id)
		return value, err
	case "node":
		value, err := evidence.GetGitTreeNode(ctx, repo, origin, id)
		return value, err
	case "delta":
		value, err := evidence.GetGitDelta(ctx, repo, origin, id, "parent")
		return value, err
	default:
		panic("unknown Git evidence read kind")
	}
}

func gitEvidenceTestChain(count int) map[string]domain.GitCommitTree {
	commits := make(map[string]domain.GitCommitTree, count)
	for position := 0; position < count; position++ {
		id := fmt.Sprintf("commit-%d", position)
		parents := []string{}
		if position+1 < count {
			parents = append(parents, fmt.Sprintf("commit-%d", position+1))
		}
		commits[id] = domain.GitCommitTree{Commit: id, Parents: parents}
	}
	return commits
}

func TestGitEvidenceConstructorIsLazyAndPreservesScope(test *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, scenario := range []struct {
			name   string
			repo   domain.ContentHash
			origin string
		}{
			{"bound origin", gitEvidenceTestRepo, gitEvidenceTestOrigin},
			{"historical empty origin", gitEvidenceTestRepo, ""},
			{"opaque internal repository", domain.ContentHash("hh"), ""},
		} {
			test.Run(fmt.Sprintf("%s/strict=%t", scenario.name, strict), func(test *testing.T) {
				trees, deltas := &gitEvidenceTestTrees{}, &gitEvidenceTestDeltas{}
				evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: scenario.repo, Origin: scenario.origin, Trees: trees, Deltas: deltas, RequireComplete: strict})
				if evidence.Origin() != scenario.origin || evidence.ReadCount() != 0 || evidence.Limited() || evidence.TraversalLimited() || len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != 0 {
					test.Fatal("constructor performed I/O or changed initial scope/state")
				}
				if value, err, found := evidence.CachedCommit("missing"); found || err != nil || !reflect.DeepEqual(value, domain.GitCommitTree{}) || evidence.ReadCount() != 0 {
					test.Fatal("cache-only inspection performed a read", value, err, found)
				}
				if _, err := evidence.GetGitCommitTree(context.Background(), scenario.repo, scenario.origin, "missing"); !errors.Is(err, domain.ErrNotFound) || evidence.ReadCount() != 1 || len(trees.commitCalls) != 1 {
					test.Fatal("constructor did not bind the declared reader and exact scope", err)
				}
				if trees.commitCalls[0] != (gitEvidenceTestCall{repo: scenario.repo, origin: scenario.origin, id: "missing"}) {
					test.Fatal("reader received a substituted scope", trees.commitCalls)
				}
			})
		}
	}
}

func TestGitEvidenceConstructorRejectsDeclaredTypedNilReaders(test *testing.T) {
	var missingTrees *gitEvidenceTestTrees
	var missingDeltas *gitEvidenceTestDeltas
	for _, strict := range []bool{false, true} {
		for _, reader := range []string{"trees", "deltas", "both"} {
			test.Run(fmt.Sprintf("%s/strict=%t", reader, strict), func(test *testing.T) {
				trees, deltas := &gitEvidenceTestTrees{}, &gitEvidenceTestDeltas{}
				config := GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees, Deltas: deltas, RequireComplete: strict}
				if reader != "deltas" {
					config.Trees = missingTrees
				}
				if reader != "trees" {
					config.Deltas = missingDeltas
				}
				if evidence, err := NewGitEvidence(config); evidence != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("declared typed-nil reader accepted", evidence, err)
				}
				if len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != 0 {
					test.Fatal("invalid construction accessed a reader")
				}
			})
		}
	}
}

func TestGitEvidenceCachesValuesMissingAndErrorsAfterReaderCalls(test *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{"positive", nil},
		{"missing", domain.ErrNotFound},
		{"wrapped missing", fmt.Errorf("immutable evidence: %w", domain.ErrNotFound)},
		{"integrity", domain.ErrIntegrity},
		{"transient", errors.New("temporary reader failure")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			commit := domain.GitCommitTree{Commit: "shared", Tree: "tree", Parents: []string{"parent"}}
			node := domain.GitTreeNode{OID: "shared", Entries: map[string]domain.GitEntry{"file": {OID: "blob", Mode: "100644"}}}
			delta := domain.GitCommitDelta{Commit: "shared", Parent: "parent", Complete: true}
			trees := &gitEvidenceTestTrees{
				commits: map[string]domain.GitCommitTree{"shared": commit}, nodes: map[string]domain.GitTreeNode{"shared": node},
				commitErrors: map[string]error{"shared": scenario.err}, nodeErrors: map[string]error{"shared": scenario.err},
			}
			deltas := &gitEvidenceTestDeltas{values: map[[2]string]domain.GitCommitDelta{{"shared", "parent"}: delta}, errors: map[[2]string]error{{"shared", "parent"}: scenario.err}}
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Origin: gitEvidenceTestOrigin, Trees: trees, Deltas: deltas})
			for repeat := 0; repeat < 2; repeat++ {
				for _, operation := range []struct {
					kind string
					want any
				}{{"commit", commit}, {"node", node}, {"delta", delta}} {
					value, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, gitEvidenceTestOrigin, operation.kind, "shared")
					if !reflect.DeepEqual(value, operation.want) || err != scenario.err {
						test.Fatal("cache changed the reader value/error", operation.kind, repeat, value, err)
					}
				}
				trees.commits["shared"] = domain.GitCommitTree{Commit: "replacement"}
				trees.nodes["shared"] = domain.GitTreeNode{OID: "replacement"}
				deltas.values[[2]string{"shared", "parent"}] = domain.GitCommitDelta{Commit: "replacement"}
				trees.commitErrors["shared"], trees.nodeErrors["shared"] = nil, nil
				deltas.errors[[2]string{"shared", "parent"}] = nil
			}
			if evidence.ReadCount() != 3 || len(trees.commitCalls) != 1 || len(trees.nodeCalls) != 1 || len(deltas.calls) != 1 {
				test.Fatal("cache namespaces collided or repeated immutable reads", evidence.ReadCount(), trees.commitCalls, trees.nodeCalls, deltas.calls)
			}
			if value, err, found := evidence.CachedCommit("shared"); !found || !reflect.DeepEqual(value, commit) || err != scenario.err || evidence.ReadCount() != 3 {
				test.Fatal("cache-only accessor lost the cached commit error/value", value, err, found)
			}
		})
	}
}

func TestGitEvidenceChecksContextAndScopeEvenOnCacheHits(test *testing.T) {
	trees, deltas := &gitEvidenceTestTrees{}, &gitEvidenceTestDeltas{}
	evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Origin: gitEvidenceTestOrigin, Trees: trees, Deltas: deltas})
	kinds := []string{"commit", "node", "delta"}
	for _, kind := range kinds {
		if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, gitEvidenceTestOrigin, kind, "cached"); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("prime missing cache", kind, err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, scenario := range []struct {
		name   string
		ctx    context.Context
		repo   domain.ContentHash
		origin string
		err    error
	}{
		{"repository fence", context.Background(), domain.HashContent([]byte("other repository")), gitEvidenceTestOrigin, domain.ErrIntegrity},
		{"origin fence", context.Background(), gitEvidenceTestRepo, "other origin", domain.ErrIntegrity},
		{"cancellation", canceled, gitEvidenceTestRepo, gitEvidenceTestOrigin, context.Canceled},
		{"cancellation before scope", canceled, "other repository", "other origin", context.Canceled},
	} {
		for _, kind := range kinds {
			for _, id := range []string{"cached", "uncached"} {
				if _, err := gitEvidenceTestRead(scenario.ctx, evidence, scenario.repo, scenario.origin, kind, id); !errors.Is(err, scenario.err) {
					test.Fatal("scope/context check bypassed", scenario.name, kind, id, err)
				}
			}
		}
	}
	if evidence.ReadCount() != 3 || len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != 3 || evidence.Limited() || evidence.TraversalLimited() {
		test.Fatal("rejected read consumed work or changed limit flags", evidence.ReadCount())
	}
	if _, err, found := evidence.CachedCommit("uncached"); found || err != nil {
		test.Fatal("rejected read entered the commit cache", err, found)
	}
	for _, kind := range kinds {
		if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, gitEvidenceTestOrigin, kind, "uncached"); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("rejected read poisoned a later authorized read", kind, err)
		}
	}
	if evidence.ReadCount() != 6 || len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != 6 {
		test.Fatal("authorized misses were cached before a real reader call", evidence.ReadCount())
	}
}

func TestGitEvidenceDeltaCacheIncludesCommitAndComparisonParent(test *testing.T) {
	keys := [][2]string{{"merge", "left"}, {"merge", "right"}, {"merge", ""}, {"other", "left"}}
	deltas := &gitEvidenceTestDeltas{values: map[[2]string]domain.GitCommitDelta{}}
	for _, key := range keys {
		deltas.values[key] = domain.GitCommitDelta{Commit: key[0], Parent: key[1], Complete: true}
	}
	evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Origin: gitEvidenceTestOrigin, Deltas: deltas})
	for repeat := 0; repeat < 2; repeat++ {
		for _, key := range keys {
			value, err := evidence.GetGitDelta(context.Background(), gitEvidenceTestRepo, gitEvidenceTestOrigin, key[0], key[1])
			if err != nil || !reflect.DeepEqual(value, deltas.values[key]) {
				test.Fatal("delta reused another commit/parent comparison", key, value, err)
			}
		}
	}
	if evidence.ReadCount() != len(keys) || len(deltas.calls) != len(keys) {
		test.Fatal("delta comparison cache missed", evidence.ReadCount(), deltas.calls)
	}
	for position, key := range keys {
		if deltas.calls[position] != (gitEvidenceTestCall{repo: gitEvidenceTestRepo, origin: gitEvidenceTestOrigin, id: key[0], parent: key[1]}) {
			test.Fatal("comparison parent or scope was changed", deltas.calls)
		}
	}
}

func TestGitEvidenceReadBudgetIsSharedAndCacheHitsSurviveExhaustion(test *testing.T) {
	for _, strict := range []bool{false, true} {
		test.Run(fmt.Sprintf("strict=%t", strict), func(test *testing.T) {
			trees, deltas := &gitEvidenceTestTrees{}, &gitEvidenceTestDeltas{}
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Origin: gitEvidenceTestOrigin, Trees: trees, Deltas: deltas, RequireComplete: strict})
			kinds := []string{"commit", "node", "delta"}
			for position := 0; position < contextprojection.MaxCodeEvidenceReads; position++ {
				kind := kinds[position%len(kinds)]
				if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, gitEvidenceTestOrigin, kind, fmt.Sprintf("object-%d", position)); !errors.Is(err, domain.ErrNotFound) || evidence.ReadCount() != position+1 {
					test.Fatal("shared budget was not charged exactly once per reader call", kind, position, evidence.ReadCount(), err)
				}
			}
			if evidence.Limited() || evidence.TraversalLimited() || len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != contextprojection.MaxCodeEvidenceReads {
				test.Fatal("exact read boundary was prematurely limited")
			}
			wantErr := domain.ErrNotFound
			if strict {
				wantErr = domain.ErrMemoryProjectionLimit
			}
			for _, kind := range kinds {
				if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, gitEvidenceTestOrigin, kind, "blocked"); !errors.Is(err, wantErr) {
					test.Fatal("reader escaped the shared budget policy", kind, err)
				}
			}
			if !evidence.Limited() || evidence.TraversalLimited() || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads || len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != contextprojection.MaxCodeEvidenceReads {
				test.Fatal("exhausted reads reached a reader or changed the counter")
			}
			if _, err, found := evidence.CachedCommit("blocked"); found || err != nil {
				test.Fatal("budget rejection was cached as immutable evidence", err, found)
			}
			for position, kind := range kinds {
				if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, gitEvidenceTestOrigin, kind, fmt.Sprintf("object-%d", position)); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("exhaustion overrode an existing cache hit", kind, err)
				}
			}
			if _, err, found := evidence.CachedCommit("object-0"); !found || !errors.Is(err, domain.ErrNotFound) || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads {
				test.Fatal("real missing read was not retained", err, found)
			}
		})
	}
}

func TestGitEvidenceConsumeReadIsMonotonicScopedAndContextAware(test *testing.T) {
	for _, strict := range []bool{false, true} {
		test.Run(fmt.Sprintf("strict=%t", strict), func(test *testing.T) {
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Origin: gitEvidenceTestOrigin, RequireComplete: strict})
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			for position := 0; position <= contextprojection.MaxCodeEvidenceReads; position++ {
				if position == 0 || position == contextprojection.MaxCodeEvidenceReads {
					if err := evidence.ConsumeRead(canceled, "wrong repository", "wrong origin"); !errors.Is(err, context.Canceled) {
						test.Fatal("admission did not prioritize cancellation", err)
					}
					if err := evidence.ConsumeRead(context.Background(), "wrong repository", gitEvidenceTestOrigin); !errors.Is(err, domain.ErrIntegrity) {
						test.Fatal("admission bypassed repository fence", err)
					}
					if err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, "wrong origin"); !errors.Is(err, domain.ErrIntegrity) {
						test.Fatal("admission bypassed origin fence", err)
					}
					if evidence.ReadCount() != position || evidence.Limited() {
						test.Fatal("rejected admission changed counter/limit state", position, evidence.ReadCount())
					}
				}
				err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, gitEvidenceTestOrigin)
				if position < contextprojection.MaxCodeEvidenceReads {
					if err != nil || evidence.ReadCount() != position+1 {
						test.Fatal("admission counter was not monotonic", position, evidence.ReadCount(), err)
					}
				} else {
					wantErr := domain.ErrNotFound
					if strict {
						wantErr = domain.ErrMemoryProjectionLimit
					}
					if !errors.Is(err, wantErr) || evidence.ReadCount() != position || !evidence.Limited() || evidence.TraversalLimited() {
						test.Fatal("admission exhaustion changed the budget", evidence.ReadCount(), err)
					}
				}
			}
			if _, err, found := evidence.CachedCommit("unused"); found || err != nil {
				test.Fatal("admission synthesized a cached read", err, found)
			}
		})
	}
}

func TestGitEvidenceMissingPortsRemainOptionalAndUncached(test *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, scenario := range []struct {
			name   string
			trees  bool
			deltas bool
			reads  int
		}{
			{"none", false, false, 6},
			{"trees only", true, false, 4},
			{"deltas only", false, true, 5},
			{"both", true, true, 3},
		} {
			test.Run(fmt.Sprintf("%s/strict=%t", scenario.name, strict), func(test *testing.T) {
				trees, deltas := &gitEvidenceTestTrees{}, &gitEvidenceTestDeltas{}
				config := GitEvidenceConfig{RepoID: gitEvidenceTestRepo, RequireComplete: strict}
				if scenario.trees {
					config.Trees = trees
				}
				if scenario.deltas {
					config.Deltas = deltas
				}
				evidence := gitEvidenceTestOwner(test, config)
				for repeat := 0; repeat < 2; repeat++ {
					for _, kind := range []string{"commit", "node", "delta"} {
						if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, "", kind, "missing"); !errors.Is(err, domain.ErrNotFound) {
							test.Fatal("strictness activated an absent reader", kind, err)
						}
					}
				}
				if evidence.ReadCount() != scenario.reads || evidence.Limited() || evidence.TraversalLimited() {
					test.Fatal("absent reader was not charged on every attempt", evidence.ReadCount(), scenario.reads)
				}
				if len(trees.commitCalls) != 0 && !scenario.trees || len(trees.nodeCalls) != 0 && !scenario.trees || len(deltas.calls) != 0 && !scenario.deltas {
					test.Fatal("undeclared reader was used")
				}
				if _, err, found := evidence.CachedCommit("missing"); found != scenario.trees || scenario.trees && !errors.Is(err, domain.ErrNotFound) || !scenario.trees && err != nil {
					test.Fatal("cache must distinguish absent ports from actual missing reads", err, found)
				}
				if scenario.name == "none" {
					for position := scenario.reads; position < contextprojection.MaxCodeEvidenceReads; position++ {
						if _, err := evidence.GetGitCommitTree(context.Background(), gitEvidenceTestRepo, "", "missing"); !errors.Is(err, domain.ErrNotFound) {
							test.Fatal("missing port changed behavior before the bound", err)
						}
						if evidence.ReadCount() != position+1 {
							test.Fatal("absent reader admission was cached or not charged", position, evidence.ReadCount())
						}
					}
					wantErr := domain.ErrNotFound
					if strict {
						wantErr = domain.ErrMemoryProjectionLimit
					}
					if _, err := evidence.GetGitCommitTree(context.Background(), gitEvidenceTestRepo, "", "missing"); !errors.Is(err, wantErr) || !evidence.Limited() || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads {
						test.Fatal("missing ports bypassed exhaustion semantics", err)
					}
				}
			})
		}
	}
}

func TestGitEvidenceDoesNotDiscoverUndeclaredCapabilities(test *testing.T) {
	for _, strict := range []bool{false, true} {
		trees, deltas := &gitEvidenceTestTrees{}, &gitEvidenceTestDeltas{}
		readers := &gitEvidenceTestReaders{gitEvidenceTestTrees: trees, gitEvidenceTestDeltas: deltas}
		treesOnly := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: readers, RequireComplete: strict})
		if _, err := treesOnly.GetGitDelta(context.Background(), gitEvidenceTestRepo, "", "commit", "parent"); !errors.Is(err, domain.ErrNotFound) || treesOnly.ReadCount() != 1 || len(deltas.calls) != 0 {
			test.Fatal("tree reader implicitly activated its undeclared delta capability", err)
		}
		deltasOnly := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Deltas: readers, RequireComplete: strict})
		for _, kind := range []string{"commit", "node"} {
			if _, err := gitEvidenceTestRead(context.Background(), deltasOnly, gitEvidenceTestRepo, "", kind, "commit"); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("delta reader implicitly activated its undeclared tree capability", kind, err)
			}
		}
		if deltasOnly.ReadCount() != 2 || len(trees.commitCalls)+len(trees.nodeCalls) != 0 {
			test.Fatal("undeclared tree reader was discovered at runtime")
		}
	}
}

func TestGitEvidenceCachesCancellationOnlyAfterARealReaderCall(test *testing.T) {
	for _, kind := range []string{"commit", "node", "delta"} {
		test.Run(kind, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			trees := &gitEvidenceTestTrees{cancel: cancel}
			deltas := &gitEvidenceTestDeltas{cancel: cancel}
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees, Deltas: deltas})
			if _, err := gitEvidenceTestRead(ctx, evidence, gitEvidenceTestRepo, "", kind, "canceled"); !errors.Is(err, context.Canceled) || evidence.ReadCount() != 1 {
				test.Fatal("reader cancellation was not propagated and charged", err)
			}
			trees.cancel, deltas.cancel = nil, nil
			if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, "", kind, "canceled"); !errors.Is(err, context.Canceled) || evidence.ReadCount() != 1 || len(trees.commitCalls)+len(trees.nodeCalls)+len(deltas.calls) != 1 {
				test.Fatal("real reader cancellation was not cached", err)
			}
			if _, err := gitEvidenceTestRead(ctx, evidence, gitEvidenceTestRepo, "", kind, "never read"); !errors.Is(err, context.Canceled) || evidence.ReadCount() != 1 {
				test.Fatal("pre-read cancellation consumed a read", err)
			}
			if _, err := gitEvidenceTestRead(context.Background(), evidence, gitEvidenceTestRepo, "", kind, "never read"); !errors.Is(err, domain.ErrNotFound) || evidence.ReadCount() != 2 {
				test.Fatal("pre-read cancellation was incorrectly cached", err)
			}
		})
	}
}

func TestGitEvidenceRelationResumesAndSharesCommitCache(test *testing.T) {
	trees := &gitEvidenceTestTrees{commits: gitEvidenceTestChain(3)}
	evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees})
	for _, scenario := range []struct {
		source   string
		selected string
		relation string
		reads    int
	}{
		{"commit-0", "commit-0", "ancestor", 0},
		{"commit-1", "commit-0", "ancestor", 1},
		{"commit-2", "commit-0", "ancestor", 2},
		{"unrelated", "commit-0", "not_ancestor", 3},
		{"commit-2", "commit-1", "ancestor", 3},
		{"commit-1", "commit-0", "ancestor", 3},
	} {
		relation, err := evidence.Relation(context.Background(), scenario.source, scenario.selected)
		if err != nil || relation != scenario.relation || evidence.ReadCount() != scenario.reads || len(trees.commitCalls) != scenario.reads {
			test.Fatal("ancestor walk rescanned, failed to resume, or lost its selected-root scope", scenario, relation, err, evidence.ReadCount())
		}
	}
	if _, err := evidence.GetGitCommitTree(context.Background(), gitEvidenceTestRepo, "", "commit-1"); err != nil || evidence.ReadCount() != 3 {
		test.Fatal("relation used a separate commit cache", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, selected := range []string{"commit-0", "new selected"} {
		if relation, err := evidence.Relation(canceled, "commit-1", selected); relation != "" || !errors.Is(err, context.Canceled) || evidence.ReadCount() != 3 {
			test.Fatal("relation ignored cancellation on an existing/new walk", relation, err)
		}
	}
	if evidence.Limited() || evidence.TraversalLimited() {
		test.Fatal("short resumed walk exhausted a bound")
	}
}

func TestGitEvidenceRelationMissingEvidenceIsUnknown(test *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, present := range []bool{false, true} {
			test.Run(fmt.Sprintf("reader=%t/strict=%t", present, strict), func(test *testing.T) {
				trees := &gitEvidenceTestTrees{commitErrors: map[string]error{"selected": fmt.Errorf("missing commit: %w", domain.ErrNotFound)}}
				config := GitEvidenceConfig{RepoID: gitEvidenceTestRepo, RequireComplete: strict}
				if present {
					config.Trees = trees
				}
				evidence := gitEvidenceTestOwner(test, config)
				for repeat := 0; repeat < 2; repeat++ {
					if relation, err := evidence.Relation(context.Background(), "source", "selected"); relation != "unknown" || err != nil || evidence.ReadCount() != 1 {
						test.Fatal("missing evidence became a proven absence or a strict feature gate", relation, err)
					}
				}
				if evidence.Limited() || evidence.TraversalLimited() || present && len(trees.commitCalls) != 1 || !present && len(trees.commitCalls) != 0 {
					test.Fatal("incomplete closure performed repeated or undeclared reads")
				}
			})
		}
	}
}

func TestGitEvidenceRelationCyclesAndRepeatedParentsTerminate(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		parents map[string][]string
	}{
		{"self cycle", map[string][]string{"root": {"root"}}},
		{"cycle", map[string][]string{"root": {"middle"}, "middle": {"root"}}},
		{"repeated parent", map[string][]string{"root": {"leaf", "leaf"}, "leaf": {}}},
		{"diamond", map[string][]string{"root": {"left", "right"}, "left": {"leaf"}, "right": {"leaf"}, "leaf": {}}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			trees := &gitEvidenceTestTrees{commits: map[string]domain.GitCommitTree{}}
			for id, parents := range scenario.parents {
				trees.commits[id] = domain.GitCommitTree{Commit: id, Parents: parents}
			}
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees})
			for repeat := 0; repeat < 2; repeat++ {
				if relation, err := evidence.Relation(context.Background(), "unrelated", "root"); relation != "not_ancestor" || err != nil || evidence.ReadCount() != len(scenario.parents) || len(trees.commitCalls) != len(scenario.parents) {
					test.Fatal("cyclic/shared parents were reread or prevented termination", relation, err, evidence.ReadCount())
				}
			}
			for id := range scenario.parents {
				if relation, err := evidence.Relation(context.Background(), id, "root"); relation != "ancestor" || err != nil || evidence.ReadCount() != len(scenario.parents) {
					test.Fatal("completed closure lost a proven ancestor", id, relation, err)
				}
			}
			if evidence.Limited() || evidence.TraversalLimited() {
				test.Fatal("cycle was mistaken for budget exhaustion")
			}
		})
	}
}

func TestGitEvidenceRelationTraversalBoundary(test *testing.T) {
	if contextprojection.MaxProjectionSnapshots != 4096 || contextprojection.MaxCodeEvidenceReads != 8192 {
		test.Fatal("shared projection limit contract changed")
	}
	bound := contextprojection.MaxProjectionSnapshots
	for _, strict := range []bool{false, true} {
		for _, scenario := range []struct {
			name     string
			count    int
			source   string
			relation string
			limited  bool
		}{
			{"exact completed closure", bound, "unrelated", "not_ancestor", false},
			{"proven ancestor at frontier", bound + 1, fmt.Sprintf("commit-%d", bound), "ancestor", false},
			{"unproven ancestor beyond frontier", bound + 2, fmt.Sprintf("commit-%d", bound+1), "unknown", true},
		} {
			test.Run(fmt.Sprintf("%s/strict=%t", scenario.name, strict), func(test *testing.T) {
				trees := &gitEvidenceTestTrees{commits: gitEvidenceTestChain(scenario.count)}
				evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees, RequireComplete: strict})
				wantRelation := scenario.relation
				var wantErr error
				if scenario.limited && strict {
					wantRelation, wantErr = "", domain.ErrMemoryProjectionLimit
				}
				for repeat := 0; repeat < 2; repeat++ {
					relation, err := evidence.Relation(context.Background(), scenario.source, "commit-0")
					if relation != wantRelation || !errors.Is(err, wantErr) || evidence.ReadCount() != bound || len(trees.commitCalls) != bound || evidence.TraversalLimited() != scenario.limited || evidence.Limited() {
						test.Fatal("ancestor traversal boundary or retained closure changed", relation, err, evidence.ReadCount(), evidence.TraversalLimited())
					}
				}
				if scenario.limited {
					if relation, err := evidence.Relation(context.Background(), fmt.Sprintf("commit-%d", bound), "commit-0"); relation != "ancestor" || err != nil || evidence.ReadCount() != bound || !evidence.TraversalLimited() {
						test.Fatal("traversal exhaustion erased an already proven ancestor", relation, err)
					}
				}
			})
		}
	}
}

func TestGitEvidenceRelationUsesTheSharedReadBudget(test *testing.T) {
	for _, strict := range []bool{false, true} {
		test.Run(fmt.Sprintf("strict=%t", strict), func(test *testing.T) {
			trees := &gitEvidenceTestTrees{commits: gitEvidenceTestChain(2)}
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees, RequireComplete: strict})
			for position := 0; position < contextprojection.MaxCodeEvidenceReads-1; position++ {
				if err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, ""); err != nil {
					test.Fatal("reserve preceding request work", err)
				}
			}
			wantRelation := "unknown"
			var wantErr error
			if strict {
				wantRelation, wantErr = "", domain.ErrMemoryProjectionLimit
			}
			relation, err := evidence.Relation(context.Background(), "unrelated", "commit-0")
			if relation != wantRelation || !errors.Is(err, wantErr) || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads || !evidence.Limited() || evidence.TraversalLimited() || len(trees.commitCalls) != 1 {
				test.Fatal("ancestor traversal used an independent read budget", relation, err, evidence.ReadCount())
			}
			if _, err, found := evidence.CachedCommit("commit-1"); found || err != nil {
				test.Fatal("read-budget rejection entered the commit cache", err, found)
			}
			if _, err := evidence.GetGitCommitTree(context.Background(), gitEvidenceTestRepo, "", "commit-0"); err != nil || evidence.ReadCount() != contextprojection.MaxCodeEvidenceReads {
				test.Fatal("exhaustion invalidated a successfully read commit", err)
			}
		})
	}
}

func TestGitEvidenceExhaustTraversalIsMonotonicAndSeparateFromReads(test *testing.T) {
	for _, strict := range []bool{false, true} {
		evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, RequireComplete: strict})
		var wantErr error
		if strict {
			wantErr = domain.ErrMemoryProjectionLimit
		}
		for repeat := 0; repeat < 2; repeat++ {
			if err := evidence.ExhaustTraversal(); !errors.Is(err, wantErr) || !evidence.TraversalLimited() || evidence.Limited() || evidence.ReadCount() != 0 {
				test.Fatal("traversal exhaustion changed unrelated state", strict, err)
			}
		}
		if err := evidence.ConsumeRead(context.Background(), gitEvidenceTestRepo, ""); err != nil || evidence.ReadCount() != 1 || !evidence.TraversalLimited() || evidence.Limited() {
			test.Fatal("admission reset traversal state or treated strictness as activation", err)
		}
	}
}

func TestGitEvidenceRelationCancellationDuringReaderCall(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trees := &gitEvidenceTestTrees{commits: gitEvidenceTestChain(3), cancel: cancel, cancelAt: 2}
	config := GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees}
	evidence := gitEvidenceTestOwner(test, config)
	if relation, err := evidence.Relation(ctx, "commit-2", "commit-0"); relation != "" || !errors.Is(err, context.Canceled) || evidence.ReadCount() != 2 || len(trees.commitCalls) != 2 {
		test.Fatal("ancestor walk did not stop on reader cancellation", relation, err, evidence.ReadCount())
	}
	if _, err, found := evidence.CachedCommit("commit-1"); !found || !errors.Is(err, context.Canceled) {
		test.Fatal("real canceled reader result was not retained", err, found)
	}
	trees.cancel = nil
	fresh := gitEvidenceTestOwner(test, config)
	if relation, err := fresh.Relation(context.Background(), "commit-2", "commit-0"); relation != "ancestor" || err != nil || fresh.ReadCount() != 2 || evidence.ReadCount() != 2 {
		test.Fatal("a new request inherited the canceled walk/cache", relation, err)
	}
}

func TestGitEvidenceSeparateRequestsOwnTheirCachesClosuresAndBudgets(test *testing.T) {
	trees := &gitEvidenceTestTrees{commits: map[string]domain.GitCommitTree{}, nodes: map[string]domain.GitTreeNode{}}
	deltas := &gitEvidenceTestDeltas{values: map[[2]string]domain.GitCommitDelta{}}
	config := GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees, Deltas: deltas}
	first, second := gitEvidenceTestOwner(test, config), gitEvidenceTestOwner(test, config)
	for _, kind := range []string{"commit", "node", "delta"} {
		if _, err := gitEvidenceTestRead(context.Background(), first, gitEvidenceTestRepo, "", kind, "shared"); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("prime first request missing evidence", kind, err)
		}
	}
	if relation, err := first.Relation(context.Background(), "parent", "shared"); relation != "unknown" || err != nil || first.ReadCount() != 3 {
		test.Fatal("prime first request incomplete closure", relation, err)
	}
	trees.commits["shared"] = domain.GitCommitTree{Commit: "shared", Parents: []string{"parent"}}
	trees.nodes["shared"] = domain.GitTreeNode{OID: "shared"}
	deltas.values[[2]string{"shared", "parent"}] = domain.GitCommitDelta{Commit: "shared", Parent: "parent"}
	for _, kind := range []string{"commit", "node", "delta"} {
		if _, err := gitEvidenceTestRead(context.Background(), second, gitEvidenceTestRepo, "", kind, "shared"); err != nil {
			test.Fatal("second request inherited missing evidence", kind, err)
		}
		if _, err := gitEvidenceTestRead(context.Background(), first, gitEvidenceTestRepo, "", kind, "shared"); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("later repository evidence mutated first request cache", kind, err)
		}
	}
	if relation, err := second.Relation(context.Background(), "parent", "shared"); relation != "ancestor" || err != nil || second.ReadCount() != 3 {
		test.Fatal("second request inherited an incomplete closure", relation, err)
	}
	if relation, err := first.Relation(context.Background(), "parent", "shared"); relation != "unknown" || err != nil || first.ReadCount() != 3 {
		test.Fatal("second request mutated the first request closure", relation, err)
	}
	for position := first.ReadCount(); position < contextprojection.MaxCodeEvidenceReads; position++ {
		if err := first.ConsumeRead(context.Background(), gitEvidenceTestRepo, ""); err != nil {
			test.Fatal("consume first request budget", err)
		}
	}
	if err := first.ConsumeRead(context.Background(), gitEvidenceTestRepo, ""); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("exhaust first request budget", err)
	}
	if err := first.ExhaustTraversal(); err != nil || !first.Limited() || !first.TraversalLimited() {
		test.Fatal("exhaust first request traversal", err)
	}
	if second.Limited() || second.TraversalLimited() || second.ReadCount() != 3 {
		test.Fatal("request limit state was shared")
	}
	if err := second.ConsumeRead(context.Background(), gitEvidenceTestRepo, ""); err != nil || second.ReadCount() != 4 || first.ReadCount() != contextprojection.MaxCodeEvidenceReads {
		test.Fatal("one request consumed or reset another request's budget", err)
	}
}

func TestGitEvidenceRelationRetainsReaderErrorOnRetry(test *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{"integrity", domain.ErrIntegrity},
		{"transient", errors.New("temporary Git tree reader failure")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			trees := &gitEvidenceTestTrees{commitErrors: map[string]error{"selected": scenario.err}}
			evidence := gitEvidenceTestOwner(test, GitEvidenceConfig{RepoID: gitEvidenceTestRepo, Trees: trees})
			for attempt := 0; attempt < 2; attempt++ {
				relation, err := evidence.Relation(context.Background(), "source", "selected")
				if relation != "" || !errors.Is(err, scenario.err) {
					test.Fatalf("attempt %d changed a reader failure into ancestry evidence: relation=%q error=%v, want empty relation and %v", attempt+1, relation, err, scenario.err)
				}
				if evidence.ReadCount() != 1 || len(trees.commitCalls) != 1 {
					test.Fatal("retry must reuse the actual reader error without new I/O", evidence.ReadCount(), trees.commitCalls)
				}
			}
		})
	}
}
