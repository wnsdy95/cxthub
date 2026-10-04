package domain

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const QueryContractVersion = 1

// HistorySelection names a read scope. Labels recorded at capture time never
// decide membership. Source distinguishes local ancestry from server evidence.
type HistorySelection struct {
	Ref        string `json:"ref"`
	Branch     string `json:"branch,omitempty"`
	CodeCommit string `json:"code_commit,omitempty"`
	Scope      string `json:"scope"`
	Source     string `json:"source"`
}

type HistoryQueryResult struct {
	Segments          *ContextSegmentPage `json:"segments,omitempty"`
	Version           int                 `json:"version"`
	Selection         HistorySelection    `json:"selection"`
	StateHash         ContentHash         `json:"state_hash"`
	DeliveryStateHash ContentHash         `json:"delivery_state_hash,omitempty"`
	Revision          *RepositoryRevision `json:"revision,omitempty"`
	Position          ContentHash         `json:"position,omitempty"`
	Snapshots         []Snapshot          `json:"snapshots"`
	Complete          bool                `json:"complete"`
	Missing           []ContentHash       `json:"missing"`
	ServerChecked     bool                `json:"server_checked"`
	Inclusion         *BranchContext      `json:"inclusion,omitempty"`
}

// These wire values mirror the server's ContextQuery contract; no local policy
// infers PR inclusion. Unknown server versions must be rejected by the client.
type ContextSelection struct {
	SegmentLimit     int         `json:"segment_limit,omitempty"`
	SegmentOffset    int         `json:"segment_offset,omitempty"`
	SegmentStateHash ContentHash `json:"segment_state_hash,omitempty"`
	CodeCommit       string      `json:"code_commit,omitempty"`
	Branch           string      `json:"branch,omitempty"`
	Position         string      `json:"position,omitempty"`
	Scope            string      `json:"scope,omitempty"`
}
type BranchContext struct {
	BranchID    string               `json:"branch_id"`
	SnapshotID  ContentHash          `json:"snapshot_id"`
	CodeCommit  string               `json:"code_commit,omitempty"`
	Reason      string               `json:"reason"`
	Merges      []BranchContextMerge `json:"merges"`
	Roots       []ContentHash        `json:"roots"`
	SnapshotIDs []ContentHash        `json:"snapshot_ids"`
}
type BranchContextMerge struct {
	DestinationBranchID string      `json:"destination_branch_id"`
	EventID             string      `json:"event_id"`
	Source              ContentHash `json:"source"`
	Before              ContentHash `json:"before,omitempty"`
	MergeSHA            string      `json:"merge_sha"`
	PRNumber            int         `json:"pr_number"`
	State               string      `json:"state"`
	Reason              string      `json:"reason"`
	Order               int         `json:"order"`
}
type RepositoryRevision struct {
	Evidence uint64 `json:"evidence,string,omitempty"`
	Graph    uint64 `json:"graph,string"`
	Pending  uint64 `json:"pending,string"`
}
type ContextQueryView struct {
	Segments          *ContextSegmentPage `json:"segments,omitempty"`
	Revision          RepositoryRevision  `json:"revision"`
	Version           int                 `json:"version"`
	Branch            string              `json:"branch,omitempty"`
	StateHash         ContentHash         `json:"state_hash,omitempty"`
	DeliveryStateHash ContentHash         `json:"delivery_state_hash,omitempty"`
	Position          ContentHash         `json:"position,omitempty"`
	Snapshots         []Snapshot          `json:"snapshots"`
	History           []HistoryEvent      `json:"history,omitempty"`
	Inclusion         *BranchContext      `json:"inclusion,omitempty"`
}

const ContextSegmentVersion = 1
const MaxContextSegmentPage = 50

// ConversationSegment addresses a half-open range of canonical CIR events. Its
// identity includes repository and native stream provenance, never prose alone.
// Byte chunks remain an independent physical storage detail.
type ConversationSegment struct {
	ID         ContentHash  `json:"id"`
	RepoID     ContentHash  `json:"repo_id"`
	DocHash    ContentHash  `json:"doc_hash"`
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
	EventStart int          `json:"event_start"`
	EventEnd   int          `json:"event_end"`
}

// CommitContextBinding reuses an accepted immutable publication receipt. It
// attests to observed finalized captures, not every possible worker's activity.
type CommitContextBinding struct {
	EventID    string        `json:"event_id"`
	GitCommit  string        `json:"git_commit"`
	BranchID   string        `json:"branch_id"`
	WorktreeID string        `json:"worktree_id,omitempty"`
	SegmentIDs []ContentHash `json:"segment_ids"`
}

type ContextSegmentCoverage struct {
	SnapshotID         ContentHash            `json:"snapshot_id"`
	Segment            *ConversationSegment   `json:"segment,omitempty"`
	Bindings           []CommitContextBinding `json:"bindings"`
	Kind               string                 `json:"kind"` // verified_prefix, full_source, unavailable
	Reason             string                 `json:"reason"`
	TotalEvents        int                    `json:"total_events"`
	BaselineSnapshotID ContentHash            `json:"baseline_snapshot_id,omitempty"`
	BaselineDocHash    ContentHash            `json:"baseline_doc_hash,omitempty"`
}

// Pages follow the shared query's snapshot order, newest first. A verified
// prefix can refer to an older page; consumers must preserve that dependency.
// Complete means page exhaustion, not complete Git or capture coverage.
type ContextSegmentPage struct {
	Version   int                      `json:"version"`
	StateHash ContentHash              `json:"state_hash"`
	PageHash  ContentHash              `json:"page_hash"`
	Offset    int                      `json:"offset"`
	Next      int                      `json:"next"`
	Total     int                      `json:"total"`
	Complete  bool                     `json:"complete"`
	Entries   []ContextSegmentCoverage `json:"entries"`
}

// ReachableHistory returns child-before-parent order. Missing ancestors are
// explicit coverage gaps; cycles and duplicate IDs fail rather than truncate.
func ReachableHistory(ctx context.Context, snapshots []Snapshot, roots []ContentHash) ([]Snapshot, []ContentHash, error) {
	byID := make(map[ContentHash]int, len(snapshots))
	for i, s := range snapshots {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if _, exists := byID[s.ID]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate snapshot %s", ErrHashMismatch, s.ID)
		}
		byID[s.ID] = i
	}
	chosen := make([]bool, len(snapshots))
	count := 0
	missing := map[ContentHash]bool{}
	stack := append([]ContentHash(nil), roots...)
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i, ok := byID[id]
		if !ok {
			missing[id] = true
			continue
		}
		if chosen[i] {
			continue
		}
		chosen[i] = true
		count++
		stack = append(stack, snapshots[i].Parents...)
		stack = append(stack, snapshots[i].GraftParents...)
	}
	children := make([]int, len(snapshots))
	parents := func(i int, visit func(int)) {
		for _, list := range [][]ContentHash{snapshots[i].Parents, snapshots[i].GraftParents} {
			for _, p := range list {
				if parent, ok := byID[p]; ok && chosen[parent] {
					visit(parent)
				}
			}
		}
	}
	for i, yes := range chosen {
		if yes {
			parents(i, func(p int) { children[p]++ })
		}
	}
	ready := &historyHeap{snapshots: snapshots, indices: make([]int, 0, count)}
	for i, yes := range chosen {
		if yes && children[i] == 0 {
			ready.indices = append(ready.indices, i)
		}
	}
	heap.Init(ready)
	out := make([]Snapshot, 0, count)
	for ready.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		i := heap.Pop(ready).(int)
		out = append(out, snapshots[i])
		parents(i, func(p int) {
			children[p]--
			if children[p] == 0 {
				heap.Push(ready, p)
			}
		})
	}
	if len(out) != count {
		return nil, nil, fmt.Errorf("%w: cyclic context ancestry", ErrHashMismatch)
	}
	gaps := make([]ContentHash, 0, len(missing))
	for id := range missing {
		gaps = append(gaps, id)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return out, gaps, nil
}

// Heap entries are indices: large snapshot metadata is never copied into maps
// or heap operations while walking a large archive.
type historyHeap struct {
	snapshots []Snapshot
	indices   []int
}

func (h historyHeap) Len() int { return len(h.indices) }
func (h historyHeap) Less(i, j int) bool {
	a, b := h.snapshots[h.indices[i]], h.snapshots[h.indices[j]]
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID < b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}
func (h historyHeap) Swap(i, j int) { h.indices[i], h.indices[j] = h.indices[j], h.indices[i] }
func (h *historyHeap) Push(v any)   { h.indices = append(h.indices, v.(int)) }
func (h *historyHeap) Pop() any {
	n := len(h.indices)
	v := h.indices[n-1]
	h.indices = h.indices[:n-1]
	return v
}

// ValidateContextQuery keeps the server contract authoritative without inferring
// merge evidence in the CLI. It checks identities and ordered membership only.
func ValidateContextQuery(repo string, in ContextSelection, out ContextQueryView) error {
	if err := ValidateContextSegmentSelection(in); err != nil {
		return err
	}
	if out.Version != QueryContractVersion {
		return fmt.Errorf("unsupported context query version %d", out.Version)
	}
	if ValidateContentHash(out.StateHash) != nil || ValidateOptionalContentHash(out.DeliveryStateHash) != nil {
		return ErrHashMismatch
	}
	if strings.HasPrefix(in.Position, "sha256:") && string(out.Position) != in.Position {
		return ErrSelectionChanged
	}
	seen := map[ContentHash]bool{}
	for _, s := range out.Snapshots {
		if s.RepoID != repo || ValidateContentHash(s.ID) != nil || seen[s.ID] {
			return ErrHashMismatch
		}
		seen[s.ID] = true
	}
	if out.Inclusion != nil {
		if in.CodeCommit != "" && out.Inclusion.CodeCommit != in.CodeCommit {
			return ErrSelectionChanged
		}
		if len(out.Inclusion.SnapshotIDs) != len(out.Snapshots) {
			return ErrHashMismatch
		}
		for i, id := range out.Inclusion.SnapshotIDs {
			if out.Snapshots[i].ID != id {
				return ErrHashMismatch
			}
		}
	}
	return ValidateContextSegmentPage(repo, in, out)
}

func ValidateContextSegmentSelection(in ContextSelection) error {
	if in.SegmentLimit < 0 || in.SegmentLimit > MaxContextSegmentPage || in.SegmentOffset < 0 || (in.SegmentLimit == 0 && (in.SegmentOffset != 0 || in.SegmentStateHash != "")) {
		return fmt.Errorf("invalid_arguments: segment_limit must be 1..%d for paged segments", MaxContextSegmentPage)
	}
	if in.SegmentOffset > 0 && in.SegmentStateHash == "" {
		return fmt.Errorf("invalid_arguments: segment continuation requires segment_state_hash")
	}
	return ValidateOptionalContentHash(in.SegmentStateHash)
}

// ContextSegmentID mirrors the versioned server descriptor hash. This verifies
// wire identity, not the event-prefix evidence (which remains server-owned).
func ContextSegmentID(segment ConversationSegment) (ContentHash, error) {
	segment.ID = ""
	raw, err := json.Marshal(struct {
		Version int
		Segment ConversationSegment
	}{ContextSegmentVersion, segment})
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}
func ContextSegmentPageHash(page ContextSegmentPage) (ContentHash, error) {
	page.PageHash = ""
	raw, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

// ValidateContextSegmentPage validates the selected source ranges and bounded
// cursor contract without inventing any inclusion or publication facts locally.
func ValidateContextSegmentPage(repo string, in ContextSelection, out ContextQueryView) error {
	page := out.Segments
	if page == nil {
		if in.SegmentLimit > 0 {
			return fmt.Errorf("unsupported context segment contract: response omitted segments")
		}
		return nil
	}
	if page.Version != ContextSegmentVersion {
		return fmt.Errorf("unsupported context segment version %d", page.Version)
	}
	if ValidateContentHash(page.StateHash) != nil || page.StateHash != out.StateHash || ValidateContentHash(page.PageHash) != nil {
		return ErrHashMismatch
	}
	if in.SegmentStateHash != "" && page.StateHash != in.SegmentStateHash {
		return ErrSelectionChanged
	}
	if page.Offset < 0 || len(page.Entries) != len(out.Snapshots) || page.Offset > page.Total || len(page.Entries) > MaxContextSegmentPage || len(page.Entries) > page.Total-page.Offset {
		return ErrHashMismatch
	}
	if in.SegmentLimit > 0 && (page.Offset != in.SegmentOffset || len(page.Entries) != min(in.SegmentLimit, page.Total-page.Offset)) {
		return ErrHashMismatch
	}
	end := page.Offset + len(page.Entries)
	if end == page.Total {
		if page.Next != -1 || !page.Complete {
			return ErrHashMismatch
		}
	} else if len(page.Entries) == 0 || page.Next != end || page.Complete {
		return ErrHashMismatch
	}
	want, err := ContextSegmentPageHash(*page)
	if err != nil {
		return err
	}
	if want != page.PageHash {
		return ErrHashMismatch
	}
	byID := make(map[ContentHash]int, len(out.Snapshots))
	for i, snap := range out.Snapshots {
		byID[snap.ID] = i
	}
	receiptIDs := map[string]bool{}
	for i, entry := range page.Entries {
		snap := out.Snapshots[i]
		if entry.SnapshotID != snap.ID || entry.Reason == "" {
			return ErrHashMismatch
		}
		switch entry.Kind {
		case "unavailable":
			if entry.Segment != nil || entry.TotalEvents != -1 || entry.BaselineSnapshotID != "" || entry.BaselineDocHash != "" {
				return ErrHashMismatch
			}
		case "full_source", "verified_prefix":
			segment := entry.Segment
			if segment == nil || segment.RepoID != ContentHash(repo) || segment.DocHash != snap.DocHash || ValidateContentHash(segment.DocHash) != nil || ValidateContentHash(segment.ID) != nil || segment.EventStart < 0 || segment.EventEnd < segment.EventStart || segment.EventEnd != entry.TotalEvents {
				return ErrHashMismatch
			}
			want, err := ContextSegmentID(*segment)
			if err != nil {
				return err
			}
			if segment.ID != want {
				return ErrHashMismatch
			}
			if entry.Kind == "full_source" {
				if segment.EventStart != 0 || entry.BaselineSnapshotID != "" || entry.BaselineDocHash != "" {
					return ErrHashMismatch
				}
			} else {
				base := Snapshot{ID: entry.BaselineSnapshotID, DocHash: entry.BaselineDocHash}
				if baseIndex, ok := byID[entry.BaselineSnapshotID]; ok {
					base = out.Snapshots[baseIndex]
				}
				if ValidateContentHash(base.ID) != nil {
					return ErrHashMismatch
				}
				if base.ID == snap.ID || base.DocHash != entry.BaselineDocHash || ValidateContentHash(entry.BaselineDocHash) != nil || len(snap.Parents) != 1 || snap.Parents[0] != base.ID || len(entry.Bindings) == 0 || segment.Provider == "" || segment.SessionID == "" {
					return ErrHashMismatch
				}
			}
		default:
			return fmt.Errorf("unsupported segment coverage %q", entry.Kind)
		}
		for _, binding := range entry.Bindings {
			if !contextOperationID(binding.EventID) || receiptIDs[binding.EventID] || !ValidGitOID(binding.GitCommit) || binding.BranchID == "" || len(binding.BranchID) > 128 || (binding.WorktreeID != "" && !contextOperationID(binding.WorktreeID)) {
				return ErrHashMismatch
			}
			receiptIDs[binding.EventID] = true
			if entry.Segment == nil {
				if len(binding.SegmentIDs) != 0 {
					return ErrHashMismatch
				}
			} else if len(binding.SegmentIDs) != 1 || binding.SegmentIDs[0] != entry.Segment.ID {
				return ErrHashMismatch
			}
		}
	}
	return nil
}
func contextOperationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
