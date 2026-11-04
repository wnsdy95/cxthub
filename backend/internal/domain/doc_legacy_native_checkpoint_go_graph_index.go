package domain

import (
	"context"
	"math/bits"
	"reflect"
	"unsafe"
)

const legacyNativeCheckpointGoGraphKeyBits = 3*bits.UintSize + 8
const legacyNativeCheckpointGoGraphIndexActions = legacyNativeCheckpointPageItems / 2

type legacyNativeCheckpointGoGraphLink struct {
	branch *legacyNativeCheckpointGoGraphLookup
	entry  *legacyNativeCheckpointGoGraphEntry
}

type legacyNativeCheckpointGoGraphLookup struct {
	children [2]legacyNativeCheckpointGoGraphLink
	bit      int
}

func (link legacyNativeCheckpointGoGraphLink) empty() bool {
	return link.branch == nil && link.entry == nil
}

func (key legacyNativeCheckpointGoGraphKey) bit(position int) uintptr {
	words := [4]uintptr{key.address, uintptr(key.length), uintptr(key.capacity), uintptr(key.kind)}
	return (words[position/bits.UintSize] >> uint(position%bits.UintSize)) & 1
}

func (key legacyNativeCheckpointGoGraphKey) difference(other legacyNativeCheckpointGoGraphKey) int {
	words := [4]uintptr{key.address ^ other.address, uintptr(key.length ^ other.length), uintptr(key.capacity ^ other.capacity), uintptr(key.kind ^ other.kind)}
	for position := len(words) - 1; position >= 0; position-- {
		if words[position] != 0 {
			return position*bits.UintSize + bits.Len(uint(words[position])) - 1
		}
	}
	return -1
}

func (link legacyNativeCheckpointGoGraphLink) valid(limit int) bool {
	if link.branch == nil {
		return link.entry != nil
	}
	return link.entry == nil && link.branch.bit >= 0 && link.branch.bit < limit &&
		!link.branch.children[0].empty() && !link.branch.children[1].empty()
}

func (registry *legacyNativeCheckpointGoGraphRegistry) lookup(key legacyNativeCheckpointGoGraphKey) (*legacyNativeCheckpointGoGraphEntry, error) {
	link, limit := registry.root, legacyNativeCheckpointGoGraphKeyBits
	if link.empty() && registry.entries == 0 && registry.nodes == 0 {
		return nil, nil
	}
	for {
		if !link.valid(limit) {
			return nil, ErrIntegrity
		}
		if link.branch == nil {
			return link.entry, nil
		}
		limit = link.branch.bit
		link = link.branch.children[key.bit(limit)]
	}
}

func (work *legacyNativeCheckpointGoGraph) index(ctx context.Context, frame *legacyNativeCheckpointGoGraphFrame) error {
	if frame.indexNode == nil || frame.entry == nil || frame.entry.key != frame.key ||
		frame.indexLevel < -1 || frame.indexLevel >= legacyNativeCheckpointGoGraphKeyBits ||
		frame.indexLimit <= frame.indexLevel || frame.indexLimit > legacyNativeCheckpointGoGraphKeyBits {
		return ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	indexed := false
	for checked := 0; checked < legacyNativeCheckpointGoGraphIndexActions; checked++ {
		link := frame.indexNode
		if !link.valid(frame.indexLimit) {
			return ErrIntegrity
		}
		if frame.indexLevel == -1 {
			if link != &work.registry.root || link.branch != nil || link.entry != frame.entry || work.registry.entries != 1 || work.registry.nodes != 0 {
				return ErrIntegrity
			}
			indexed = true
			break
		}
		if link.branch != nil && link.branch.bit >= frame.indexLevel {
			if link.branch.bit == frame.indexLevel {
				return ErrIntegrity
			}
			frame.indexLimit = link.branch.bit
			frame.indexNode = &link.branch.children[frame.key.bit(frame.indexLimit)]
			continue
		}
		position := frame.key.bit(frame.indexLevel)
		if link.entry != nil && link.entry.key.bit(frame.indexLevel) == position {
			return ErrIntegrity
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphLookup{})))
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes, 0); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		branch := &legacyNativeCheckpointGoGraphLookup{bit: frame.indexLevel}
		branch.children[position].entry = frame.entry
		branch.children[1-position] = *link
		*link = legacyNativeCheckpointGoGraphLink{branch: branch}
		work.registry.nodes++
		indexed = true
		break
	}
	if !indexed {
		return nil
	}
	frame.indexNode = nil
	frame.phase = legacyNativeCheckpointGoGraphNext
	if frame.entry.object != nil {
		frame.iterator = reflect.ValueOf(frame.entry.source).MapRange()
	}
	return nil
}
