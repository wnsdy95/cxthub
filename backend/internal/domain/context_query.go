package domain

import (
	"fmt"
	"sort"
	"strings"
)

// ContextSelection is an explicit read position, not a mutable branch command.
// It is shared by REST and MCP; neither adapter defines its own history scope.
type ContextSelection struct {
	Branch   string `json:"branch,omitempty"`
	Position string `json:"position,omitempty"`
	Scope    string `json:"scope,omitempty"`
}

type ContextQueryView struct {
	Semantics ContextSemantics   `json:"semantics"`
	Version   int                `json:"version"`
	Revision  RepositoryRevision `json:"revision"`
	Position  ContentHash        `json:"position,omitempty"`
	Snapshots []Snapshot         `json:"snapshots"`
	History   []HistoryEvent     `json:"history"`
}

// SelectContext only consumes one coherent repository generation. Missing
// selected positions fail explicitly instead of silently selecting the tip.
func SelectContext(view RepositoryView, in ContextSelection, defaultBranch string) (ContextQueryView, error) {
	out := ContextQueryView{Version: 1, Revision: view.Revision, Semantics: view.Semantics, Snapshots: []Snapshot{}, History: []HistoryEvent{}}
	if in.Scope == "" {
		in.Scope = "all"
	}
	if in.Scope != "all" && in.Scope != "current" && in.Scope != "previous" && in.Scope != "archived" {
		return out, fmt.Errorf("%w: scope must be all, current, previous, or archived", ErrValidation)
	}
	byID := make(map[ContentHash]Snapshot, len(view.Snapshots))
	for _, s := range view.Snapshots {
		if _, exists := byID[s.ID]; exists {
			return out, fmt.Errorf("%w: duplicate snapshot", ErrIntegrity)
		}
		byID[s.ID] = s
	}
	if in.Position != "" || in.Scope == "current" || in.Scope == "previous" {
		if in.Position == "" {
			return out, fmt.Errorf("%w: position is required for %s history", ErrValidation, in.Scope)
		}
		var err error
		out.Position, err = resolveContextPosition(view, strings.TrimSpace(in.Position), defaultBranch)
		if err != nil {
			return out, err
		}
	}
	var bindings ContextBranchProjection
	if in.Branch != "" || in.Scope == "archived" {
		var err error
		bindings, err = ProjectContextBranches(view.History)
		if err != nil {
			return out, fmt.Errorf("%w: %v", ErrIntegrity, err)
		}
	}
	identity := bindings.Active[in.Branch].ID
	// An archived identity still has a stable name unless that name was reused.
	if identity == "" && in.Branch != "" {
		for _, b := range bindings.ByID {
			if b.Name != in.Branch {
				continue
			}
			if identity != "" && identity != b.ID {
				return out, fmt.Errorf("%w: ambiguous archived branch", ErrConflict)
			}
			identity = b.ID
		}
	}
	for _, event := range view.History {
		if in.Branch == "" || contextEventMatches(event, in.Branch, identity) {
			out.History = append(out.History, event)
		}
	}
	// Scope changes the returned captures, never the meaning of a receipt. A
	// branch-specific result carries the birth dependencies of its receipts too.
	if out.Semantics.Version == 0 {
		out.Semantics = ProjectContextSemantics(view.Snapshots, view.History)
	}
	if in.Branch != "" {
		selectedEvents := make(map[string]bool, len(out.History))
		byEvent := make(map[string]HistoryEvent, len(view.History))
		for _, e := range out.History {
			selectedEvents[e.ID] = true
		}
		for _, e := range view.History {
			byEvent[e.ID] = e
		}
		facts := make([]ContextMergeEvidence, 0)
		for _, fact := range out.Semantics.Merges {
			if !selectedEvents[fact.EventID] {
				continue
			}
			facts = append(facts, fact)
			if e, ok := byEvent[fact.BirthID]; ok && !selectedEvents[e.ID] {
				out.History = append(out.History, e)
				selectedEvents[e.ID] = true
			}
		}
		out.Semantics.Merges = facts
	}
	var roots, archiveRoots, branchRoots []ContentHash
	for _, r := range view.Refs {
		if r.Target != "" {
			roots = append(roots, r.Target)
		}
	}
	states, err := BranchLifecycleStates(view.Refs)
	if err != nil {
		return out, err
	}
	for _, state := range states {
		if state.State == BranchArchived {
			archiveRoots = append(archiveRoots, state.Target)
		}
	}
	// Logical identities survive reuse of a name. Legacy lifecycle tags only
	// describe that name's latest generation and cannot hide older archives.
	for _, event := range view.History {
		if event.Kind == "archive" && bindings.ByID[event.BranchID].Archived {
			archiveRoots = append(archiveRoots, event.Source, event.Target)
		}
	}
	for _, r := range view.Refs {
		if r.Kind == RefBranch && r.Name == in.Branch {
			branchRoots = append(branchRoots, r.Target)
		}
		if e, ok, err := ParseBranchLifecycleRef(r); err != nil {
			return out, err
		} else if ok && e.Branch == in.Branch && identity == "" {
			branchRoots = append(branchRoots, e.Target)
		}
	}
	for _, event := range out.History {
		branchRoots = append(branchRoots, event.Source, event.Target, event.SharedTarget)
	}
	if identity == "" {
		for _, s := range view.Snapshots {
			if s.Branch == in.Branch {
				branchRoots = append(branchRoots, s.ID)
				continue
			}
			for _, name := range s.Branches {
				if name == in.Branch {
					branchRoots = append(branchRoots, s.ID)
					break
				}
			}
		}
	}
	var retained, selected, archived, branch map[ContentHash]bool
	if in.Scope == "previous" {
		retained = ContextClosure(byID, roots)
	}
	if in.Scope == "current" || in.Scope == "previous" {
		selected = ContextClosure(byID, []ContentHash{out.Position})
	}
	if in.Scope == "archived" {
		archived = ContextClosure(byID, archiveRoots)
	}
	if in.Branch != "" {
		branch = ContextClosure(byID, branchRoots)
	}
	for _, s := range view.Snapshots {
		if in.Branch != "" && !branch[s.ID] {
			continue
		}
		if in.Scope == "current" && !selected[s.ID] {
			continue
		}
		if in.Scope == "previous" && (selected[s.ID] || !retained[s.ID]) {
			continue
		}
		if in.Scope == "archived" && !archived[s.ID] {
			continue
		}
		out.Snapshots = append(out.Snapshots, s)
	}
	sort.Slice(out.Snapshots, func(i, j int) bool {
		a, b := out.Snapshots[i], out.Snapshots[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	return out, nil
}

func contextEventMatches(e HistoryEvent, name, identity string) bool {
	if identity != "" {
		return e.BranchID == identity
	}
	return e.Branch == name
}

// ContextClosure is iterative and follows natural and active overlay edges.
func ContextClosure(byID map[ContentHash]Snapshot, roots []ContentHash) map[ContentHash]bool {
	stack := append([]ContentHash(nil), roots...)
	seen := map[ContentHash]bool{}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if s, ok := byID[id]; ok {
			stack = append(stack, s.ReachabilityParents()...)
		}
	}
	return seen
}

func resolveContextPosition(v RepositoryView, selector, defaultBranch string) (ContentHash, error) {
	if strings.EqualFold(selector, "HEAD") {
		selector = defaultBranch
	}
	for _, r := range v.Refs {
		if (r.Kind == RefBranch || r.Kind == RefTag) && r.Name == selector {
			for _, s := range v.Snapshots {
				if s.ID == r.Target {
					return s.ID, nil
				}
			}
			return "", fmt.Errorf("%w: selected ref target is missing", ErrIntegrity)
		}
	}
	var found ContentHash
	for _, s := range v.Snapshots {
		if string(s.ID) == selector || (!strings.HasPrefix(selector, "sha256:") && len(selector) >= 6 && strings.HasPrefix(strings.TrimPrefix(string(s.ID), "sha256:"), selector)) {
			if found != "" {
				return "", fmt.Errorf("%w: ambiguous context prefix", ErrValidation)
			}
			found = s.ID
		}
	}
	if found == "" {
		return "", fmt.Errorf("%w: context position %q", ErrNotFound, selector)
	}
	return found, nil
}
