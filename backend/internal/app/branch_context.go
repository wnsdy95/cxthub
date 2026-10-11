package app

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// branchContext runs under the repository's pinned read. A completion proves
// an integration happened; verified Git ancestry determines its inclusion at
// the selected code. Neither mutable graft placement nor receipt time does.
func (s *Service) branchContext(ctx context.Context, ref domain.Ref, code string, snapshots []domain.Snapshot, history []domain.HistoryEvent, evidence *codeEvidence) (domain.BranchContext, error) {
	inputs, err := newBranchQueryInputs(ctx, ref.RepoID, snapshots, history)
	if err != nil {
		return domain.BranchContext{}, err
	}
	return s.branchContextFromInputs(ctx, ref, code, inputs, evidence)
}

func (s *Service) branchContextFromInputs(ctx context.Context, ref domain.Ref, code string, inputs *branchQueryInputs, evidence *codeEvidence) (domain.BranchContext, error) {
	out := domain.BranchContext{BranchID: ref.BranchID, SnapshotID: ref.Target, CodeCommit: code, Reason: "selected_code", Merges: []domain.BranchContextMerge{}, Roots: []domain.ContentHash{ref.Target}, SnapshotIDs: []domain.ContentHash{}}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if inputs.history.repo != ref.RepoID {
		return out, domain.ErrIntegrity
	}
	if code != "" && domain.ValidateGitOID(code) != nil {
		return out, domain.ErrValidation
	}
	if code == "" {
		out.Reason = "code_position_unavailable"
	}
	byID := inputs.snapshots
	if _, ok := byID[ref.Target]; !ok {
		return out, domain.ErrNotFound
	}
	scopes, err := inheritedIntegrationScopes(ctx, ref.BranchID, byID, inputs.history)
	if err != nil {
		return out, err
	}
	// One linear walk establishes Git integration order, regardless of delayed
	// promotion jobs and reversed delivery order. Missing evidence is explicit.
	order := map[string]int{}
	for id := code; id != "" && len(order) < maxProjectionSnapshots; {
		if _, seen := order[id]; seen {
			return out, fmt.Errorf("%w: cyclic Git evidence", domain.ErrIntegrity)
		}
		order[id] = len(order)
		commit, err := evidence.GetGitCommitTree(ctx, ref.RepoID, evidence.origin, id)
		if errors.Is(err, domain.ErrNotFound) {
			break
		}
		if err != nil {
			return out, err
		}
		if len(commit.Parents) == 0 {
			break
		}
		id = commit.Parents[0]
	}
	var receipts []int
	for scope := range scopes {
		receipts = append(receipts, inputs.history.receipts[scope]...)
	}
	sort.Ints(receipts)
	for _, position := range receipts {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		receipt := inputs.history.rows[position]
		completion, complete, err := inputs.history.completion(receipt)
		if err != nil {
			return out, err
		}
		if !complete {
			continue
		}
		out.Merges = append(out.Merges, domain.BranchContextMerge{DeliveryAcknowledged: true, EventID: completion.ID, DestinationBranchID: receipt.BranchID, Source: receipt.Source, MergeSHA: receipt.PR.MergeSHA, PRNumber: receipt.PR.Number, Before: completion.SharedTarget, State: "review", Reason: "git_evidence_pending", Order: -1})
	}
	contributions, err := s.contextContributionsForEvidence(ctx, ref.RepoID, inputs.history.rows, evidence)
	if err != nil {
		return out, err
	}
	appendContributionMerges(&out, scopes, contributions)
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
			relation, err := evidence.relation(ctx, merge.MergeSHA, code)
			if err != nil {
				return out, err
			}
			if relation == "not_ancestor" {
				merge.State, merge.Reason = "not_selected", "merge_not_in_selected_code"
			}
			if relation == "ancestor" {
				merge.Reason = "integration_order_unavailable"
			}
		}
	}
	sort.Slice(out.Merges, func(i, j int) bool {
		a, b := out.Merges[i], out.Merges[j]
		if a.Order != b.Order {
			return a.Order > b.Order
		} // oldest first
		return a.EventID < b.EventID
	})
	// Preserve the selected conversation closure. Supplement completed PRs using
	// immutable natural ancestry only: a source's later mutable graft must not
	// pull an unrelated/future PR into a past code selection.
	allowed := map[domain.ContentHash]bool{}
	ranks := map[domain.ContentHash]int{}
	// An overlay is a preservation link, not proof that a PR is applied at this
	// code position. Prevent known unselected/review sources from re-entering via
	// that link after their integration receipt has already excluded them. Shared
	// natural ancestors are still admitted by the selected roots themselves.
	unselected := map[domain.ContentHash]bool{}
	for _, m := range out.Merges {
		if m.State == "included" {
			continue
		}
		stack := []domain.ContentHash{m.Source}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if unselected[id] {
				continue
			}
			unselected[id] = true
			if snap, ok := byID[id]; ok {
				stack = append(stack, snap.Parents...)
			}
		}
	}
	visit := func(root domain.ContentHash, natural bool, rank int) error {
		stack, seen := []domain.ContentHash{root}, map[domain.ContentHash]bool{}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			snap, ok := byID[id]
			if !ok {
				return fmt.Errorf("%w: missing included context %s", domain.ErrIntegrity, id)
			}
			if !allowed[id] {
				ranks[id] = rank
			}
			allowed[id] = true
			if len(allowed) > maxProjectionSnapshots {
				return fmt.Errorf("branch context exceeds %d snapshots", maxProjectionSnapshots)
			}
			stack = append(stack, snap.Parents...)
			if !natural {
				for _, parent := range snap.GraftParents {
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
	for _, m := range out.Merges {
		if m.State != "included" {
			continue
		}
		// A merge also preserves the destination's pre-merge knowledge. Read its
		// immutable ancestry; later graft placements are not historical proof.
		if m.Before != "" {
			if err := visit(m.Before, true, m.Order+1); err != nil {
				return out, err
			}
			if !roots[m.Before] && m.Before != ref.Target {
				out.Roots = append(out.Roots, m.Before)
				roots[m.Before] = true
			}
		}
		if err := visit(m.Source, true, m.Order); err != nil {
			return out, err
		}
		if !roots[m.Source] && m.Source != ref.Target {
			out.Roots = append(out.Roots, m.Source)
			roots[m.Source] = true
		}
	}
	if err := visit(ref.Target, false, -1); err != nil {
		return out, err
	}
	out.Roots = append(out.Roots, ref.Target)
	// Exact branch publications can order ordinary captures between PR merges.
	// Conflicting same-snapshot associations stay at their oldest introduction.
	for _, position := range inputs.history.byBranch[ref.BranchID] {
		h := inputs.history.rows[position]
		if h.Kind != "publish" || h.Source != h.Target || !allowed[h.Target] {
			continue
		}
		if n, ok := order[h.GitAfter]; ok && ranks[h.Target] < 0 {
			ranks[h.Target] = n
		}
	}
	out.SnapshotIDs, err = orderedBranchSnapshots(byID, allowed, ranks)
	return out, err
}

// A new branch inherits its recorded source's project knowledge. Follow only
// birth sources and accepted publications in natural ancestry, never names,
// timestamps, mutable grafts or a reused binding's name-parent. Inclusion is
// still checked against the selected Git code below. Orphans have no birth
// source and do not inherit conversation membership through this path.
func inheritedIntegrationScopes(ctx context.Context, branch string, snapshots map[domain.ContentHash]domain.Snapshot, history *branchHistoryIndex) (map[string]bool, error) {
	scopes := map[string]bool{branch: true}
	queue := []string{branch}
	seen := map[domain.ContentHash]bool{}
	for len(queue) > 0 {
		b := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		stack := append([]domain.ContentHash{}, history.births[b]...)
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			if len(seen) > maxProjectionSnapshots {
				return nil, fmt.Errorf("branch inheritance exceeds %d snapshots", maxProjectionSnapshots)
			}
			for _, scope := range history.publications[id] {
				if !scopes[scope] {
					scopes[scope] = true
					queue = append(queue, scope)
				}
			}
			if s, ok := snapshots[id]; ok {
				stack = append(stack, s.Parents...)
			}
		}
	}
	return scopes, nil
}

// The priority queue chooses newest verified Git groups among ready nodes.
// Causal child-before-parent ordering always wins over any timestamp or rank.
func orderedBranchSnapshots(byID map[domain.ContentHash]domain.Snapshot, allowed map[domain.ContentHash]bool, ranks map[domain.ContentHash]int) ([]domain.ContentHash, error) {
	degree := map[domain.ContentHash]int{}
	for id := range allowed {
		for _, p := range byID[id].ReachabilityParents() {
			if allowed[p] {
				degree[p]++
			}
		}
	}
	q := &branchContextQueue{snapshots: byID, ranks: ranks}
	for id := range allowed {
		if degree[id] == 0 {
			heap.Push(q, id)
		}
	}
	out := make([]domain.ContentHash, 0, len(allowed))
	for q.Len() > 0 {
		id := heap.Pop(q).(domain.ContentHash)
		out = append(out, id)
		for _, p := range byID[id].ReachabilityParents() {
			if allowed[p] {
				degree[p]--
				if degree[p] == 0 {
					heap.Push(q, p)
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

func (q branchContextQueue) Len() int { return len(q.ids) }
func (q branchContextQueue) Less(i, j int) bool {
	a, b := q.ids[i], q.ids[j]
	if q.ranks[a] != q.ranks[b] {
		return q.ranks[a] < q.ranks[b]
	}
	if !q.snapshots[a].CreatedAt.Equal(q.snapshots[b].CreatedAt) {
		return q.snapshots[a].CreatedAt.After(q.snapshots[b].CreatedAt)
	}
	return a < b
}
func (q branchContextQueue) Swap(i, j int) { q.ids[i], q.ids[j] = q.ids[j], q.ids[i] }
func (q *branchContextQueue) Push(v any)   { q.ids = append(q.ids, v.(domain.ContentHash)) }
func (q *branchContextQueue) Pop() any {
	i := len(q.ids) - 1
	v := q.ids[i]
	q.ids = q.ids[:i]
	return v
}

// A virtual projection root is request-local. It never writes grafts, refs,
// receipts or a replacement digest. All readers use the existing checked
// projection walker, including its provenance deduplication and byte bounds.
func (s *Service) projectBranchMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot) (domain.MemoryProjection, error) {
	history, err := s.ListHistory(ctx, repo)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	contributions, err := s.contextContributionsForEvidence(ctx, repo, history, nil)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return s.projectBranchMemoryWithContributions(ctx, repo, inclusion, snapshots, contributions)
}

func (s *Service) projectBranchMemoryWithContributions(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, contributions []contextContributionProjection) (domain.MemoryProjection, error) {
	plan, err := s.prepareContextContributionMemory(ctx, repo, inclusion, snapshots, contributions)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	allowed := map[domain.ContentHash]bool{}
	for _, id := range inclusion.SnapshotIDs {
		allowed[id] = true
	}
	rows := make([]domain.Snapshot, 0, len(allowed)+1)
	for _, snap := range plan.Snapshots {
		if !allowed[snap.ID] {
			continue
		}
		grafts := make([]domain.ContentHash, 0, len(snap.GraftParents))
		for _, p := range snap.GraftParents {
			if allowed[p] {
				grafts = append(grafts, p)
			}
		}
		snap.GraftParents = grafts
		rows = append(rows, snap)
	}
	key, _ := json.Marshal(struct {
		Version   int
		Inclusion domain.BranchContext
	}{1, inclusion})
	id := domain.HashContent(key)
	if allowed[id] {
		return domain.MemoryProjection{}, domain.ErrIntegrity
	}
	rows = append(rows, domain.Snapshot{ID: id, RepoID: repo, Parents: inclusion.Roots})
	result, err := ProjectMemory(ctx, branchProjectionSource{serviceProjectionSource{s.meta, s.blobs}, rows}, repo, id)
	result.Digest.SnapshotID = inclusion.SnapshotID
	result.Inclusion = &inclusion
	if err != nil {
		return result, err
	}
	return plan.Merge(ctx, result)
}

type branchProjectionSource struct {
	serviceProjectionSource
	rows []domain.Snapshot
}

func (s branchProjectionSource) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	return s.rows, nil
}
