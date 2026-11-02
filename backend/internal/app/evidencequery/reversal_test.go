package evidencequery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const reversalTestOrigin = "https://example.invalid/reversal.git"

type reversalTestDeltaCall struct {
	ctx    context.Context
	repo   domain.ContentHash
	origin string
	commit string
	parent string
}

type reversalTestAncestorCall struct {
	ctx        context.Context
	origin     string
	ancestor   string
	descendant string
}

type reversalTestProvider struct {
	deltas        map[string]domain.GitCommitDelta
	deltaErrors   map[string]error
	deltaCalls    []reversalTestDeltaCall
	ancestor      bool
	ancestorError error
	ancestorCalls []reversalTestAncestorCall
}

func (provider *reversalTestProvider) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	provider.deltaCalls = append(provider.deltaCalls, reversalTestDeltaCall{ctx: ctx, origin: origin, commit: commit, parent: parent})
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	delta, found := provider.deltas[commit]
	if err := provider.deltaErrors[commit]; err != nil {
		return delta, err
	}
	if !found {
		return delta, domain.ErrNotFound
	}
	return delta, nil
}

func (provider *reversalTestProvider) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	provider.ancestorCalls = append(provider.ancestorCalls, reversalTestAncestorCall{ctx: ctx, origin: origin, ancestor: ancestor, descendant: descendant})
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return provider.ancestor, provider.ancestorError
}

type reversalTestCache struct {
	deltas map[[2]string]domain.GitCommitDelta
	errors map[[2]string]error
	calls  []reversalTestDeltaCall
}

func (cache *reversalTestCache) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	cache.calls = append(cache.calls, reversalTestDeltaCall{ctx: ctx, repo: repo, origin: origin, commit: commit, parent: parent})
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	key := [2]string{commit, parent}
	delta, found := cache.deltas[key]
	if err := cache.errors[key]; err != nil {
		return delta, err
	}
	if !found {
		return delta, domain.ErrNotFound
	}
	return delta, nil
}

var _ ReversalReader = (*reversalTestProvider)(nil)
var _ ReversalReader = (*CachedReversalReader)(nil)
var _ GitDeltaReader = (*reversalTestCache)(nil)

func reversalTestOID(value int) string {
	return fmt.Sprintf("%040x", value)
}

func reversalTestPair() (domain.GitCommitDelta, domain.GitCommitDelta) {
	change := domain.GitPathChange{
		Path:   "feature",
		Before: domain.GitEntry{OID: reversalTestOID(5), Mode: "100644"},
		After:  domain.GitEntry{OID: reversalTestOID(6), Mode: "100755"},
	}
	before := domain.GitCommitDelta{
		Commit: reversalTestOID(2), Parents: []string{reversalTestOID(1)}, Parent: reversalTestOID(1),
		Changes: []domain.GitPathChange{change}, Complete: true,
	}
	after := domain.GitCommitDelta{
		Commit: reversalTestOID(4), Parents: []string{reversalTestOID(3)}, Parent: reversalTestOID(3),
		Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}, Complete: true,
	}
	return before, after
}

func reversalTestReader(test testing.TB, repo domain.ContentHash, cache GitDeltaReader, provider ReversalReader) *CachedReversalReader {
	test.Helper()
	reader, err := NewCachedReversalReader(CachedReversalConfig{RepoID: repo, Cache: cache, Provider: provider})
	if err != nil || reader == nil {
		test.Fatal("construct cached reversal reader", reader, err)
	}
	return reader
}

func TestCachedReversalConstructorRequiresDeclaredReadersWithoutIO(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	cache := &reversalTestCache{}
	provider := &reversalTestProvider{}
	reader := reversalTestReader(test, repo, cache, provider)
	if reader.repo != repo || reader.cache != cache || reader.provider != provider {
		test.Fatal("constructor replaced declared repository or readers", reader)
	}
	var missingCache *reversalTestCache
	var missingProvider *reversalTestProvider
	for _, scenario := range []struct {
		name   string
		config CachedReversalConfig
	}{
		{name: "nil cache", config: CachedReversalConfig{RepoID: repo, Provider: provider}},
		{name: "typed nil cache", config: CachedReversalConfig{RepoID: repo, Cache: missingCache, Provider: provider}},
		{name: "nil provider", config: CachedReversalConfig{RepoID: repo, Cache: cache}},
		{name: "typed nil provider", config: CachedReversalConfig{RepoID: repo, Cache: cache, Provider: missingProvider}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if reader, err := NewCachedReversalReader(scenario.config); reader != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("missing reader accepted", reader, err)
			}
		})
	}
	if len(cache.calls) != 0 || len(provider.deltaCalls) != 0 || len(provider.ancestorCalls) != 0 {
		test.Fatal("constructor performed evidence I/O")
	}
}

func TestVerifyReversalPreservesAssessmentAndBoundInputs(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		mutate   func(*domain.GitCommitDelta, *domain.GitCommitDelta)
		coverage string
		reason   string
		ancestry bool
	}{
		{name: "full", coverage: "full", ancestry: true},
		{name: "partial", mutate: func(before, after *domain.GitCommitDelta) {
			before.Changes = append(before.Changes, domain.GitPathChange{Path: "remaining", After: before.Changes[0].After})
		}, coverage: "partial", reason: "remaining_paths_need_review", ancestry: true},
		{name: "unrelated extra path", mutate: func(before, after *domain.GitCommitDelta) {
			after.Changes = append(after.Changes, domain.GitPathChange{Path: "other", After: before.Changes[0].After})
		}, coverage: "full", ancestry: true},
		{name: "no exact inverse", mutate: func(before, after *domain.GitCommitDelta) {
			after.Changes[0].After.Mode = "100755"
		}, coverage: "unverified", reason: "no_exact_inverse", ancestry: true},
		{name: "incomplete target", mutate: func(before, after *domain.GitCommitDelta) {
			before.Complete = false
		}, coverage: "unverified", reason: "incomplete_git_evidence", ancestry: true},
		{name: "incomplete candidate", mutate: func(before, after *domain.GitCommitDelta) {
			after.Complete = false
		}, coverage: "unverified", reason: "incomplete_git_evidence", ancestry: true},
		{name: "empty target", mutate: func(before, after *domain.GitCommitDelta) {
			before.Changes = nil
		}, coverage: "unverified", reason: "target_has_no_changes", ancestry: true},
		{name: "root candidate", mutate: func(before, after *domain.GitCommitDelta) {
			after.Parents = nil
			after.Parent = ""
		}, coverage: "unverified", reason: "candidate_has_no_parent"},
		{name: "explicit merge parent", mutate: func(before, after *domain.GitCommitDelta) {
			before.Parents = append(before.Parents, reversalTestOID(7))
			after.Parents = append(after.Parents, reversalTestOID(8))
		}, coverage: "full", ancestry: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			before, after := reversalTestPair()
			if scenario.mutate != nil {
				scenario.mutate(&before, &after)
			}
			expected, err := domain.AssessGitReversal(before, after)
			if err != nil || expected.Coverage != scenario.coverage || expected.Reason != scenario.reason {
				test.Fatal("invalid assessment fixture", expected, err)
			}
			provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{before.Commit: before, after.Commit: after}, ancestor: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			actual, err := VerifyReversal(ctx, provider, reversalTestOrigin, before.Commit, before.Parent, after.Commit, after.Parent)
			if err != nil || !reflect.DeepEqual(actual, expected) {
				test.Fatal("assessment changed", actual, expected, err)
			}
			calls := []reversalTestDeltaCall{
				{ctx: ctx, origin: reversalTestOrigin, commit: before.Commit, parent: before.Parent},
				{ctx: ctx, origin: reversalTestOrigin, commit: after.Commit, parent: after.Parent},
			}
			if !reflect.DeepEqual(provider.deltaCalls, calls) {
				test.Fatal("delta order, context, origin or selected parent changed", provider.deltaCalls)
			}
			var ancestry []reversalTestAncestorCall
			if scenario.ancestry {
				ancestry = []reversalTestAncestorCall{{ctx: ctx, origin: reversalTestOrigin, ancestor: before.Commit, descendant: after.Parent}}
			}
			if !reflect.DeepEqual(provider.ancestorCalls, ancestry) {
				test.Fatal("ancestry did not use the selected candidate parent", provider.ancestorCalls)
			}
		})
	}
}

func TestVerifyReversalRejectsForeignObjectsAndInvalidDeltasBeforeAncestry(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*domain.GitCommitDelta, *domain.GitCommitDelta)
		want   error
	}{
		{name: "foreign target", mutate: func(before, after *domain.GitCommitDelta) { before.Commit = reversalTestOID(9) }, want: domain.ErrIntegrity},
		{name: "foreign candidate", mutate: func(before, after *domain.GitCommitDelta) { after.Commit = reversalTestOID(9) }, want: domain.ErrIntegrity},
		{name: "different target parent", mutate: func(before, after *domain.GitCommitDelta) { before.Parent = reversalTestOID(9) }, want: domain.ErrIntegrity},
		{name: "different candidate parent", mutate: func(before, after *domain.GitCommitDelta) { after.Parent = reversalTestOID(9) }, want: domain.ErrIntegrity},
		{name: "invalid target", mutate: func(before, after *domain.GitCommitDelta) { before.Changes[0].Path = "../foreign" }, want: domain.ErrValidation},
		{name: "invalid candidate", mutate: func(before, after *domain.GitCommitDelta) { after.Changes[0].After.Mode = "invalid" }, want: domain.ErrValidation},
		{name: "ambiguous target merge", mutate: func(before, after *domain.GitCommitDelta) {
			before.Parents = append(before.Parents, reversalTestOID(9))
			before.Parent = ""
		}, want: domain.ErrValidation},
		{name: "ambiguous candidate merge", mutate: func(before, after *domain.GitCommitDelta) {
			after.Parents = append(after.Parents, reversalTestOID(9))
			after.Parent = ""
		}, want: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			before, after := reversalTestPair()
			target, candidate := before.Commit, after.Commit
			targetParent, candidateParent := before.Parent, after.Parent
			scenario.mutate(&before, &after)
			if scenario.name == "ambiguous target merge" {
				targetParent = ""
			}
			if scenario.name == "ambiguous candidate merge" {
				candidateParent = ""
			}
			provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{target: before, candidate: after}, ancestor: true}
			proof, err := VerifyReversal(context.Background(), provider, reversalTestOrigin, target, targetParent, candidate, candidateParent)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(proof, domain.GitReversalEvidence{}) || len(provider.deltaCalls) != 2 || len(provider.ancestorCalls) != 0 {
				test.Fatal("foreign or invalid evidence reached ancestry", proof, err, provider.deltaCalls, provider.ancestorCalls)
			}
		})
	}
}

func TestVerifyReversalPreservesNegativeAncestryAndOptionalParents(test *testing.T) {
	before, after := reversalTestPair()
	for _, ancestor := range []bool{false, true} {
		provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{before.Commit: before, after.Commit: after}, ancestor: ancestor}
		proof, err := VerifyReversal(context.Background(), provider, reversalTestOrigin, before.Commit, "", after.Commit, "")
		if err != nil || len(provider.deltaCalls) != 2 || provider.deltaCalls[0].parent != "" || provider.deltaCalls[1].parent != "" {
			test.Fatal("optional parents changed", proof, err, provider.deltaCalls)
		}
		expected, err := domain.AssessGitReversal(before, after)
		if err != nil {
			test.Fatal(err)
		}
		if !ancestor {
			expected = domain.GitReversalEvidence{Commit: after.Commit, Target: before.Commit, Parent: after.Parent, TargetParent: before.Parent,
				Coverage: "unverified", Reason: "target_not_in_candidate_ancestry", Paths: []string{}, UnverifiedPaths: []string{}, OtherPaths: []string{}}
		}
		if !reflect.DeepEqual(proof, expected) {
			test.Fatal("ancestry rejection or inferred parent result changed", proof, expected)
		}
	}
}

func TestVerifyReversalPreservesErrorsAndReadOrder(test *testing.T) {
	before, after := reversalTestPair()
	failure := errors.New("provider temporarily unavailable")
	for _, stage := range []string{"target", "candidate", "ancestry"} {
		test.Run(stage, func(test *testing.T) {
			provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{before.Commit: before, after.Commit: after}, deltaErrors: map[string]error{}, ancestor: true}
			wantReads, wantAncestry := 2, 0
			switch stage {
			case "target":
				provider.deltaErrors[before.Commit] = failure
				wantReads = 1
			case "candidate":
				provider.deltas[before.Commit] = domain.GitCommitDelta{}
				provider.deltaErrors[after.Commit] = failure
			case "ancestry":
				provider.ancestorError = failure
				wantAncestry = 1
			}
			proof, err := VerifyReversal(context.Background(), provider, reversalTestOrigin, before.Commit, "", after.Commit, "")
			if err != failure || !reflect.DeepEqual(proof, domain.GitReversalEvidence{}) || len(provider.deltaCalls) != wantReads || len(provider.ancestorCalls) != wantAncestry {
				test.Fatal("provider error was replaced or later reads ran", proof, err, provider.deltaCalls, provider.ancestorCalls)
			}
		})
	}
	var missing *reversalTestProvider
	for _, provider := range []ReversalReader{nil, missing} {
		if proof, err := VerifyReversal(context.Background(), provider, reversalTestOrigin, before.Commit, "", after.Commit, ""); err == nil || !reflect.DeepEqual(proof, domain.GitReversalEvidence{}) {
			test.Fatal("nil reader accepted", proof, err)
		}
	}
}

func TestCachedReversalDeltaPreservesRepositoryContextAndFallbackErrors(test *testing.T) {
	before, after := reversalTestPair()
	repo := domain.HashContent([]byte(test.Name()))
	failure := errors.New("cache unavailable")
	for _, scenario := range []struct {
		name          string
		cacheError    error
		providerError error
		fallback      bool
	}{
		{name: "hit"},
		{name: "cache failure", cacheError: failure},
		{name: "missing", cacheError: domain.ErrNotFound, fallback: true},
		{name: "wrapped missing", cacheError: fmt.Errorf("missing delta: %w", domain.ErrNotFound), fallback: true},
		{name: "provider failure", cacheError: domain.ErrNotFound, providerError: failure, fallback: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			key := [2]string{before.Commit, before.Parent}
			cache := &reversalTestCache{deltas: map[[2]string]domain.GitCommitDelta{key: before}, errors: map[[2]string]error{key: scenario.cacheError}}
			provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{before.Commit: after}, deltaErrors: map[string]error{before.Commit: scenario.providerError}}
			reader := reversalTestReader(test, repo, cache, provider)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			delta, err := reader.ReadCommitDelta(ctx, reversalTestOrigin, before.Commit, before.Parent)
			expected, wantError := before, scenario.cacheError
			if scenario.fallback {
				expected, wantError = after, scenario.providerError
			}
			if err != wantError || !reflect.DeepEqual(delta, expected) {
				test.Fatal("cache/provider value or error was replaced", delta, err, expected, wantError)
			}
			if !reflect.DeepEqual(cache.calls, []reversalTestDeltaCall{{ctx: ctx, repo: repo, origin: reversalTestOrigin, commit: before.Commit, parent: before.Parent}}) {
				test.Fatal("cache repository key or context changed", cache.calls)
			}
			var calls []reversalTestDeltaCall
			if scenario.fallback {
				calls = []reversalTestDeltaCall{{ctx: ctx, origin: reversalTestOrigin, commit: before.Commit, parent: before.Parent}}
			}
			if !reflect.DeepEqual(provider.deltaCalls, calls) || len(provider.ancestorCalls) != 0 {
				test.Fatal("fallback changed context or performed unexpected I/O", provider.deltaCalls, provider.ancestorCalls)
			}
		})
	}
}

func TestCachedReversalAncestryPreservesClosureUnknownAndCycles(test *testing.T) {
	descendant, middle, ancestor, missing := reversalTestOID(10), reversalTestOID(11), reversalTestOID(12), reversalTestOID(13)
	failure := errors.New("cached parent unavailable")
	for _, scenario := range []struct {
		name          string
		parents       map[string][]string
		cacheError    error
		provider      bool
		providerError error
		want          bool
		wantError     error
		reads         int
		fallback      bool
	}{
		{name: "complete positive", parents: map[string][]string{descendant: {middle}, middle: {ancestor}}, want: true, reads: 2},
		{name: "complete negative", parents: map[string][]string{descendant: {middle}, middle: {}}, provider: true, reads: 2},
		{name: "merge second parent", parents: map[string][]string{descendant: {middle, ancestor}}, want: true, reads: 1},
		{name: "missing root", provider: true, want: true, reads: 1, fallback: true},
		{name: "missing parent", parents: map[string][]string{descendant: {missing}}, provider: true, want: true, reads: 2, fallback: true},
		{name: "missing branch negative", parents: map[string][]string{descendant: {middle, missing}, middle: {}}, reads: 3, fallback: true},
		{name: "known branch overrides unknown", parents: map[string][]string{descendant: {middle, missing}, middle: {ancestor}}, want: true, reads: 3},
		{name: "cycle terminates negative", parents: map[string][]string{descendant: {middle}, middle: {descendant}}, provider: true, reads: 2},
		{name: "duplicate edge read once", parents: map[string][]string{descendant: {middle, middle}, middle: {}}, provider: true, reads: 2},
		{name: "wrapped missing", cacheError: fmt.Errorf("missing parents: %w", domain.ErrNotFound), provider: true, want: true, reads: 1, fallback: true},
		{name: "cache failure", cacheError: failure, wantError: failure, reads: 1},
		{name: "provider failure", providerError: failure, wantError: failure, reads: 1, fallback: true},
		{name: "provider value and error", provider: true, providerError: failure, want: true, wantError: failure, reads: 1, fallback: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			cache := &reversalTestCache{deltas: map[[2]string]domain.GitCommitDelta{}, errors: map[[2]string]error{}}
			for commit, parents := range scenario.parents {
				cache.deltas[[2]string{commit, ""}] = domain.GitCommitDelta{Commit: commit, Parents: parents}
			}
			if scenario.cacheError != nil {
				cache.errors[[2]string{descendant, ""}] = scenario.cacheError
			}
			provider := &reversalTestProvider{ancestor: scenario.provider, ancestorError: scenario.providerError}
			repo := domain.HashContent([]byte(test.Name()))
			reader := reversalTestReader(test, repo, cache, provider)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			actual, err := reader.IsGitAncestor(ctx, reversalTestOrigin, ancestor, descendant)
			if actual != scenario.want || err != scenario.wantError || len(cache.calls) != scenario.reads {
				test.Fatal("cached closure result, error or read count changed", actual, err, cache.calls)
			}
			for _, call := range cache.calls {
				if call.ctx != ctx || call.repo != repo || call.origin != reversalTestOrigin || call.parent != "" {
					test.Fatal("ancestry cache lookup changed repository, context or parent selection", call)
				}
			}
			var calls []reversalTestAncestorCall
			if scenario.fallback {
				calls = []reversalTestAncestorCall{{ctx: ctx, origin: reversalTestOrigin, ancestor: ancestor, descendant: descendant}}
			}
			if !reflect.DeepEqual(provider.ancestorCalls, calls) || len(provider.deltaCalls) != 0 {
				test.Fatal("ancestry fallback changed original query or context", provider.ancestorCalls, provider.deltaCalls)
			}
		})
	}
}

func TestCachedReversalAncestryRetainsVisitedLimitAndReflexivity(test *testing.T) {
	for _, count := range []int{255, 256, 257} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			cache := &reversalTestCache{deltas: map[[2]string]domain.GitCommitDelta{}}
			for position := 1; position <= count; position++ {
				commit := reversalTestOID(position)
				parents := []string{}
				if position < count {
					parents = append(parents, reversalTestOID(position+1))
				}
				cache.deltas[[2]string{commit, ""}] = domain.GitCommitDelta{Commit: commit, Parents: parents, Complete: true}
			}
			provider := &reversalTestProvider{ancestor: true}
			reader := reversalTestReader(test, domain.HashContent([]byte(test.Name())), cache, provider)
			ancestor, descendant := reversalTestOID(count+1), reversalTestOID(1)
			actual, err := reader.IsGitAncestor(context.Background(), reversalTestOrigin, ancestor, descendant)
			wantReads, wantFallback := count, 0
			if count > 256 {
				wantReads, wantFallback = 256, 1
			}
			if err != nil || actual != (wantFallback == 1) || len(cache.calls) != wantReads || len(provider.ancestorCalls) != wantFallback {
				test.Fatal("visited limit or complete negative changed", actual, err, len(cache.calls), len(provider.ancestorCalls))
			}
		})
	}
	cache, provider := &reversalTestCache{}, &reversalTestProvider{}
	reader := reversalTestReader(test, domain.HashContent([]byte(test.Name())), cache, provider)
	commit := reversalTestOID(1)
	if actual, err := reader.IsGitAncestor(context.Background(), reversalTestOrigin, commit, commit); err != nil || !actual || len(cache.calls) != 0 || len(provider.ancestorCalls) != 0 {
		test.Fatal("reflexive ancestry performed evidence I/O", actual, err)
	}
}

func TestCachedReversalDoesNotRetainAbsenceOrProviderResults(test *testing.T) {
	before, _ := reversalTestPair()
	cache := &reversalTestCache{deltas: map[[2]string]domain.GitCommitDelta{}}
	provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{before.Commit: before}}
	reader := reversalTestReader(test, domain.HashContent([]byte(test.Name())), cache, provider)
	for attempt := 0; attempt < 2; attempt++ {
		delta, err := reader.ReadCommitDelta(context.Background(), reversalTestOrigin, before.Commit, before.Parent)
		if err != nil || !reflect.DeepEqual(delta, before) {
			test.Fatal("provider result changed", delta, err)
		}
	}
	if len(cache.calls) != 2 || len(provider.deltaCalls) != 2 {
		test.Fatal("reader introduced another result or absence cache", cache.calls, provider.deltaCalls)
	}
	cache.deltas[[2]string{before.Commit, before.Parent}] = before
	if delta, err := reader.ReadCommitDelta(context.Background(), reversalTestOrigin, before.Commit, before.Parent); err != nil || !reflect.DeepEqual(delta, before) || len(cache.calls) != 3 || len(provider.deltaCalls) != 2 {
		test.Fatal("current cache response was replaced by retained provider evidence", delta, err)
	}
}

func TestCachedReversalAncestryChecksEqualityOnlyWithinVisitedLimit(test *testing.T) {
	for _, position := range []int{256, 257} {
		test.Run(fmt.Sprint(position), func(test *testing.T) {
			cache := &reversalTestCache{deltas: map[[2]string]domain.GitCommitDelta{}}
			for index := 1; index <= 256; index++ {
				commit := reversalTestOID(index)
				cache.deltas[[2]string{commit, ""}] = domain.GitCommitDelta{Commit: commit, Parents: []string{reversalTestOID(index + 1)}}
			}
			provider := &reversalTestProvider{}
			reader := reversalTestReader(test, domain.HashContent([]byte(test.Name())), cache, provider)
			ancestor, descendant := reversalTestOID(position), reversalTestOID(1)
			actual, err := reader.IsGitAncestor(context.Background(), reversalTestOrigin, ancestor, descendant)
			wantFallback := 0
			if position == 257 {
				wantFallback = 1
			}
			if err != nil || actual != (position == 256) || len(cache.calls) != position-1 || len(provider.ancestorCalls) != wantFallback {
				test.Fatal("equality bypassed original visited limit", actual, err, len(cache.calls), provider.ancestorCalls)
			}
		})
	}
}

func TestVerifyReversalUsesCachedPolicyAndLeavesContextBindingToAdapter(test *testing.T) {
	for _, foreign := range []bool{false, true} {
		test.Run(fmt.Sprint(foreign), func(test *testing.T) {
			before, after := reversalTestPair()
			cached := before
			if foreign {
				cached.Commit = reversalTestOID(9)
			}
			cache := &reversalTestCache{deltas: map[[2]string]domain.GitCommitDelta{{before.Commit, before.Parent}: cached}}
			provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{after.Commit: after}, ancestor: true}
			repo := domain.HashContent([]byte(test.Name()))
			reader := reversalTestReader(test, repo, cache, provider)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proof, err := VerifyReversal(ctx, reader, reversalTestOrigin, before.Commit, before.Parent, after.Commit, after.Parent)
			wantReads, wantAncestry := 2, 0
			if foreign {
				if err != domain.ErrIntegrity || !reflect.DeepEqual(proof, domain.GitReversalEvidence{}) {
					test.Fatal("foreign cached immutable object reached assessment", proof, err)
				}
			} else {
				wantReads, wantAncestry = 3, 1
				expected, assessError := domain.AssessGitReversal(before, after)
				if err != nil || assessError != nil || !reflect.DeepEqual(proof, expected) {
					test.Fatal("cached/provider reversal changed", proof, err, expected, assessError)
				}
			}
			if len(cache.calls) != wantReads || len(provider.deltaCalls) != 1 || len(provider.ancestorCalls) != wantAncestry {
				test.Fatal("unexpected cache/provider work", cache.calls, provider.deltaCalls, provider.ancestorCalls)
			}
			for _, call := range cache.calls {
				if call.ctx != ctx || call.repo != repo || call.origin != reversalTestOrigin {
					test.Fatal("cache lost existing context or repository binding", call)
				}
			}
			if provider.deltaCalls[0] != (reversalTestDeltaCall{ctx: ctx, origin: reversalTestOrigin, commit: after.Commit, parent: after.Parent}) {
				test.Fatal("delta fallback replaced adapter context", provider.deltaCalls)
			}
			if !foreign && provider.ancestorCalls[0] != (reversalTestAncestorCall{ctx: ctx, origin: reversalTestOrigin, ancestor: before.Commit, descendant: after.Parent}) {
				test.Fatal("ancestry fallback replaced adapter context", provider.ancestorCalls)
			}
		})
	}
}

func TestVerifyReversalPreservesAssessmentErrorResult(test *testing.T) {
	for _, root := range []bool{false, true} {
		test.Run(fmt.Sprint(root), func(test *testing.T) {
			delta, _ := reversalTestPair()
			if root {
				delta.Parents = nil
				delta.Parent = ""
			}
			provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{delta.Commit: delta}, ancestor: true}
			expected, assessError := domain.AssessGitReversal(delta, delta)
			if !errors.Is(assessError, domain.ErrValidation) {
				test.Fatal("self-reversal fixture did not reach assessment error", expected, assessError)
			}
			proof, err := VerifyReversal(context.Background(), provider, reversalTestOrigin, delta.Commit, delta.Parent, delta.Commit, delta.Parent)
			if !errors.Is(err, domain.ErrValidation) || err.Error() != assessError.Error() || !reflect.DeepEqual(proof, expected) {
				test.Fatal("assessment error lost its original result", proof, err, expected, assessError)
			}
		})
	}
}

func TestReversalCancellationRemainsDependencyError(test *testing.T) {
	before, after := reversalTestPair()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &reversalTestProvider{deltas: map[string]domain.GitCommitDelta{before.Commit: before, after.Commit: after}}
	if proof, err := VerifyReversal(ctx, provider, reversalTestOrigin, before.Commit, "", after.Commit, ""); err != context.Canceled || !reflect.DeepEqual(proof, domain.GitReversalEvidence{}) || len(provider.deltaCalls) != 1 || len(provider.ancestorCalls) != 0 {
		test.Fatal("canceled reversal continued or replaced error", proof, err)
	}
	cache := &reversalTestCache{}
	provider = &reversalTestProvider{}
	reader := reversalTestReader(test, domain.HashContent([]byte(test.Name())), cache, provider)
	if _, err := reader.ReadCommitDelta(ctx, reversalTestOrigin, before.Commit, before.Parent); err != context.Canceled {
		test.Fatal("canceled cache read changed error", err)
	}
	if ancestor, err := reader.IsGitAncestor(ctx, reversalTestOrigin, before.Commit, after.Commit); err != context.Canceled || ancestor {
		test.Fatal("canceled ancestry changed error", ancestor, err)
	}
	if len(cache.calls) != 2 || len(provider.deltaCalls) != 0 || len(provider.ancestorCalls) != 0 {
		test.Fatal("canceled cache error fell back to provider", cache.calls, provider.deltaCalls, provider.ancestorCalls)
	}
}
