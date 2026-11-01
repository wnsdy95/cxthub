package memoryquery

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryIntegration struct {
	queries      *LiveQueries
	evidence     *liveEvidence
	repo         domain.ContentHash
	publications map[[2]string][]domain.HistoryEvent
	receipts     map[string][]domain.HistoryEvent
	snapshots    map[domain.ContentHash]domain.Snapshot
	natural      map[domain.ContentHash]*memoryNaturalClosure
	history      []domain.HistoryEvent
}
type memoryNaturalClosure struct {
	seen, expanded map[domain.ContentHash]bool
	todo           []domain.ContentHash
	incomplete     bool
}

type Assessment = memoryIntegration

func (queries *LiveQueries) NewAssessment(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, git *evidencequery.GitEvidence) (*Assessment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if domain.ValidateContentHash(repo) != nil {
		return nil, domain.ErrValidation
	}
	if queries == nil || git == nil {
		return nil, domain.ErrIntegrity
	}
	evidence := &liveEvidence{repo: repo, origin: git.Origin(), git: git}
	return newMemoryIntegration(queries, evidence, repo, history)
}

func (resolver *memoryIntegration) Assess(ctx context.Context, item domain.EffectiveMemoryItem, code string, revision domain.RepositoryRevision) (domain.EffectiveMemoryItem, error) {
	return resolver.assess(ctx, item, code, revision)
}

func (resolver *memoryIntegration) ResolvePage(ctx context.Context, out domain.EffectiveMemoryPage, items []domain.EffectiveMemoryItem, offset, limit int) (domain.EffectiveMemoryPage, error) {
	return resolveEffectiveMemoryPage(ctx, resolver, out, items, offset, limit)
}

func (resolver *memoryIntegration) DeliveryHash(ctx context.Context, out domain.EffectiveMemoryPage, items []domain.EffectiveMemoryItem) (domain.ContentHash, error) {
	return effectiveMemoryDeliveryHash(ctx, resolver, out, items)
}

func newMemoryIntegration(queries *LiveQueries, evidence *liveEvidence, repo domain.ContentHash, history []domain.HistoryEvent) (*memoryIntegration, error) {
	if queries == nil || evidence == nil || evidence.repo != repo {
		return nil, domain.ErrIntegrity
	}
	if evidence.contributions == nil {
		evidence.contributions = &liveContributionCache{repo: repo}
	}
	if err := evidence.contributions.bind(repo, history); err != nil {
		return nil, err
	}
	resolver := &memoryIntegration{queries: queries, evidence: evidence, repo: repo, publications: map[[2]string][]domain.HistoryEvent{}, receipts: map[string][]domain.HistoryEvent{}, natural: map[domain.ContentHash]*memoryNaturalClosure{}, history: history}
	observations := map[[2]string][]domain.HistoryEvent{}
	byID := map[string]domain.HistoryEvent{}
	for _, event := range history {
		if event.RepoID != string(repo) {
			return nil, domain.ErrIntegrity
		}
		byID[event.ID] = event
		if event.Kind != "publish" && event.Kind != "pr-merge" {
			key := [2]string{string(event.Target), event.GitAfter}
			observations[key] = append(observations[key], event)
		}
	}
	for _, event := range history {
		if event.Kind == "publish" && event.Source == event.Target && event.Source != "" && event.GitAfter != "" {
			key := [2]string{string(event.Target), event.GitAfter}
			for _, observation := range observations[key] {
				if domain.IsPublicationProof(event, observation) {
					resolver.publications[key] = append(resolver.publications[key], event)
					break
				}
			}
		}
		if event.Kind == "pr-merge" && !event.PRCompleted && event.PR != nil {
			key := sha256.Sum256([]byte(event.ID + ":completed"))
			completion := byID[fmt.Sprintf("%x", key[:16])]
			complete, err := domain.HasPRCompletion([]domain.HistoryEvent{completion}, event)
			if err != nil {
				return nil, err
			}
			if complete {
				resolver.receipts[event.SourceBranchID] = append(resolver.receipts[event.SourceBranchID], event)
			}
		}
	}
	return resolver, nil
}
func (resolver *memoryIntegration) assess(ctx context.Context, item domain.EffectiveMemoryItem, code string, revision domain.RepositoryRevision) (domain.EffectiveMemoryItem, error) {
	claim := domain.MemoryClaim{Kind: item.Kind, Text: item.Text, Code: item.Code}
	if claim.Code == nil {
		return item, domain.ErrIntegrity
	}
	pubs := resolver.publications[[2]string{string(item.SourceSnapshot), claim.Code.Commit}]
	if len(pubs) == 0 {
		item.MemoryClaimAssessment = domain.MemoryClaimAssessment{State: "review", Reason: "source_publication_missing"}
		return item, nil
	}
	for _, candidate := range pubs {
		item.PublicationIDs = append(item.PublicationIDs, candidate.ID)
	}
	view, err := resolver.evidence.query(ctx, domain.CodeSelection{CodeCommit: code, SourceCommit: claim.Code.Commit, SourceParent: claim.Code.Parent, Paths: claim.Code.Paths}, revision)
	if isMemoryScopeError(err) {
		item.MemoryClaimAssessment = domain.MemoryClaimAssessment{State: "review", Reason: "declared_scope_invalid"}
		return item, nil
	}
	if err != nil {
		return item, err
	}
	integration, receipt := "absent", ""
	if view.Relation == "ancestor" {
		integration = "verified"
	} else {
		integration, receipt, err = resolver.proveIntegration(ctx, item.SourceSnapshot, claim.Code.Commit, code, pubs, view.Relation)
		if err != nil {
			return item, err
		}
	}
	item.Paths = view.Paths
	item.IntegrationReceipt = receipt
	if integration == "verified" {
		// A completed squash anchor can prove integration even while unrelated
		// missing Git ancestors leave direct source ancestry unknown.
		for index, candidate := range item.Paths {
			if candidate.State == "unknown" && candidate.Reason == "incomplete_ancestry" {
				item.Paths[index] = domain.AssessCodePath(domain.GitPathChange{Path: candidate.Path, Before: candidate.Before, After: candidate.After}, candidate.Selected, "ancestor")
			}
		}
	}
	item.MemoryClaimAssessment = domain.AssessMemoryClaim(claim, item.Paths, integration)
	if resolver.evidence.Limited() && item.State == "review" {
		item.Reason = "evidence_budget_exhausted"
	}
	return item, nil
}
func (resolver *memoryIntegration) proveIntegration(ctx context.Context, source domain.ContentHash, sourceCode, selected string, pubs []domain.HistoryEvent, direct string) (string, string, error) {
	unknown := direct == "unknown"
	contributions, err := resolver.queries.contextContributions(ctx, resolver.repo, resolver.history, resolver.evidence)
	if err != nil {
		return "", "", err
	}
	for _, contribution := range contributions.records {
		if !contribution.Valid || contribution.Record.Capture != source || contribution.Record.OriginalHead != sourceCode {
			continue
		}
		matched := false
		for _, publication := range pubs {
			accepted, exists := contribution.Publications[publication.ID]
			if exists && accepted == publication && publication.BranchID == contribution.Record.SourceBranchID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		relation, err := resolver.evidence.Relation(ctx, contribution.Record.IntegrationSHA, selected)
		if err != nil {
			return "", "", err
		}
		if relation == "ancestor" {
			return "verified", contribution.Record.ID, nil
		}
		unknown = unknown || relation == "unknown"
	}
	seen := map[string]bool{}
	for _, candidate := range pubs {
		for _, receipt := range resolver.receipts[candidate.BranchID] {
			if seen[receipt.ID] {
				continue
			}
			seen[receipt.ID] = true
			if len(seen) > 128 {
				if err := resolver.evidence.ExhaustTraversal(); err != nil {
					return "", "", err
				}
				return "unknown", "", nil
			}
			// Immutable natural ancestry proves earlier context contributions. Mutable
			// grafts and current lane placement cannot supply historical merge proof.
			contextRelation, err := resolver.naturalRelation(ctx, source, receipt.Source)
			if err != nil {
				return "", "", err
			}
			if contextRelation == "not_ancestor" {
				continue
			}
			if contextRelation == "unknown" {
				unknown = true
				continue
			}
			headRelation, err := resolver.evidence.Relation(ctx, sourceCode, receipt.PR.HeadSHA)
			if err != nil {
				return "", "", err
			}
			if headRelation == "not_ancestor" {
				continue
			}
			if headRelation == "unknown" {
				unknown = true
				continue
			}
			mergeRelation, err := resolver.evidence.Relation(ctx, receipt.PR.MergeSHA, selected)
			if err != nil {
				return "", "", err
			}
			if mergeRelation == "ancestor" {
				key := sha256.Sum256([]byte(receipt.ID + ":completed"))
				return "verified", fmt.Sprintf("%x", key[:16]), nil
			}
			if mergeRelation == "unknown" {
				unknown = true
			}
		}
	}
	if unknown {
		return "unknown", "", nil
	}
	return "absent", "", nil
}
func (resolver *memoryIntegration) naturalRelation(ctx context.Context, source, tip domain.ContentHash) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source == tip {
		return "ancestor", nil
	}
	if resolver.snapshots == nil {
		rows, err := resolver.queries.metadata.ListSnapshots(ctx, resolver.repo, "")
		if err != nil {
			return "", err
		}
		resolver.snapshots = map[domain.ContentHash]domain.Snapshot{}
		for _, snapshot := range rows {
			if snapshot.RepoID != resolver.repo {
				return "", domain.ErrIntegrity
			}
			resolver.snapshots[snapshot.ID] = snapshot
		}
	}
	closure := resolver.natural[tip]
	if closure == nil {
		closure = &memoryNaturalClosure{seen: map[domain.ContentHash]bool{tip: true}, expanded: map[domain.ContentHash]bool{}, todo: []domain.ContentHash{tip}}
		resolver.natural[tip] = closure
	}
	if closure.seen[source] {
		return "ancestor", nil
	}
	for len(closure.todo) > 0 && len(closure.expanded) < MaxProjectionSnapshots {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id := closure.todo[len(closure.todo)-1]
		closure.todo = closure.todo[:len(closure.todo)-1]
		if closure.expanded[id] {
			continue
		}
		closure.expanded[id] = true
		snapshot, ok := resolver.snapshots[id]
		if !ok {
			closure.incomplete = true
			continue
		}
		for _, candidate := range snapshot.Parents {
			closure.seen[candidate] = true
			closure.todo = append(closure.todo, candidate)
		}
		if closure.seen[source] {
			return "ancestor", nil
		}
	}
	if closure.incomplete || len(closure.todo) > 0 {
		if len(closure.todo) > 0 {
			if err := resolver.evidence.ExhaustTraversal(); err != nil {
				return "", err
			}
		}
		return "unknown", nil
	}
	return "not_ancestor", nil
}

func (template *memoryIntegration) assessReadItem(ctx context.Context, item domain.EffectiveMemoryItem, code string, revision domain.RepositoryRevision) (domain.EffectiveMemoryItem, error) {
	evidence, err := template.queries.newLiveEvidence(ctx, template.repo, template.evidence.contributions)
	if err != nil {
		return domain.EffectiveMemoryItem{}, err
	}
	resolver := *template
	resolver.evidence = evidence
	resolver.natural = map[domain.ContentHash]*memoryNaturalClosure{}
	item, err = resolver.assess(ctx, item, code, revision)
	if err != nil {
		return domain.EffectiveMemoryItem{}, err
	}
	if evidence.Limited() || evidence.TraversalLimited() {
		return domain.EffectiveMemoryItem{}, domain.ErrMemoryProjectionLimit
	}
	if err := ctx.Err(); err != nil {
		return domain.EffectiveMemoryItem{}, err
	}
	template.snapshots = resolver.snapshots
	return item, nil
}
