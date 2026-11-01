package memoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLiveHasNoParentBackreferenceOrCapabilityRediscovery(test *testing.T) {
	files, err := filepath.Glob("live*.go")
	if err != nil || len(files) == 0 {
		test.Fatal("live sources unavailable", err)
	}
	for _, filename := range files {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			if imported.Path.Value == `"github.com/wnsdy95/cxthub/backend/internal/app"` || strings.Contains(imported.Path.Value, "/repositoryquery") {
				test.Fatalf("%s depends on the parent or unrelated sibling helper", filename)
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch expression := node.(type) {
			case *ast.TypeAssertExpr:
				test.Errorf("%s rediscovers a dependency capability", filename)
			case *ast.Ident:
				if expression.Name == "Service" {
					test.Errorf("%s retains a parent Service reference", filename)
				}
			}
			return true
		})
	}
	for _, shape := range []reflect.Type{reflect.TypeOf(LiveConfig{}), reflect.TypeOf(LiveQueries{}), reflect.TypeOf(liveEvidence{})} {
		for index := 0; index < shape.NumField(); index++ {
			field := shape.Field(index)
			if field.Anonymous || field.Type.Kind() == reflect.Func {
				test.Fatalf("%s.%s is an embedded backend or callback", shape.Name(), field.Name)
			}
		}
	}
}

func TestLiveDeliveryCacheIsSharedBoundedAndPolicyGated(test *testing.T) {
	source, config, request := liveFixture(test)
	queries := newLiveTestQueries(test, config)
	first, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
	if err != nil || config.Delivery.Len() != 1 {
		test.Fatal("proof was not cached", err, config.Delivery.Len())
	}
	secondOwner := newLiveTestQueries(test, config)
	if queries.delivery != config.Delivery || secondOwner.delivery != config.Delivery {
		test.Fatal("constructor replaced the shared proof cache")
	}
	before := source.repositoryReads
	second, err := secondOwner.QueryEffectiveMemory(context.Background(), source.repo, request)
	if err != nil || second.DeliveryStateHash != first.DeliveryStateHash || source.repositoryReads != before+1 {
		test.Fatal("shared proof miss", err, source.repositoryReads-before)
	}
	source.scope.cacheable = false
	before = source.repositoryReads
	if _, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request); err != nil || source.repositoryReads != before+2 {
		test.Fatal("unknown/write mode reused a process proof", err, source.repositoryReads-before)
	}
	config.ProofScope = nil
	uncached := newLiveTestQueries(test, config)
	before = source.repositoryReads
	if _, err := uncached.QueryEffectiveMemory(context.Background(), source.repo, request); err != nil || source.repositoryReads != before+2 {
		test.Fatal("absent policy reused a process proof", err)
	}
	cache := &DeliveryCache{}
	for index := 0; index < 65; index++ {
		cache.Store(domain.HashContent([]byte(fmt.Sprint(index))), domain.HashContent([]byte(fmt.Sprint("proof", index))))
	}
	if cache.Len() != 64 {
		test.Fatal("unbounded delivery cache", cache.Len())
	}
	if _, found := cache.Lookup(domain.HashContent([]byte("0"))); found {
		test.Fatal("oldest proof was not evicted")
	}
	state := domain.HashContent([]byte("64"))
	proof, found := cache.Lookup(state)
	cache.Store(state, domain.HashContent([]byte("replacement")))
	if retained, exists := cache.Lookup(state); !found || !exists || retained != proof {
		test.Fatal("a stable state proof was overwritten")
	}
}

func TestLiveBranchAndIntegrationShareOneHistoryBoundContributionCache(test *testing.T) {
	source, config, request := liveFixture(test)
	liveCodeFixture(test, source, &request)
	request.Selection.Branch, request.Selection.MemoryHash = "main", ""
	request.Content = "claims"
	queries := newLiveTestQueries(test, config)
	page, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
	if err != nil || len(page.Items) != 1 || page.Items[0].Reason != "equivalent_without_integration" || page.Inclusion == nil {
		test.Fatalf("actual branch/code assessment: %+v %v", page, err)
	}
	if source.historyReads != 1 || source.prLedgerReads != 1 || source.gitLedgerReads != 1 {
		test.Fatalf("repeated history/ledger normalization: history=%d PR=%d Git=%d", source.historyReads, source.prLedgerReads, source.gitLedgerReads)
	}
	if page.DeliveryStateHash == "" || page.Items[0].Paths[0].Selected != page.Items[0].Paths[0].After {
		test.Fatal("GitEvidence.Query did not provide actual path applicability")
	}
}

func TestLiveContributionCacheBindsHistoryBackingNotOnlyLength(test *testing.T) {
	source, config, _ := liveFixture(test)
	source.scoped = false
	queries := newLiveTestQueries(test, config)
	history := []domain.HistoryEvent{{ID: "accepted", RepoID: string(source.repo)}}
	evidence, err := queries.newLiveEvidence(context.Background(), source.repo, nil)
	if err != nil {
		test.Fatal(err)
	}
	first, err := queries.contextContributions(context.Background(), source.repo, history, evidence)
	if err != nil {
		test.Fatal(err)
	}
	otherEvidence, err := queries.newLiveEvidence(context.Background(), source.repo, first)
	if err != nil {
		test.Fatal(err)
	}
	if same, err := queries.contextContributions(context.Background(), source.repo, history, otherEvidence); err != nil || same != first || source.prLedgerReads != 1 || source.gitLedgerReads != 1 {
		test.Fatal("same history backing did not share the request cache", err)
	}
	copied := append([]domain.HistoryEvent(nil), history...)
	if _, err := queries.contextContributions(context.Background(), source.repo, copied, otherEvidence); err != nil || source.prLedgerReads != 2 || source.gitLedgerReads != 2 {
		test.Fatal("different history backing reused accepted contributions", err)
	}
	if err := first.bind(domain.HashContent([]byte("foreign")), history); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("contribution cache crossed repository authority", err)
	}
}

func TestLiveGenerationCompatibilityAndNoNewFallback(test *testing.T) {
	for _, scenario := range []string{"unpinned", "missing-reader", "graph-unavailable", "graph-error", "invalid-graph", "memory-unavailable", "memory-mismatch", "captured-profile"} {
		test.Run(scenario, func(test *testing.T) {
			source, config, request := liveFixture(test)
			var expected error
			switch scenario {
			case "unpinned":
				source.scope.pinned = false
			case "missing-reader":
				config.GraphGenerations = nil
			case "graph-unavailable":
				source.graphError = domain.ErrCatalogResetRequired
			case "graph-error":
				source.graphError = errors.New("graph storage failed")
				expected = source.graphError
			case "invalid-graph":
				source.generation.Graph.Scope = domain.RepositoryViewSourceScope
				expected = domain.ErrIntegrity
			case "memory-unavailable":
				source.generationError = domain.ErrCatalogUnsupported
				expected = domain.ErrCatalogUnsupported
			case "memory-mismatch":
				source.generation.Version = 0
				expected = domain.ErrIntegrity
			case "captured-profile":
				config.CapturedEvidence = true
			}
			queries := newLiveTestQueries(test, config)
			_, err := queries.QueryEffectiveMemory(context.Background(), source.repo, request)
			if !errors.Is(err, expected) {
				test.Fatalf("generation fallback changed: %v, want %v", err, expected)
			}
			if expected == nil && source.generationReads != 0 {
				test.Fatal("incomplete graph generation read a memory generation")
			}
		})
	}
}

func TestLiveCodeAssessmentFreshBudgetAndPublicationErrors(test *testing.T) {
	source, config, request := liveFixture(test)
	liveCodeFixture(test, source, &request)
	source.scoped = false
	queries := newLiveTestQueries(test, config)
	evidence, err := queries.newLiveEvidence(context.Background(), source.repo, nil)
	if err != nil {
		test.Fatal(err)
	}
	resolver, err := newMemoryIntegration(queries, evidence, source.repo, source.history)
	if err != nil {
		test.Fatal(err)
	}
	items, err := EffectiveItems(source.digest, "claims")
	if err != nil {
		test.Fatal(err)
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		test.Fatal(err)
	}
	for index := 0; index < contextprojection.MaxCodeEvidenceReads; index++ {
		if err := owner.ConsumeRead(context.Background(), source.repo, source.origin); err != nil {
			test.Fatal(err)
		}
	}
	item, err := resolver.assessReadItem(context.Background(), items[0], items[0].Code.Commit, source.revision)
	if err != nil || item.State != "applied" || owner.ReadCount() != contextprojection.MaxCodeEvidenceReads {
		test.Fatalf("item reused the template verification budget: %+v %v", item, err)
	}
	if err := evidence.ExhaustTraversal(); err != nil || !evidence.TraversalLimited() {
		test.Fatal("compatibility traversal flag", err)
	}
	config.RequireComplete = true
	strict := newLiveTestQueries(test, config)
	strictEvidence, err := strict.newLiveEvidence(context.Background(), source.repo, nil)
	if err != nil {
		test.Fatal(err)
	}
	if err := strictEvidence.ExhaustTraversal(); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
		test.Fatal("strict traversal silently degraded", err)
	}
	malformed := append([]domain.HistoryEvent(nil), source.history...)
	malformed[0].RepoID = string(domain.HashContent([]byte("foreign")))
	if _, err := newMemoryIntegration(queries, evidence, source.repo, malformed); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("foreign publication accepted", err)
	}
	missing, err := newMemoryIntegration(queries, evidence, source.repo, nil)
	if err != nil {
		test.Fatal(err)
	}
	item, err = missing.assess(context.Background(), items[0], items[0].Code.Commit, source.revision)
	if err != nil || item.Reason != "source_publication_missing" {
		test.Fatalf("unpublished claim gained applicability: %+v %v", item, err)
	}
}

func TestLiveAssessmentUsesSuppliedGitOwnerAndLazyCompatibility(test *testing.T) {
	source, config, request := liveFixture(test)
	liveCodeFixture(test, source, &request)
	source.scoped = false
	queries := newLiveTestQueries(test, config)
	lazy, err := queries.newLiveEvidence(context.Background(), source.repo, nil)
	if err != nil || lazy.git != nil || lazy.Limited() || lazy.TraversalLimited() {
		test.Fatal("metadata-only evidence construction was not lazy", err)
	}
	owner, err := evidencequery.NewGitEvidence(evidencequery.GitEvidenceConfig{RepoID: source.repo, Origin: source.origin, Trees: source, Deltas: source})
	if err != nil {
		test.Fatal(err)
	}
	assessment, err := queries.NewAssessment(context.Background(), source.repo, source.history, owner)
	if err != nil || assessment.evidence.git != owner {
		test.Fatal("assessment replaced the supplied Git evidence owner", err)
	}
	items, err := EffectiveItems(source.digest, "claims")
	if err != nil {
		test.Fatal(err)
	}
	for index := 0; index < contextprojection.MaxCodeEvidenceReads; index++ {
		if err := owner.ConsumeRead(context.Background(), source.repo, source.origin); err != nil {
			test.Fatal(err)
		}
	}
	item, err := assessment.Assess(context.Background(), items[0], items[0].Code.Commit, source.revision)
	if err != nil || item.Reason != "evidence_budget_exhausted" || !owner.Limited() {
		test.Fatalf("assessment did not share caller exhaustion: %+v %v", item, err)
	}
	page := domain.EffectiveMemoryPage{Selection: request.Selection, Total: len(items), Items: []domain.EffectiveMemoryItem{}}
	if proof, err := assessment.DeliveryHash(context.Background(), page, items); err != nil || proof != "" {
		test.Fatal("partial evidence acquired a delivery proof", proof, err)
	}
	resolved, err := assessment.ResolvePage(context.Background(), page, items, 0, 1)
	if err != nil || len(resolved.Items) != 1 || resolved.Items[0].Reason != item.Reason {
		test.Fatalf("exported page calculation diverged: %+v %v", resolved, err)
	}
	config.Trees, config.Deltas = nil, nil
	withoutReaders := newLiveTestQueries(test, config)
	evidence, err := withoutReaders.newLiveEvidence(context.Background(), source.repo, nil)
	if err != nil || evidence.git != nil {
		test.Fatal("absent Git readers failed metadata construction", err)
	}
	result, err := evidence.query(context.Background(), domain.CodeSelection{CodeCommit: request.Selection.CodeCommit, SourceCommit: items[0].Code.Commit, Paths: []string{"main.go"}}, source.revision)
	if err != nil || result.Reason != "git_evidence_unavailable" || evidence.Limited() {
		test.Fatalf("no-reader compatibility changed: %+v %v", result, err)
	}
}

func TestLiveEffectiveItemIdentityAndPromptContracts(test *testing.T) {
	snapshot := domain.HashContent([]byte("source"))
	claim := domain.MemoryClaim{Kind: "decision", Text: "keep the original contract"}
	digest := domain.MemoryDigest{SnapshotID: snapshot, ClaimsVersion: 1,
		Fragments: []domain.MemoryFragment{
			{SourceSnapshot: snapshot, Summary: strings.Repeat("older ", 200), Claims: []domain.MemoryClaim{claim, claim}},
			{SourceSnapshot: snapshot, Summary: strings.Repeat("최신 ", 400)},
		}}
	claims, err := EffectiveItems(digest, "claims")
	if err != nil || len(claims) != 1 {
		test.Fatal("claim identity/deduplication changed", claims, err)
	}
	expected := domain.EffectiveMemoryItem{SourceSnapshot: snapshot, Kind: claim.Kind, Text: claim.Text,
		MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "retained", Reason: "historical_knowledge"}}
	raw, err := json.Marshal(expected)
	if err != nil || claims[0].ID != domain.HashContent(raw) {
		test.Fatal("item identity no longer hashes the original unassessed wire item", err)
	}
	prompt, err := EffectiveItems(digest, "prompt")
	if err != nil || len(prompt) != 3 || prompt[0].ID != claims[0].ID || !strings.HasPrefix(prompt[1].Text, "[excerpt; full source through MCP]\n") {
		test.Fatalf("prompt order/excerpt contract: %+v %v", prompt, err)
	}
	if prompt[1].TextHash != domain.HashContent([]byte(strings.TrimSpace(digest.Fragments[1].Summary))) || prompt[1].State != "review" {
		test.Fatal("prompt excerpt lost full-text identity or legacy assessment")
	}
	digest.Fragments[1].Summary = string([]byte{255})
	if _, err := EffectiveItems(digest, ""); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("malformed historical UTF-8 acquired a wire identity", err)
	}
}
