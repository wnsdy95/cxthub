package app

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryIntegration struct {
	service      *Service
	evidence     *codeEvidence
	repo         domain.ContentHash
	publications map[[2]string][]domain.HistoryEvent
	receipts     map[string][]domain.HistoryEvent
	snapshots    map[domain.ContentHash]domain.Snapshot
	natural      map[domain.ContentHash]*memoryNaturalClosure
}
type memoryNaturalClosure struct {
	seen, expanded map[domain.ContentHash]bool
	todo           []domain.ContentHash
	incomplete     bool
}

func newMemoryIntegration(s *Service, evidence *codeEvidence, repo domain.ContentHash, history []domain.HistoryEvent) (*memoryIntegration, error) {
	r := &memoryIntegration{service: s, evidence: evidence, repo: repo, publications: map[[2]string][]domain.HistoryEvent{}, receipts: map[string][]domain.HistoryEvent{}, natural: map[domain.ContentHash]*memoryNaturalClosure{}}
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
					r.publications[key] = append(r.publications[key], event)
					break
				}
			}
		}
		if event.Kind == "pr-merge" && !event.PRCompleted && event.PR != nil {
			key := sha256.Sum256([]byte(event.ID + ":completed"))
			completion := byID[fmt.Sprintf("%x", key[:16])]
			complete, err := hasPRCompletion([]domain.HistoryEvent{completion}, event)
			if err != nil {
				return nil, err
			}
			if complete {
				r.receipts[event.SourceBranchID] = append(r.receipts[event.SourceBranchID], event)
			}
		}
	}
	return r, nil
}
func (r *memoryIntegration) assess(ctx context.Context, item domain.EffectiveMemoryItem, code string, revision domain.RepositoryRevision) (domain.EffectiveMemoryItem, error) {
	claim := domain.MemoryClaim{Kind: item.Kind, Text: item.Text, Code: item.Code}
	if claim.Code == nil {
		return item, domain.ErrIntegrity
	}
	pubs := r.publications[[2]string{string(item.SourceSnapshot), claim.Code.Commit}]
	if len(pubs) == 0 {
		item.MemoryClaimAssessment = domain.MemoryClaimAssessment{State: "review", Reason: "source_publication_missing"}
		return item, nil
	}
	for _, p := range pubs {
		item.PublicationIDs = append(item.PublicationIDs, p.ID)
	}
	view, err := r.evidence.query(ctx, domain.CodeSelection{CodeCommit: code, SourceCommit: claim.Code.Commit, SourceParent: claim.Code.Parent, Paths: claim.Code.Paths}, revision)
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
		integration, receipt, err = r.proveIntegration(ctx, item.SourceSnapshot, claim.Code.Commit, code, pubs, view.Relation)
		if err != nil {
			return item, err
		}
	}
	item.Paths = view.Paths
	item.IntegrationReceipt = receipt
	if integration == "verified" {
		// A completed squash anchor can prove integration even while unrelated
		// missing Git ancestors leave direct source ancestry unknown.
		for i, p := range item.Paths {
			if p.State == "unknown" && p.Reason == "incomplete_ancestry" {
				item.Paths[i] = domain.AssessCodePath(domain.GitPathChange{Path: p.Path, Before: p.Before, After: p.After}, p.Selected, "ancestor")
			}
		}
	}
	item.MemoryClaimAssessment = domain.AssessMemoryClaim(claim, item.Paths, integration)
	if r.evidence.limited && item.State == "review" {
		item.Reason = "evidence_budget_exhausted"
	}
	return item, nil
}
func (r *memoryIntegration) proveIntegration(ctx context.Context, source domain.ContentHash, sourceCode, selected string, pubs []domain.HistoryEvent, direct string) (string, string, error) {
	unknown := direct == "unknown"
	seen := map[string]bool{}
	for _, p := range pubs {
		for _, receipt := range r.receipts[p.BranchID] {
			if seen[receipt.ID] {
				continue
			}
			seen[receipt.ID] = true
			if len(seen) > 128 {
				return "unknown", "", nil
			}
			// Immutable natural ancestry proves earlier context contributions. Mutable
			// grafts and current lane placement cannot supply historical merge proof.
			contextRelation, err := r.naturalRelation(ctx, source, receipt.Source)
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
			headRelation, err := r.evidence.relation(ctx, sourceCode, receipt.PR.HeadSHA)
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
			mergeRelation, err := r.evidence.relation(ctx, receipt.PR.MergeSHA, selected)
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
func (r *memoryIntegration) naturalRelation(ctx context.Context, source, tip domain.ContentHash) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source == tip {
		return "ancestor", nil
	}
	if r.snapshots == nil {
		rows, err := r.service.meta.ListSnapshots(ctx, r.repo, "")
		if err != nil {
			return "", err
		}
		r.snapshots = map[domain.ContentHash]domain.Snapshot{}
		for _, s := range rows {
			if s.RepoID != r.repo {
				return "", domain.ErrIntegrity
			}
			r.snapshots[s.ID] = s
		}
	}
	closure := r.natural[tip]
	if closure == nil {
		closure = &memoryNaturalClosure{seen: map[domain.ContentHash]bool{tip: true}, expanded: map[domain.ContentHash]bool{}, todo: []domain.ContentHash{tip}}
		r.natural[tip] = closure
	}
	if closure.seen[source] {
		return "ancestor", nil
	}
	for len(closure.todo) > 0 && len(closure.expanded) < maxProjectionSnapshots {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id := closure.todo[len(closure.todo)-1]
		closure.todo = closure.todo[:len(closure.todo)-1]
		if closure.expanded[id] {
			continue
		}
		closure.expanded[id] = true
		s, ok := r.snapshots[id]
		if !ok {
			closure.incomplete = true
			continue
		}
		for _, p := range s.Parents {
			closure.seen[p] = true
			closure.todo = append(closure.todo, p)
		}
		if closure.seen[source] {
			return "ancestor", nil
		}
	}
	if closure.incomplete || len(closure.todo) > 0 {
		return "unknown", nil
	}
	return "not_ancestor", nil
}
