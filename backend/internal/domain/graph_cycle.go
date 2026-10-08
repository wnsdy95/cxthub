package domain

import (
	"context"
	"fmt"
)

// ValidateAcyclicAncestry checks the complete natural and overlay graph in
// O(nodes + edges). Missing ancestors terminate paths; completeness is a
// separate invariant. Disconnected components are checked as well.
func ValidateAcyclicAncestry(ctx context.Context, snapshots map[ContentHash]Snapshot) error {
	degrees := make(map[ContentHash]int, len(snapshots))
	children := make(map[ContentHash][]ContentHash, len(snapshots))
	ready := make([]ContentHash, 0, len(snapshots))
	for id, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i, parent := range snapshot.ReachabilityParents() {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if _, exists := snapshots[parent]; exists {
				degrees[id]++
				children[parent] = append(children[parent], id)
			}
		}
		if degrees[id] == 0 {
			ready = append(ready, id)
		}
	}
	visited := 0
	for len(ready) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		visited++
		for i, child := range children[id] {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			degrees[child]--
			if degrees[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if visited != len(snapshots) {
		return fmt.Errorf("%w: cyclic graph ancestry", ErrIntegrity)
	}
	return nil
}
