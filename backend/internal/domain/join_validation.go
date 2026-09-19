package domain

import (
	"fmt"
	"strings"
)

func validateJoinHashes(ids ...ContentHash) error {
	for _, id := range ids {
		if err := ValidateContentHash(id); err != nil {
			return err
		}
	}
	return nil
}

// ValidateJoinMutation checks the lossless mutation contract shared by all stores.
func ValidateJoinMutation(m JoinMutation) error {
	if err := validateJoinHashes(m.RepoID, m.Source, m.ExpectedHead, m.NewHead); err != nil {
		return err
	}
	if len(m.Segment) == 0 || m.Segment[0] != m.Source {
		return fmt.Errorf("%w: join segment must start at source", ErrValidation)
	}
	segmentSeen := map[ContentHash]bool{}
	for _, id := range m.Segment {
		if err := ValidateContentHash(id); err != nil {
			return err
		}
		if segmentSeen[id] {
			return fmt.Errorf("%w: duplicate join segment snapshot", ErrIntegrity)
		}
		segmentSeen[id] = true
	}
	if !segmentSeen[m.NewHead] {
		return fmt.Errorf("%w: join head is outside segment", ErrValidation)
	}
	if err := ValidateBranchName(m.Branch); err != nil {
		return err
	}
	if (m.ForkName == "") != (m.ForkTip == "") {
		return fmt.Errorf("%w: join fork name and tip must be provided together", ErrValidation)
	}
	if m.ForkName != "" {
		if err := ValidateBranchName(m.ForkName); err != nil {
			return err
		}
		if err := ValidateContentHash(m.ForkTip); err != nil {
			return err
		}
		if !segmentSeen[m.ForkTip] {
			return fmt.Errorf("%w: join session tip is outside segment", ErrValidation)
		}
	}
	if len(m.Grafts) == 0 {
		return fmt.Errorf("%w: join requires at least one graft patch", ErrValidation)
	}

	seen := map[ContentHash]bool{}
	for _, patch := range m.Grafts {
		if err := ValidateContentHash(patch.SnapshotID); err != nil {
			return err
		}
		if err := validateJoinHashes(patch.Parents...); err != nil {
			return err
		}
		if seen[patch.SnapshotID] {
			return ErrIntegrity
		}
		seen[patch.SnapshotID] = true
	}
	return validateJoinMutationPlan(m)
}

// validateJoinMutationPlan revalidates the join plan computed by the service at the storage port boundary to ensure it does not lose the previous head or remaining session branches.
func validateJoinMutationPlan(m JoinMutation) error {
	if m.Branch == HeadRefName {
		return fmt.Errorf("%w: HEAD is not a joinable branch", ErrValidation)
	}
	if len(m.Segment) == 0 || m.Segment[0] != m.Source {
		return fmt.Errorf("%w: join segment must start at source", ErrValidation)
	}
	tip := m.Segment[len(m.Segment)-1]
	switch {
	case len(m.Segment) == 1:
		if m.NewHead != m.Source || m.ForkName != "" || m.ForkTip != "" {
			return fmt.Errorf("%w: single-snapshot join cannot create a residual session ref", ErrValidation)
		}
	case m.NewHead == tip:
		if m.ForkName != "" || m.ForkTip != "" {
			return fmt.Errorf("%w: whole-segment join cannot create a residual session ref", ErrValidation)
		}
	case m.NewHead == m.Source:
		if m.ForkName == "" || m.ForkTip != tip {
			return fmt.Errorf("%w: partial join must preserve the segment tip in a session ref", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: join head must be the source or segment tip", ErrValidation)
	}
	if m.ForkName != "" && !strings.HasPrefix(m.ForkName, SessionRefPrefix(m.Branch)) {
		return fmt.Errorf("%w: join session ref is not scoped to git branch %q", ErrValidation, m.Branch)
	}

	// If X already points to the previous head as a natural parent, H is included in the service request. While it can be deduplicated during storage, rejecting the plan if H itself is removed could cause a ref move to lose the existing lineage.
	sourcePatch := false
	for _, patch := range m.Grafts {
		if patch.SnapshotID != m.Source {
			continue
		}
		sourcePatch = true
		keepsHead := false
		for _, parent := range patch.Parents {
			if parent == m.ExpectedHead {
				keepsHead = true
				break
			}
		}
		if !keepsHead {
			return fmt.Errorf("%w: source graft patch does not preserve the previous branch head", ErrValidation)
		}
	}
	if !sourcePatch {
		return fmt.Errorf("%w: join requires a source graft patch", ErrValidation)
	}
	return nil
}

// validateJoinSegmentTopology checks that, between service queries and storage application, the target branch/session ref has not changed and X…tip remains the same single first-parent branch. Objects are uploaded but children not yet attached to any ref are excluded from the public graph.
func validateJoinSegmentTopology(
	m JoinMutation,
	byID map[ContentHash]Snapshot,
	attached map[ContentHash]bool,
) error {
	for i, id := range m.Segment {
		snap, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: join segment snapshot %s", ErrNotFound, id)
		}
		if !attached[id] {
			return fmt.Errorf("%w: join segment snapshot %s is no longer attached to its branch/session ref", ErrConflict, id)
		}
		if i > 0 && (len(snap.Parents) == 0 || snap.Parents[0] != m.Segment[i-1]) {
			return fmt.Errorf("%w: join segment is no longer a first-parent chain", ErrConflict)
		}
	}
	for _, patch := range m.Grafts {
		if !attached[patch.SnapshotID] {
			return fmt.Errorf("%w: graft patch snapshot %s is outside the target git branch scope", ErrConflict, patch.SnapshotID)
		}
		for _, parent := range patch.Parents {
			if !attached[parent] {
				return fmt.Errorf("%w: graft parent %s is outside the target git branch scope", ErrConflict, parent)
			}
		}
	}

	firstChildren := make(map[ContentHash][]ContentHash)
	for _, snap := range byID {
		if len(snap.Parents) > 0 && attached[snap.ID] {
			firstChildren[snap.Parents[0]] = append(firstChildren[snap.Parents[0]], snap.ID)
		}
	}
	for i, id := range m.Segment {
		expected := ContentHash("")
		if i+1 < len(m.Segment) {
			expected = m.Segment[i+1]
		}
		for _, child := range firstChildren[id] {
			if child != expected {
				return fmt.Errorf("%w: join segment changed or gained another attached first-parent child", ErrConflict)
			}
		}
	}
	return nil
}

// ValidateJoinGraphScope checks attachment and branch isolation against the locked graph.
func ValidateJoinGraphScope(m JoinMutation, byID map[ContentHash]Snapshot, refs []Ref) error {
	var targetRoots, otherRoots []ContentHash
	sessionPrefix := SessionRefPrefix(m.Branch)
	for _, ref := range refs {
		if ref.Target == "" {
			continue
		}
		switch ref.Kind {
		case RefBranch:
			if ref.Name == m.Branch {
				targetRoots = append(targetRoots, ref.Target)
			} else {
				otherRoots = append(otherRoots, ref.Target)
			}
		case RefSession:
			if strings.HasPrefix(ref.Name, sessionPrefix) {
				targetRoots = append(targetRoots, ref.Target)
			} else {
				otherRoots = append(otherRoots, ref.Target)
			}
		}
	}
	other := snapshotReachableSet(byID, otherRoots...)
	for _, id := range m.Segment {
		if other[id] {
			return fmt.Errorf("%w: snapshot %s is reachable from another branch", ErrConflict, id)
		}
	}
	for _, patch := range m.Grafts {
		if other[patch.SnapshotID] {
			return fmt.Errorf("%w: graft source %s is reachable from another branch", ErrConflict, patch.SnapshotID)
		}
	}
	return validateJoinSegmentTopology(m, byID, snapshotReachableSet(byID, targetRoots...))
}
