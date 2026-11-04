package contextprojection

import (
	"context"
	"errors"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func BranchCode(ref domain.Ref, history []domain.HistoryEvent) string {
	codes := BranchCodeCandidates(ref, history)
	if len(codes) == 1 {
		for code := range codes {
			return code
		}
	}
	return ""
}

func BranchCodeCandidates(ref domain.Ref, history []domain.HistoryEvent) map[string]bool {
	codes := map[string]bool{}
	mergedSources := map[string]bool{}
	for _, event := range history {
		if event.BranchID != ref.BranchID || event.Target != ref.Target {
			continue
		}
		code := ""
		switch event.Kind {
		case "publish":
			if event.Source == event.Target {
				code = event.GitAfter
			}
		case "position", "advance", "birth", "attach":
			code = event.GitAfter
		case "pr-merge":
			if event.PRCompleted && event.PR != nil {
				code = event.PR.MergeSHA
				if event.Source == ref.Target && event.PR.HeadSHA != code && domain.ValidateGitOID(code) == nil {
					mergedSources[event.PR.HeadSHA] = true
				}
			}
		}
		if domain.ValidateGitOID(code) == nil {
			codes[code] = true
		}
	}
	for code := range mergedSources {
		delete(codes, code)
	}
	return codes
}

func ResolveBranchCode(ctx context.Context, ref domain.Ref, history []domain.HistoryEvent, evidence CodeEvidence) (string, error) {
	index, err := NewHistoryIndex(ctx, ref.RepoID, history)
	if err != nil {
		return "", err
	}
	return ResolveBranchCodeFromIndex(ctx, ref, index, evidence)
}

func ResolveBranchCodeFromIndex(ctx context.Context, ref domain.Ref, index *HistoryIndex, evidence CodeEvidence) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if index.repo != ref.RepoID {
		return "", domain.ErrIntegrity
	}
	if evidence == nil {
		return BranchCode(ref, index.TargetEvents(ref.BranchID, ref.Target)), nil
	}
	associations := map[domain.ContentHash]map[string]bool{}
	causalCompletions := map[string]bool{}
	visited, active := map[domain.ContentHash]bool{}, map[domain.ContentHash]bool{}
	var collect func(domain.ContentHash) error
	collect = func(target domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if active[target] {
			return errAmbiguousBranchCode
		}
		if visited[target] {
			return nil
		}
		if len(visited) >= MaxCodeEvidenceReads {
			if err := evidence.ExhaustTraversal(); err != nil {
				return err
			}
			return errAmbiguousBranchCode
		}
		visited[target], active[target] = true, true
		defer delete(active, target)
		selectedRef := ref
		selectedRef.Target = target
		rows := index.TargetEvents(ref.BranchID, target)
		codes := BranchCodeCandidates(selectedRef, rows)
		merges := map[string]bool{}
		for _, event := range rows {
			if event.Kind == "pr-merge" && event.PRCompleted && event.PR != nil && domain.ValidateGitOID(event.PR.MergeSHA) == nil {
				merges[event.PR.MergeSHA] = true
				causalCompletions[event.ID] = true
			}
		}
		associations[target] = codes
		if len(merges) == 0 {
			if target == ref.Target && len(codes) > 1 {
				return errAmbiguousBranchCode
			}
			return nil
		}
		for _, event := range rows {
			if event.Kind == "pr-merge" && event.PRCompleted && event.PR != nil && event.SharedTarget != "" && event.SharedTarget != target {
				if err := collect(event.SharedTarget); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(ref.Target); err != nil {
		if errors.Is(err, errAmbiguousBranchCode) {
			return "", nil
		}
		return "", err
	}
	for _, position := range index.receipts[ref.BranchID] {
		receipt := index.rows[position]
		if receipt.PR.HeadSHA == receipt.PR.MergeSHA {
			continue
		}
		if !causalCompletions[domain.PRCompletionID(receipt.ID)] || !associations[receipt.Source][receipt.PR.HeadSHA] {
			continue
		}
		_, complete, err := index.Completion(receipt)
		if err != nil {
			return "", err
		}
		if complete {
			delete(associations[receipt.Source], receipt.PR.HeadSHA)
		}
	}
	candidates := map[string]bool{}
	for _, codes := range associations {
		for code := range codes {
			candidates[code] = true
		}
	}
	codes := make([]string, 0, len(candidates))
	for code := range candidates {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if len(codes) == 0 {
		return "", nil
	}
	selected := codes[0]
	var ordinary []domain.HistoryEvent
	for _, event := range index.TargetEvents(ref.BranchID, ref.Target) {
		if event.Kind != "pr-merge" {
			ordinary = append(ordinary, event)
		}
	}
	ordinaryCodes := BranchCodeCandidates(ref, ordinary)
	for code := range ordinaryCodes {
		if !associations[ref.Target][code] {
			delete(ordinaryCodes, code)
		}
	}
	if len(ordinaryCodes) > 1 {
		return "", nil
	}
	if len(ordinaryCodes) == 1 {
		for code := range ordinaryCodes {
			selected = code
		}
	} else {
		for _, code := range codes[1:] {
			relation, err := evidence.Relation(ctx, selected, code)
			if err != nil {
				return "", err
			}
			if relation == "ancestor" {
				selected = code
			}
		}
	}
	for _, code := range codes {
		relation, err := evidence.Relation(ctx, code, selected)
		if err != nil {
			return "", err
		}
		if relation != "ancestor" {
			return "", nil
		}
	}
	return selected, nil
}

var errAmbiguousBranchCode = errors.New("ambiguous ordinary branch code observations")
