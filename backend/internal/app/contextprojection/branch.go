package contextprojection

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type BranchPlan struct {
	result domain.BranchContext
	ref    domain.Ref
	code   string
	inputs *Inputs
	scopes map[string]bool
	order  map[string]int
}

func PrepareBranchContext(ctx context.Context, ref domain.Ref, code string, inputs *Inputs, evidence CodeEvidence) (*BranchPlan, error) {
	plan := &BranchPlan{
		result: domain.BranchContext{BranchID: ref.BranchID, SnapshotID: ref.Target, CodeCommit: code, Reason: "selected_code", Merges: []domain.BranchContextMerge{}, Roots: []domain.ContentHash{ref.Target}, SnapshotIDs: []domain.ContentHash{}},
		ref:    ref, code: code, inputs: inputs,
	}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if inputs.history.repo != ref.RepoID {
		return plan, domain.ErrIntegrity
	}
	if code != "" && domain.ValidateGitOID(code) != nil {
		return plan, domain.ErrValidation
	}
	if code == "" {
		plan.result.Reason = "code_position_unavailable"
	}
	byID := inputs.snapshots
	if _, exists := byID[ref.Target]; !exists {
		return plan, domain.ErrNotFound
	}
	scopes, err := InheritedIntegrationScopes(ctx, ref.BranchID, byID, inputs.history)
	if err != nil {
		return plan, err
	}
	plan.scopes = scopes
	order := map[string]int{}
	plan.order = order
	for identifier := code; identifier != "" && len(order) < MaxProjectionSnapshots; {
		if _, seen := order[identifier]; seen {
			return plan, fmt.Errorf("%w: cyclic Git evidence", domain.ErrIntegrity)
		}
		order[identifier] = len(order)
		commit, err := evidence.GetGitCommitTree(ctx, ref.RepoID, evidence.Origin(), identifier)
		if errors.Is(err, domain.ErrNotFound) {
			break
		}
		if err != nil {
			return plan, err
		}
		if len(commit.Parents) == 0 {
			break
		}
		identifier = commit.Parents[0]
		if len(order) == MaxProjectionSnapshots {
			if err := evidence.ExhaustTraversal(); err != nil {
				return plan, err
			}
		}
	}
	var receipts []int
	for scope := range scopes {
		receipts = append(receipts, inputs.history.receipts[scope]...)
	}
	sort.Ints(receipts)
	for _, position := range receipts {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		receipt := inputs.history.rows[position]
		completion, complete, err := inputs.history.Completion(receipt)
		if err != nil {
			return plan, err
		}
		if !complete {
			continue
		}
		plan.result.Merges = append(plan.result.Merges, domain.BranchContextMerge{DeliveryAcknowledged: true, EventID: completion.ID, DestinationBranchID: receipt.BranchID, Source: receipt.Source, MergeSHA: receipt.PR.MergeSHA, PRNumber: receipt.PR.Number, Before: completion.SharedTarget, State: "review", Reason: "git_evidence_pending", Order: -1})
	}
	return plan, nil
}

func (plan *BranchPlan) Result() domain.BranchContext {
	return plan.result
}

func (plan *BranchPlan) Finish(ctx context.Context, contributions Contributions, evidence CodeEvidence) (domain.BranchContext, error) {
	out := &plan.result
	ref, code, inputs := plan.ref, plan.code, plan.inputs
	byID, order := inputs.snapshots, plan.order
	AppendContributionMerges(out, plan.scopes, contributions)
	for index := range out.Merges {
		merge := &out.Merges[index]
		if merge.Reason == "participation_evidence_conflict" {
			continue
		}
		if _, exists := byID[merge.Source]; !exists {
			merge.Reason = "source_unavailable"
		} else if rank, included := order[merge.MergeSHA]; included {
			merge.State, merge.Reason, merge.Order = "included", "verified_git_order", rank
		} else if code != "" {
			relation, err := evidence.Relation(ctx, merge.MergeSHA, code)
			if err != nil {
				return *out, err
			}
			if relation == "not_ancestor" {
				merge.State, merge.Reason = "not_selected", "merge_not_in_selected_code"
			}
			if relation == "ancestor" {
				merge.Reason = "integration_order_unavailable"
			}
		}
	}
	sort.Slice(out.Merges, func(left, right int) bool {
		first, second := out.Merges[left], out.Merges[right]
		if first.Order != second.Order {
			return first.Order > second.Order
		}
		return first.EventID < second.EventID
	})
	allowed := map[domain.ContentHash]bool{}
	ranks := map[domain.ContentHash]int{}
	unselected := map[domain.ContentHash]bool{}
	for _, merge := range out.Merges {
		if merge.State == "included" {
			continue
		}
		stack := []domain.ContentHash{merge.Source}
		for len(stack) > 0 {
			identifier := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if unselected[identifier] {
				continue
			}
			unselected[identifier] = true
			if snapshot, exists := byID[identifier]; exists {
				stack = append(stack, snapshot.Parents...)
			}
		}
	}
	visit := func(root domain.ContentHash, natural bool, rank int) error {
		stack, seen := []domain.ContentHash{root}, map[domain.ContentHash]bool{}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			identifier := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[identifier] {
				continue
			}
			seen[identifier] = true
			snapshot, exists := byID[identifier]
			if !exists {
				return fmt.Errorf("%w: missing included context %s", domain.ErrIntegrity, identifier)
			}
			if !allowed[identifier] {
				ranks[identifier] = rank
			}
			allowed[identifier] = true
			if len(allowed) > MaxProjectionSnapshots {
				return fmt.Errorf("branch context exceeds %d snapshots", MaxProjectionSnapshots)
			}
			stack = append(stack, snapshot.Parents...)
			if !natural {
				for _, parent := range snapshot.GraftParents {
					if !unselected[parent] {
						stack = append(stack, parent)
					}
				}
			}
		}
		return nil
	}
	out.Roots = nil
	roots := map[domain.ContentHash]bool{}
	for _, merge := range out.Merges {
		if merge.State != "included" {
			continue
		}
		if merge.Before != "" {
			if err := visit(merge.Before, true, merge.Order+1); err != nil {
				return *out, err
			}
			if !roots[merge.Before] && merge.Before != ref.Target {
				out.Roots = append(out.Roots, merge.Before)
				roots[merge.Before] = true
			}
		}
		if err := visit(merge.Source, true, merge.Order); err != nil {
			return *out, err
		}
		if !roots[merge.Source] && merge.Source != ref.Target {
			out.Roots = append(out.Roots, merge.Source)
			roots[merge.Source] = true
		}
	}
	if err := visit(ref.Target, false, -1); err != nil {
		return *out, err
	}
	out.Roots = append(out.Roots, ref.Target)
	for _, position := range inputs.history.byBranch[ref.BranchID] {
		event := inputs.history.rows[position]
		if event.Kind != "publish" || event.Source != event.Target || !allowed[event.Target] {
			continue
		}
		if rank, exists := order[event.GitAfter]; exists && ranks[event.Target] < 0 {
			ranks[event.Target] = rank
		}
	}
	var err error
	out.SnapshotIDs, err = OrderedBranchSnapshots(byID, allowed, ranks)
	return *out, err
}

func InheritedIntegrationScopes(ctx context.Context, branch string, snapshots map[domain.ContentHash]domain.Snapshot, history *HistoryIndex) (map[string]bool, error) {
	scopes := map[string]bool{branch: true}
	queue := []string{branch}
	seen := map[domain.ContentHash]bool{}
	for len(queue) > 0 {
		currentBranch := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		stack := append([]domain.ContentHash{}, history.births[currentBranch]...)
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			identifier := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[identifier] {
				continue
			}
			seen[identifier] = true
			if len(seen) > MaxProjectionSnapshots {
				return nil, fmt.Errorf("branch inheritance exceeds %d snapshots", MaxProjectionSnapshots)
			}
			for _, scope := range history.publications[identifier] {
				if !scopes[scope] {
					scopes[scope] = true
					queue = append(queue, scope)
				}
			}
			if snapshot, exists := snapshots[identifier]; exists {
				stack = append(stack, snapshot.Parents...)
			}
		}
	}
	return scopes, nil
}

func OrderedBranchSnapshots(byID map[domain.ContentHash]domain.Snapshot, allowed map[domain.ContentHash]bool, ranks map[domain.ContentHash]int) ([]domain.ContentHash, error) {
	degree := map[domain.ContentHash]int{}
	for identifier := range allowed {
		for _, parent := range byID[identifier].ReachabilityParents() {
			if allowed[parent] {
				degree[parent]++
			}
		}
	}
	queue := &branchContextQueue{snapshots: byID, ranks: ranks}
	for identifier := range allowed {
		if degree[identifier] == 0 {
			heap.Push(queue, identifier)
		}
	}
	out := make([]domain.ContentHash, 0, len(allowed))
	for queue.Len() > 0 {
		identifier := heap.Pop(queue).(domain.ContentHash)
		out = append(out, identifier)
		for _, parent := range byID[identifier].ReachabilityParents() {
			if allowed[parent] {
				degree[parent]--
				if degree[parent] == 0 {
					heap.Push(queue, parent)
				}
			}
		}
	}
	if len(out) != len(allowed) {
		return nil, fmt.Errorf("%w: cyclic branch context", domain.ErrIntegrity)
	}
	return out, nil
}

type branchContextQueue struct {
	ids       []domain.ContentHash
	snapshots map[domain.ContentHash]domain.Snapshot
	ranks     map[domain.ContentHash]int
}

func (queue branchContextQueue) Len() int { return len(queue.ids) }

func (queue branchContextQueue) Less(left, right int) bool {
	first, second := queue.ids[left], queue.ids[right]
	if queue.ranks[first] != queue.ranks[second] {
		return queue.ranks[first] < queue.ranks[second]
	}
	if !queue.snapshots[first].CreatedAt.Equal(queue.snapshots[second].CreatedAt) {
		return queue.snapshots[first].CreatedAt.After(queue.snapshots[second].CreatedAt)
	}
	return first < second
}

func (queue branchContextQueue) Swap(left, right int) {
	queue.ids[left], queue.ids[right] = queue.ids[right], queue.ids[left]
}

func (queue *branchContextQueue) Push(value any) {
	queue.ids = append(queue.ids, value.(domain.ContentHash))
}

func (queue *branchContextQueue) Pop() any {
	index := len(queue.ids) - 1
	value := queue.ids[index]
	queue.ids = queue.ids[:index]
	return value
}
