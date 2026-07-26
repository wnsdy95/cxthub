package store

import (
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// validateJoinMutationPlan revalidates the join plan computed by the service at the storage port boundary to ensure it does not lose the previous head or remaining session branches.
func validateJoinMutationPlan(m outbound.JoinMutation) error {
	if m.Branch == domain.HeadRefName {
		return fmt.Errorf("%w: HEAD is not a joinable branch", domain.ErrValidation)
	}
	if len(m.Segment) == 0 || m.Segment[0] != m.Source {
		return fmt.Errorf("%w: join segment must start at source", domain.ErrValidation)
	}
	tip := m.Segment[len(m.Segment)-1]
	switch {
	case len(m.Segment) == 1:
		if m.NewHead != m.Source || m.ForkName != "" || m.ForkTip != "" {
			return fmt.Errorf("%w: single-snapshot join cannot create a residual session ref", domain.ErrValidation)
		}
	case m.NewHead == tip:
		if m.ForkName != "" || m.ForkTip != "" {
			return fmt.Errorf("%w: whole-segment join cannot create a residual session ref", domain.ErrValidation)
		}
	case m.NewHead == m.Source:
		if m.ForkName == "" || m.ForkTip != tip {
			return fmt.Errorf("%w: partial join must preserve the segment tip in a session ref", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: join head must be the source or segment tip", domain.ErrValidation)
	}
	if m.ForkName != "" && !strings.HasPrefix(m.ForkName, domain.SessionRefPrefix(m.Branch)) {
		return fmt.Errorf("%w: join session ref is not scoped to git branch %q", domain.ErrValidation, m.Branch)
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
			return fmt.Errorf("%w: source graft patch does not preserve the previous branch head", domain.ErrValidation)
		}
	}
	if !sourcePatch {
		return fmt.Errorf("%w: join requires a source graft patch", domain.ErrValidation)
	}
	return nil
}

// validateJoinSegmentTopology checks that, between service queries and storage application, the target branch/session ref has not changed and X…tip remains the same single first-parent branch. Objects are uploaded but children not yet attached to any ref are excluded from the public graph.
func validateJoinSegmentTopology(
	m outbound.JoinMutation,
	byID map[domain.ContentHash]domain.Snapshot,
	attached map[domain.ContentHash]bool,
) error {
	for i, id := range m.Segment {
		snap, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: join segment snapshot %s", domain.ErrNotFound, id)
		}
		if !attached[id] {
			return fmt.Errorf("%w: join segment snapshot %s is no longer attached to its branch/session ref", domain.ErrConflict, id)
		}
		if i > 0 && (len(snap.Parents) == 0 || snap.Parents[0] != m.Segment[i-1]) {
			return fmt.Errorf("%w: join segment is no longer a first-parent chain", domain.ErrConflict)
		}
	}
	for _, patch := range m.Grafts {
		if !attached[patch.SnapshotID] {
			return fmt.Errorf("%w: graft patch snapshot %s is outside the target git branch scope", domain.ErrConflict, patch.SnapshotID)
		}
		for _, parent := range patch.Parents {
			if !attached[parent] {
				return fmt.Errorf("%w: graft parent %s is outside the target git branch scope", domain.ErrConflict, parent)
			}
		}
	}

	firstChildren := make(map[domain.ContentHash][]domain.ContentHash)
	for _, snap := range byID {
		if len(snap.Parents) > 0 && attached[snap.ID] {
			firstChildren[snap.Parents[0]] = append(firstChildren[snap.Parents[0]], snap.ID)
		}
	}
	for i, id := range m.Segment {
		expected := domain.ContentHash("")
		if i+1 < len(m.Segment) {
			expected = m.Segment[i+1]
		}
		for _, child := range firstChildren[id] {
			if child != expected {
				return fmt.Errorf("%w: join segment changed or gained another attached first-parent child", domain.ErrConflict)
			}
		}
	}
	return nil
}
