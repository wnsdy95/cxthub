package domain

import "unsafe"

func legacyNativeCheckpointGoGraphRangeHeight(entry *legacyNativeCheckpointGoGraphEntry) int {
	if entry == nil {
		return 0
	}
	return entry.rangeHeight
}

func (entry *legacyNativeCheckpointGoGraphEntry) updateRangeHeight() {
	entry.rangeHeight = 1 + max(legacyNativeCheckpointGoGraphRangeHeight(entry.rangeLeft), legacyNativeCheckpointGoGraphRangeHeight(entry.rangeRight))
}

func (registry *legacyNativeCheckpointGoGraphRegistry) replaceRange(previous, next *legacyNativeCheckpointGoGraphEntry) error {
	parent := previous.rangeParent
	if parent == nil {
		if registry.ranges != previous {
			return ErrIntegrity
		}
		registry.ranges = next
	} else if parent.rangeLeft == previous {
		parent.rangeLeft = next
	} else if parent.rangeRight == previous {
		parent.rangeRight = next
	} else {
		return ErrIntegrity
	}
	if next != nil {
		next.rangeParent = parent
	}
	return nil
}

func (registry *legacyNativeCheckpointGoGraphRegistry) rotateRange(entry *legacyNativeCheckpointGoGraphEntry, left bool) (*legacyNativeCheckpointGoGraphEntry, error) {
	child, middle := entry.rangeRight, (*legacyNativeCheckpointGoGraphEntry)(nil)
	if !left {
		child = entry.rangeLeft
	}
	if child == nil || child.rangeParent != entry {
		return nil, ErrIntegrity
	}
	if left {
		middle = child.rangeLeft
	} else {
		middle = child.rangeRight
	}
	if err := registry.replaceRange(entry, child); err != nil {
		return nil, err
	}
	if left {
		entry.rangeRight, child.rangeLeft = middle, entry
	} else {
		entry.rangeLeft, child.rangeRight = middle, entry
	}
	entry.rangeParent = child
	if middle != nil {
		middle.rangeParent = entry
	}
	entry.updateRangeHeight()
	child.updateRangeHeight()
	return child, nil
}

func (registry *legacyNativeCheckpointGoGraphRegistry) insertRange(entry, parent *legacyNativeCheckpointGoGraphEntry) error {
	if entry.key.kind != legacyNativeCheckpointArray || entry.key.capacity <= 0 || entry.rangeHeight != 0 || entry.rangeParent != nil || entry.rangeLeft != nil || entry.rangeRight != nil {
		return ErrIntegrity
	}
	const width = uintptr(unsafe.Sizeof(any(nil)))
	if parent == nil {
		if registry.ranges != nil {
			return ErrIntegrity
		}
		registry.ranges = entry
	} else if entry.key.address+uintptr(entry.key.capacity)*width <= parent.key.address {
		if parent.rangeLeft != nil {
			return ErrIntegrity
		}
		parent.rangeLeft = entry
	} else if entry.key.address >= parent.key.address+uintptr(parent.key.capacity)*width {
		if parent.rangeRight != nil {
			return ErrIntegrity
		}
		parent.rangeRight = entry
	} else {
		return ErrIntegrity
	}
	entry.rangeParent, entry.rangeHeight = parent, 1
	return nil
}

func (work *legacyNativeCheckpointGoGraph) balanceRanges(frame *legacyNativeCheckpointGoGraphFrame) error {
	for checked := 0; checked < legacyNativeCheckpointPageItems/8 && frame.rangeParent != nil; checked++ {
		entry := frame.rangeParent
		previousHeight := entry.rangeHeight
		entry.updateRangeHeight()
		balance := legacyNativeCheckpointGoGraphRangeHeight(entry.rangeLeft) - legacyNativeCheckpointGoGraphRangeHeight(entry.rangeRight)
		if balance < -2 || balance > 2 {
			return ErrIntegrity
		}
		if balance == 2 {
			if legacyNativeCheckpointGoGraphRangeHeight(entry.rangeLeft.rangeLeft) < legacyNativeCheckpointGoGraphRangeHeight(entry.rangeLeft.rangeRight) {
				if _, err := work.registry.rotateRange(entry.rangeLeft, true); err != nil {
					return err
				}
			}
			rotated, err := work.registry.rotateRange(entry, false)
			if err != nil {
				return err
			}
			entry = rotated
		} else if balance == -2 {
			if legacyNativeCheckpointGoGraphRangeHeight(entry.rangeRight.rangeRight) < legacyNativeCheckpointGoGraphRangeHeight(entry.rangeRight.rangeLeft) {
				if _, err := work.registry.rotateRange(entry.rangeRight, false); err != nil {
					return err
				}
			}
			rotated, err := work.registry.rotateRange(entry, true)
			if err != nil {
				return err
			}
			entry = rotated
		}
		frame.rangeParent = entry.rangeParent
		if entry.rangeHeight == previousHeight {
			frame.rangeParent = nil
		}
	}
	if frame.rangeParent == nil {
		frame.phase = legacyNativeCheckpointGoGraphIndex
	}
	return nil
}
