package domain

import (
	"sort"
	"time"
)

// SyncAuditCheck reports facts rather than attempting history repair. Missing
// local provenance and unavailable GitHub objects are never called corruption.
type SyncAuditCheck struct {
	ID       string       `json:"id"`
	EventID  string       `json:"event_id,omitempty"`
	Snapshot ContentHash  `json:"snapshot,omitempty"`
	Branch   string       `json:"branch,omitempty"`
	State    string       `json:"state"`
	Code     string       `json:"code"`
	Expected string       `json:"expected,omitempty"`
	Actual   string       `json:"actual,omitempty"`
	Creation *GitCreation `json:"creation,omitempty"`
}
type SyncAuditPage struct {
	Phase      string           `json:"phase,omitempty"`
	Version    int              `json:"version"`
	Revision   string           `json:"revision"`
	CheckedAt  time.Time        `json:"checked_at"`
	Checks     []SyncAuditCheck `json:"checks"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Processed  int              `json:"processed"`
	Total      int              `json:"total"`
}

// AuditGraphContracts is independent of graph routing. It checks immutable
// operation facts against the finished projection, before UI folding/layout.
func AuditGraphContracts(v RepositoryView, g *GraphState) []SyncAuditCheck {
	out := []SyncAuditCheck{}
	add := func(e HistoryEvent, state, code, expected, actual string) {
		out = append(out, SyncAuditCheck{ID: e.ID + ":" + code, EventID: e.ID, Snapshot: e.Source, Branch: e.Branch, State: state, Code: code, Expected: expected, Actual: actual, Creation: e.Creation})
	}
	snaps := map[ContentHash]Snapshot{}
	for _, s := range v.Snapshots {
		snaps[s.ID] = s
	}
	for _, s := range v.Snapshots {
		for _, p := range s.ReachabilityParents() {
			if _, ok := snaps[p]; !ok {
				out = append(out, SyncAuditCheck{ID: string(s.ID) + ":" + string(p), Snapshot: s.ID, State: "incomplete", Code: "missing_parent", Expected: string(p)})
			}
		}
	}
	for _, e := range v.History {
		if err := ValidateHistoryEvent(e); err != nil {
			add(e, "mismatch", "invalid_history", "", err.Error())
			continue
		}
		if err := ValidateCreationOrigin(v.History, e); err != nil {
			add(e, "mismatch", "origin_identity_mismatch", e.Creation.OriginBranchID, "")
		}
		if e.Kind == "birth" || e.Kind == "orphan" || e.Kind == "attach" {
			if e.Creation == nil || e.Creation.Evidence != "process-argv" {
				add(e, "incomplete", "command_unavailable", "", "")
			} else {
				add(e, "verified", "command_recorded", e.Creation.StartCommit, e.GitAfter)
			}
		}
		if e.Kind == "birth" && e.Source != "" && e.Source == e.Target {
			if _, ok := snaps[e.Source]; !ok {
				add(e, "incomplete", "birth_source_missing", string(e.Source), "")
				continue
			}
			if g == nil {
				continue
			}
			count := 0
			actual := ""
			for _, b := range g.Operations.Births {
				if b.EventID == e.ID {
					count++
					actual = string(b.Source)
				}
			}
			if count != 1 || actual != string(e.Source) {
				add(e, "mismatch", "birth_projection_mismatch", string(e.Source), actual)
			}
		}
		if e.Kind == "pr-merge" && e.PRCompleted && e.PR != nil {
			_, source := snaps[e.Source]
			_, before := snaps[e.SharedTarget]
			_, after := snaps[e.Target]
			if !source || !before || !after {
				add(e, "incomplete", "merge_objects_missing", "", "")
				continue
			}
			if g == nil {
				continue
			}
			found := false
			for _, m := range g.Operations.Merges {
				if m.EventID == e.ID && m.Source == e.Source && m.Before == e.SharedTarget && m.After == e.Target && m.PRNumber == e.PR.Number {
					found = true
				}
			}
			if !found {
				add(e, "mismatch", "merge_projection_missing", string(e.Source), "")
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AuditIntegrationContracts checks the cross-reader roots and rendered PR path
// without re-running the projection builder. Memory composition consumes these
// same roots; this checks provenance, never the truth of generated sentences.
func AuditIntegrationContracts(g GraphState) []SyncAuditCheck {
	out := []SyncAuditCheck{}
	has := func(ids []ContentHash, id ContentHash) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	for name, c := range g.BranchContexts {
		for _, m := range c.Merges {
			if m.State != "included" {
				continue
			}
			add := func(code string) {
				out = append(out, SyncAuditCheck{ID: name + ":" + m.EventID + ":" + code, EventID: m.EventID, Branch: name, Snapshot: m.Source, State: "mismatch", Code: code, Expected: string(m.Source)})
			}
			if !has(c.Roots, m.Source) || !has(c.SnapshotIDs, m.Source) || !has(g.BranchSnapshots[name], m.Source) {
				add("included_context_missing")
			}
			// A child inherits the destination's integrated context, not a new
			// merge operation on the child's lane. Its roots still need checking,
			// but only the original destination owns the rendered merge path.
			if m.DestinationBranchID != "" && m.DestinationBranchID != c.BranchID {
				continue
			}
			mergeID := ""
			for _, operation := range g.Operations.Merges {
				if operation.EventID == m.EventID && operation.Scope == c.BranchID && operation.Source == m.Source {
					mergeID = operation.ID
					break
				}
			}
			represented := false
			for _, integration := range g.Integrations {
				if mergeID != "" && integration.Scope == c.BranchID {
					if _, ok := integration.Parents[mergeID]; ok {
						represented = true
					}
				}
			}
			if !represented {
				add("included_merge_path_missing")
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
