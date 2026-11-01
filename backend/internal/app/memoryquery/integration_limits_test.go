package memoryquery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryIntegrationInputs struct {
	repo domain.ContentHash
	view domain.RepositoryView
}

var _ outbound.MemoryMetadataReader = (*memoryIntegrationInputs)(nil)

func (reader *memoryIntegrationInputs) check(ctx context.Context, repo domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo != reader.repo {
		return domain.ErrIntegrity
	}
	return nil
}

func (reader *memoryIntegrationInputs) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if err := reader.check(ctx, repo); err != nil {
		return nil, err
	}
	var snapshots []domain.Snapshot
	for _, snapshot := range reader.view.Snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if branch == "" || snapshot.Branch == branch {
			snapshots = append(snapshots, snapshot.Clone())
		}
	}
	return snapshots, ctx.Err()
}

func (reader *memoryIntegrationInputs) GetSnapshot(ctx context.Context, repo, identity domain.ContentHash) (domain.Snapshot, error) {
	if err := reader.check(ctx, repo); err != nil {
		return domain.Snapshot{}, err
	}
	for _, snapshot := range reader.view.Snapshots {
		if snapshot.ID == identity {
			return snapshot.Clone(), ctx.Err()
		}
	}
	return domain.Snapshot{}, domain.ErrNotFound
}

func (reader *memoryIntegrationInputs) GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error) {
	panic("snapshot-only fixture queried a ref")
}

func memoryIntegrationEvidence(test *testing.T, config evidencequery.GitEvidenceConfig) *liveEvidence {
	test.Helper()
	owner, err := evidencequery.NewGitEvidence(config)
	if err != nil {
		test.Fatal(err)
	}
	return &liveEvidence{repo: config.RepoID, origin: config.Origin, trees: config.Trees, deltas: config.Deltas,
		requireComplete: config.RequireComplete, git: owner}
}

func memoryIntegrationPublication(observation domain.HistoryEvent) domain.HistoryEvent {
	key := sha256.Sum256([]byte(observation.ID + ":publish"))
	return domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: observation.RepoID,
		BranchID: observation.BranchID, Branch: observation.Branch, LocalBranch: observation.LocalBranch,
		WorktreeID: observation.WorktreeID, Kind: "publish", Source: observation.Target, Target: observation.Target,
		GitAfter: observation.GitAfter, MemoryPinned: true, CreatedAt: observation.CreatedAt.Add(time.Second)}
}

func memoryReadReceiptLimitFixture(test *testing.T, strict bool) (*memoryIntegration, domain.ContentHash, string, string) {
	test.Helper()
	repo := domain.HashContent([]byte("receipt-limit-repository"))
	source := domain.HashContent([]byte("receipt-limit-source"))
	sourceCode, selected := strings.Repeat("1", 40), strings.Repeat("2", 40)
	sourceBranch, destinationBranch := strings.Repeat("a", 32), strings.Repeat("b", 32)
	observation := domain.HistoryEvent{
		ID: strings.Repeat("c", 32), RepoID: string(repo), BranchID: sourceBranch, Branch: "feature",
		Kind: "position", Target: source, GitAfter: sourceCode, CreatedAt: time.Unix(1, 0).UTC(),
	}
	history := []domain.HistoryEvent{observation, memoryIntegrationPublication(observation)}
	snapshots := []domain.Snapshot{{ID: source, RepoID: repo, DocHash: source}}
	for ordinal := range 129 {
		tip := domain.HashContent(fmt.Appendf(nil, "receipt-limit-tip-%d", ordinal))
		snapshot := domain.Snapshot{ID: tip, RepoID: repo, DocHash: tip}
		if ordinal == 128 {
			snapshot.Parents = []domain.ContentHash{source}
		}
		snapshots = append(snapshots, snapshot)
		receipt := domain.HistoryEvent{
			ID: fmt.Sprintf("%032x", ordinal+1), RepoID: string(repo), BranchID: destinationBranch, Branch: "main",
			Kind: "pr-merge", SourceBranchID: sourceBranch, Source: tip, Target: tip, CreatedAt: time.Unix(int64(ordinal+10), 0).UTC(),
			PR: &domain.PullRequestMerge{Number: ordinal + 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: sourceCode, MergeSHA: selected},
		}
		completion := receipt
		digest := sha256.Sum256([]byte(receipt.ID + ":completed"))
		completion.ID, completion.PRCompleted = fmt.Sprintf("%x", digest[:16]), true
		history = append(history, receipt, completion)
	}
	for _, event := range history {
		if err := domain.ValidateHistoryEvent(event); err != nil {
			test.Fatal("invalid receipt fixture", err)
		}
	}
	reader := &memoryIntegrationInputs{repo: repo, view: domain.RepositoryView{Snapshots: snapshots, History: history}}
	ledger := &memoryAssessmentEvidence{repo: repo}
	contributions, err := ledger.queries()
	if err != nil {
		test.Fatal(err)
	}
	queries := &LiveQueries{metadata: reader, evidence: contributions}
	evidence := memoryIntegrationEvidence(test, evidencequery.GitEvidenceConfig{RepoID: repo, RequireComplete: strict})
	resolver, err := newMemoryIntegration(queries, evidence, repo, history)
	if err != nil {
		test.Fatal(err)
	}
	if len(resolver.receipts[sourceBranch]) != 129 || len(resolver.publications[[2]string{string(source), sourceCode}]) != 1 {
		test.Fatal("fixture did not index all receipts and their source publication")
	}
	return resolver, source, sourceCode, selected
}

func TestMemoryReadReceiptTraversalLimit(test *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, scenario := range []string{"proof-at-129", "proof-at-128", "128-unrelated"} {
			test.Run(fmt.Sprintf("strict=%t/%s", strict, scenario), func(test *testing.T) {
				resolver, source, sourceCode, selected := memoryReadReceiptLimitFixture(test, strict)
				publications := resolver.publications[[2]string{string(source), sourceCode}]
				branch := publications[0].BranchID
				receipts := resolver.receipts[branch]
				digest := sha256.Sum256([]byte(receipts[128].ID + ":completed"))
				expectedReceipt := fmt.Sprintf("%x", digest[:16])
				switch scenario {
				case "proof-at-128":
					receipts[127], receipts[128] = receipts[128], receipts[127]
				case "128-unrelated":
					resolver.receipts[branch] = receipts[:128]
				}
				state, receipt, err := resolver.proveIntegration(context.Background(), source, sourceCode, selected, publications, "not_ancestor")
				switch {
				case scenario == "proof-at-129" && strict:
					if !errors.Is(err, domain.ErrMemoryProjectionLimit) || state != "" || receipt != "" {
						test.Fatal("strict resolver returned a truncated result", state, receipt, err)
					}
				case scenario == "proof-at-129":
					if err != nil || state != "unknown" || receipt != "" {
						test.Fatal("legacy receipt limit changed behavior", state, receipt, err)
					}
				case scenario == "proof-at-128":
					if err != nil || state != "verified" || receipt != expectedReceipt {
						test.Fatal("in-budget receipt failed to prove source integration", state, receipt, err)
					}
				case scenario == "128-unrelated":
					if err != nil || state != "absent" || receipt != "" {
						test.Fatal("complete negative result was treated as exhaustion", state, receipt, err)
					}
				}
				if resolver.evidence.TraversalLimited() != (scenario == "proof-at-129") || resolver.evidence.Limited() {
					test.Fatal("receipt cap was confused with missing facts or read exhaustion")
				}
				if scenario == "proof-at-129" && len(resolver.natural) != 128 {
					test.Fatal("fixture did not exhaust the first 128 unrelated natural closures", len(resolver.natural))
				}
			})
		}
	}
}

func TestMemoryReadNaturalTraversalLimit(test *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, scenario := range []string{"exact-limit", "over-limit", "missing-fact", "canceled"} {
			test.Run(fmt.Sprintf("strict=%t/%s", strict, scenario), func(test *testing.T) {
				repo := domain.HashContent([]byte("natural-limit-repository"))
				source := domain.HashContent([]byte("natural-limit-source"))
				snapshots := []domain.Snapshot{{ID: source, RepoID: repo, DocHash: source}}
				depth := MaxProjectionSnapshots
				if scenario == "over-limit" {
					depth++
				}
				tip := source
				for ordinal := range depth {
					identity := domain.HashContent(fmt.Appendf(nil, "natural-limit-%d", ordinal))
					snapshots = append(snapshots, domain.Snapshot{ID: identity, RepoID: repo, DocHash: identity, Parents: []domain.ContentHash{tip}})
					tip = identity
				}
				if scenario == "missing-fact" {
					snapshots = snapshots[:1]
				}
				reader := &memoryIntegrationInputs{repo: repo, view: domain.RepositoryView{Snapshots: snapshots}}
				queries := &LiveQueries{metadata: reader}
				evidence := memoryIntegrationEvidence(test, evidencequery.GitEvidenceConfig{RepoID: repo, RequireComplete: strict})
				resolver, err := newMemoryIntegration(queries, evidence, repo, nil)
				if err != nil {
					test.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if scenario == "canceled" {
					cancel()
				}
				state, err := resolver.naturalRelation(ctx, source, tip)
				switch {
				case scenario == "canceled":
					if !errors.Is(err, context.Canceled) || state != "" {
						test.Fatal("natural traversal lost cancellation", state, err)
					}
				case scenario == "over-limit" && strict:
					if !errors.Is(err, domain.ErrMemoryProjectionLimit) || state != "" {
						test.Fatal("strict natural traversal returned a partial result", state, err)
					}
				case scenario == "over-limit" || scenario == "missing-fact":
					if err != nil || state != "unknown" {
						test.Fatal("legacy traversal or observed missing fact changed behavior", state, err)
					}
				default:
					if err != nil || state != "ancestor" {
						test.Fatal("exact-bound natural proof rejected", state, err)
					}
				}
				if resolver.evidence.TraversalLimited() != (scenario == "over-limit") || resolver.evidence.Limited() {
					test.Fatal("natural traversal cap was confused with missing facts or read exhaustion")
				}
			})
		}
	}
}
