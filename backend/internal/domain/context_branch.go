package domain

import (
	"container/heap"
	"crypto/sha256"
	"fmt"
	"reflect"
)

// Names are mutable projections. EventID is a causal compare-and-swap token,
// not a timestamp or a replica-local generation number.
type ContextBranch struct {
	ID       string
	Name     string
	EventID  string
	Archived bool
}

type ContextBranchProjection struct {
	Active   map[string]ContextBranch
	ByID     map[string]ContextBranch
	Released map[string]string
}

func LegacyContextBranchID(repo, name string) string {
	h := sha256.Sum256([]byte(repo + "\x00" + name))
	return fmt.Sprintf("legacy-%x", h[:16])
}

func IsBranchBindingEvent(e HistoryEvent) bool {
	return e.Kind == "birth" || e.Kind == "orphan" || e.Kind == "rename" || e.Kind == "archive"
}

// ProjectContextBranches rejects competing or incomplete identity histories.
// It consumes explicit dependencies so offline clocks and response ordering
// cannot reinterpret a rename as a birth, or merge unrelated same-name work.
func ProjectContextBranches(events []HistoryEvent) (ContextBranchProjection, error) {
	p := ContextBranchProjection{Active: map[string]ContextBranch{}, ByID: map[string]ContextBranch{}, Released: map[string]string{}}
	bindings := make([]HistoryEvent, 0)
	for _, e := range events {
		if IsBranchBindingEvent(e) {
			bindings = append(bindings, e)
		}
	}
	ordered, err := OrderHistoryEvents(bindings)
	if err != nil {
		return p, err
	}
	for _, e := range ordered {
		if err := p.apply(e); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p *ContextBranchProjection) apply(e HistoryEvent) error {
	conflict := func() error { return fmt.Errorf("context branch identity conflict for %q (%s)", e.Branch, e.ID) }
	if e.Kind == "birth" || e.Kind == "orphan" {
		if _, exists := p.ByID[e.BranchID]; exists {
			return conflict()
		}
		if _, exists := p.Active[e.Branch]; exists {
			return conflict()
		}
		if p.Released[e.Branch] != e.BindingParent {
			return conflict()
		}
		b := ContextBranch{ID: e.BranchID, Name: e.Branch, EventID: e.ID}
		p.Active[b.Name], p.ByID[b.ID] = b, b
		return nil
	}
	oldName := e.Branch
	if e.Kind == "rename" {
		oldName = e.PreviousBranch
	}
	b, exists := p.ByID[e.BranchID]
	if !exists {
		// Upgrades preserve the known name/identity without fabricating a birth.
		if e.BindingParent != "" || p.Released[oldName] != "" || e.BranchID != LegacyContextBranchID(e.RepoID, oldName) {
			return conflict()
		}
		if active, ok := p.Active[oldName]; ok && active.ID != e.BranchID {
			return conflict()
		}
		b = ContextBranch{ID: e.BranchID, Name: oldName}
	}
	if b.Archived || b.EventID != e.BindingParent || b.Name != oldName {
		return conflict()
	}
	if e.Kind == "rename" {
		if _, exists := p.Active[e.Branch]; exists {
			return conflict()
		}
		if p.Released[e.Branch] != e.NameParent {
			return conflict()
		}
	}
	delete(p.Active, oldName)
	p.Released[oldName] = e.ID
	b.EventID = e.ID
	b.Archived = e.Kind == "archive"
	if !b.Archived {
		b.Name = e.Branch
		p.Active[b.Name] = b
	}
	p.ByID[b.ID] = b
	return nil
}

func (p ContextBranchProjection) Identity(repo, name string) string {
	if b, ok := p.Active[name]; ok {
		return b.ID
	}
	return LegacyContextBranchID(repo, name)
}

// An observation can describe a past name. A live continuation must
// address the identity currently bound to that name, even if two tips coincide.
func ValidateHistoryBranch(events []HistoryEvent, e HistoryEvent) error {
	if IsBranchBindingEvent(e) {
		_, err := ProjectContextBranches(append(events, e))
		return err
	}
	if e.Kind != "advance" {
		return nil
	}
	p, err := ProjectContextBranches(events)
	if err != nil {
		return err
	}
	if b, ok := p.Active[e.Branch]; ok {
		if b.ID != e.BranchID {
			return fmt.Errorf("branch %q belongs to a different context identity", e.Branch)
		}
	} else if p.Released[e.Branch] != "" || p.ByID[e.BranchID].ID != "" {
		return fmt.Errorf("context branch %q was renamed or archived", e.Branch)
	}
	return nil
}

// OrderHistoryEvents keeps ordinary observation order while satisfying the
// explicit dependencies of identity changes, including clocks that moved back.
func OrderHistoryEvents(events []HistoryEvent) ([]HistoryEvent, error) {
	byID := make(map[string]HistoryEvent, len(events))
	for _, e := range events {
		if old, exists := byID[e.ID]; exists && !reflect.DeepEqual(old, e) {
			return nil, fmt.Errorf("conflicting history operation %s", e.ID)
		}
		byID[e.ID] = e
	}
	indegree := make(map[string]int, len(byID))
	children := make(map[string][]string, len(byID))
	ready := &historyQueue{}
	for id, e := range byID {
		deps := []string{e.BindingParent}
		if e.NameParent != e.BindingParent {
			deps = append(deps, e.NameParent)
		}
		for _, parent := range deps {
			if parent == "" {
				continue
			}
			if _, exists := byID[parent]; !exists {
				return nil, fmt.Errorf("missing history dependency %s", parent)
			}
			indegree[id]++
			children[parent] = append(children[parent], id)
		}
		if indegree[id] == 0 {
			*ready = append(*ready, e)
		}
	}
	heap.Init(ready)
	out := make([]HistoryEvent, 0, len(byID))
	for ready.Len() > 0 {
		e := heap.Pop(ready).(HistoryEvent)
		out = append(out, e)
		for _, child := range children[e.ID] {
			indegree[child]--
			if indegree[child] == 0 {
				heap.Push(ready, byID[child])
			}
		}
	}
	if len(out) != len(byID) {
		return nil, fmt.Errorf("cyclic history dependency")
	}
	return out, nil
}

type historyQueue []HistoryEvent

func (q historyQueue) Len() int { return len(q) }
func (q historyQueue) Less(i, j int) bool {
	if q[i].CreatedAt.Equal(q[j].CreatedAt) {
		return q[i].ID < q[j].ID
	}
	return q[i].CreatedAt.Before(q[j].CreatedAt)
}
func (q historyQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *historyQueue) Push(x any)   { *q = append(*q, x.(HistoryEvent)) }
func (q *historyQueue) Pop() any     { last := len(*q) - 1; x := (*q)[last]; *q = (*q)[:last]; return x }
