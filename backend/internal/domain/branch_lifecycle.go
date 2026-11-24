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
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: malformed branch lifecycle tag %q", ErrValidation, name)
	}
	generation, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || generation == 0 || fmt.Sprintf("%020d", generation) != parts[0] {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: invalid branch lifecycle generation in %q", ErrValidation, name)
	}
	state := BranchLifecycleState(parts[1])
	if state != BranchActive && state != BranchArchived {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: invalid branch lifecycle state in %q", ErrValidation, name)
	}
	if len(parts[2]) != 64 || parts[2] != strings.ToLower(parts[2]) {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: invalid branch lifecycle target in %q", ErrValidation, name)
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

func ParseBranchLifecycleRef(ref Ref) (BranchLifecycleEvent, bool, error) {
	if ref.Kind != RefTag {
		return BranchLifecycleEvent{}, false, nil
	}
	event, ok, err := parseBranchLifecycleTagName(ref.Name)
	if err != nil || !ok {
		return BranchLifecycleEvent{}, ok, err
	}
	if ref.Target != event.Target {
		return BranchLifecycleEvent{}, true, fmt.Errorf("%w: lifecycle tag target does not match its name", ErrValidation)
	}
	event.Ref = ref
	return event, true, nil
}
