package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// BranchLifecycleTagPrefix is a reserved immutable-tag namespace used as a
// backward-compatible branch lifecycle event log. Older clients preserve
// these refs as ordinary tags instead of rejecting an unknown ref kind.
const BranchLifecycleTagPrefix = "cxt/branch-state/v1/"

type BranchLifecycleState string

const (
	BranchActive   BranchLifecycleState = "active"
	BranchArchived BranchLifecycleState = "archived"
)

type BranchLifecycleEvent struct {
	Branch     string
	Generation uint64
	State      BranchLifecycleState
	Target     ContentHash
	Ref        Ref
}

func parseBranchLifecycleTagName(name string) (BranchLifecycleEvent, bool, error) {
	if !strings.HasPrefix(name, BranchLifecycleTagPrefix) {
		return BranchLifecycleEvent{}, false, nil
	}
	rest := strings.TrimPrefix(name, BranchLifecycleTagPrefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) != 4 || len(parts[0]) != 20 {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: malformed branch lifecycle tag %q", ErrInvalidRef, name)
	}
	generation, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || generation == 0 || fmt.Sprintf("%020d", generation) != parts[0] {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: invalid branch lifecycle generation in %q", ErrInvalidRef, name)
	}
	state := BranchLifecycleState(parts[1])
	if state != BranchActive && state != BranchArchived {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: invalid branch lifecycle state in %q", ErrInvalidRef, name)
	}
	if len(parts[2]) != 64 || parts[2] != strings.ToLower(parts[2]) {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: invalid branch lifecycle target in %q", ErrInvalidRef, name)
	}
	target := ContentHash("sha256:" + parts[2])
	if err := ValidateContentHash(target); err != nil {
		return BranchLifecycleEvent{}, true, err
	}
	if err := ValidateBranchName(parts[3]); err != nil {
		return BranchLifecycleEvent{}, true, err
	}
	return BranchLifecycleEvent{Branch: parts[3], Generation: generation, State: state, Target: target}, true, nil
}

func NewBranchLifecycleRef(repoID, branch string, target ContentHash, generation uint64, state BranchLifecycleState) (Ref, error) {
	if err := ValidateBranchName(branch); err != nil {
		return Ref{}, err
	}
	if err := ValidateContentHash(target); err != nil {
		return Ref{}, err
	}
	if generation == 0 || (state != BranchActive && state != BranchArchived) {
		return Ref{}, fmt.Errorf("%w: invalid branch lifecycle event", ErrInvalidRef)
	}
	name := fmt.Sprintf("%s%020d/%s/%s/%s", BranchLifecycleTagPrefix, generation, state, strings.TrimPrefix(string(target), "sha256:"), branch)
	ref := Ref{Kind: RefTag, Name: name, RepoID: repoID, Target: target}
	if err := ValidateRef(ref); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func ParseBranchLifecycleRef(ref Ref) (BranchLifecycleEvent, bool, error) {
	if ref.Kind != RefTag {
		return BranchLifecycleEvent{}, false, nil
	}
	event, ok, err := parseBranchLifecycleTagName(ref.Name)
	if err != nil || !ok {
		return BranchLifecycleEvent{}, ok, err
	}
	if ref.Target != event.Target {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: lifecycle tag target does not match its name", ErrInvalidRef)
	}
	event.Ref = ref
	return event, true, nil
}

func branchLifecycleLater(left, right BranchLifecycleEvent) bool {
	if left.Generation != right.Generation {
		return left.Generation > right.Generation
	}
	if left.State != right.State {
		return left.State == BranchActive
	}
	if left.Target != right.Target {
		return left.Target > right.Target
	}
	return left.Ref.Name > right.Ref.Name
}

func LatestBranchLifecycle(refs []Ref, branch string) (BranchLifecycleEvent, bool, error) {
	var latest BranchLifecycleEvent
	found := false
	for _, ref := range refs {
		event, ok, err := ParseBranchLifecycleRef(ref)
		if err != nil {
			return BranchLifecycleEvent{}, false, err
		}
		if !ok || event.Branch != branch {
			continue
		}
		if !found || branchLifecycleLater(event, latest) {
			latest, found = event, true
		}
	}
	return latest, found, nil
}

// BranchLifecycleStates projects the append-only event refs into one latest
// state per branch in a single pass. List/manifest consumers use this instead
// of rescanning the complete immutable event history for every branch.
func BranchLifecycleStates(refs []Ref) (map[string]BranchLifecycleEvent, error) {
	states := make(map[string]BranchLifecycleEvent)
	for _, ref := range refs {
		event, ok, err := ParseBranchLifecycleRef(ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if current, found := states[event.Branch]; !found || branchLifecycleLater(event, current) {
			states[event.Branch] = event
		}
	}
	return states, nil
}

// ProjectBranchLifecycleRefs derives the externally visible mutable
// projections from raw refs plus immutable lifecycle events. An archive hides
// only the exact branch target it observed. If a crash left symbolic HEAD
// pointing at that hidden branch, HEAD is exposed as detached at the archived
// snapshot so manifests never contain a dangling symbolic ref.
func ProjectBranchLifecycleRefs(refs []Ref) ([]Ref, error) {
	states, err := BranchLifecycleStates(refs)
	if err != nil {
		return nil, err
	}
	branches := make(map[string]Ref)
	for _, ref := range refs {
		if ref.Kind == RefBranch {
			branches[ref.Name] = ref
		}
	}
	out := make([]Ref, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind == RefBranch {
			latest, ok := states[ref.Name]
			if ok && latest.State == BranchArchived && latest.Target == ref.Target {
				continue
			}
		}
		if ref.Kind == RefHEAD && ref.Symbolic != "" {
			branch := strings.TrimPrefix(ref.Symbolic, "refs/heads/")
			latest, ok := states[branch]
			raw, exists := branches[branch]
			if ok && latest.State == BranchArchived && (!exists || raw.Target == latest.Target) {
				ref.Symbolic = ""
				ref.Target = latest.Target
			}
		}
		out = append(out, ref)
	}
	return out, nil
}

func NextBranchLifecycleGeneration(refs []Ref, branch string) (uint64, error) {
	latest, ok, err := LatestBranchLifecycle(refs, branch)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 1, nil
	}
	if latest.Generation == ^uint64(0) {
		return 0, fmt.Errorf("%w: branch lifecycle generation exhausted", ErrInvalidRef)
	}
	return latest.Generation + 1, nil
}
