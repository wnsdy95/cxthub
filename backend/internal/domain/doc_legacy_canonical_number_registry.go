package domain

import (
	"math/bits"
	"unsafe"
)

const legacyCanonicalNumberBucketValues = 8
const legacyCanonicalNumberRadix = 16
const legacyCanonicalNumberWordNibbles = int(unsafe.Sizeof(uintptr(0))) * 2
const legacyCanonicalNumberDigits = 2 * legacyCanonicalNumberWordNibbles
const legacyCanonicalNumberLookupNodeBytes = int(unsafe.Sizeof((*legacyCanonicalNumberRegistryNode)(nil))) +
	int(unsafe.Sizeof(int(0))) + int(unsafe.Sizeof(true))
const legacyCanonicalNumberProofBytes = int(unsafe.Sizeof(legacyCanonicalNumberRegistry{})) +
	(legacyCanonicalNumberDigits+1)*legacyCanonicalNumberLookupNodeBytes +
	legacyCanonicalNumberBucketValues*int(unsafe.Sizeof(legacyCanonicalNumberEntry{})) + int(unsafe.Sizeof(true))

type legacyCanonicalNumberEntry struct {
	identity legacyCanonicalNumberIdentity
	ordinal  int
	proven   bool
}

type legacyCanonicalNumberRegistryNode struct {
	children [legacyCanonicalNumberRadix]*legacyCanonicalNumberRegistryNode
	entries  [legacyCanonicalNumberBucketValues]legacyCanonicalNumberEntry
	used     int
	branched bool
}

type legacyCanonicalNumberRegistry struct {
	root  *legacyCanonicalNumberRegistryNode
	count int
	nodes int
}

func legacyCanonicalNumberDigit(identity legacyCanonicalNumberIdentity, depth int) int {
	word := uintptr(unsafe.Pointer(identity.start))
	if depth >= legacyCanonicalNumberWordNibbles {
		word = uintptr(identity.size)
		depth -= legacyCanonicalNumberWordNibbles
	}
	return int(word >> (4 * (legacyCanonicalNumberWordNibbles - depth - 1)) & (legacyCanonicalNumberRadix - 1))
}

func (registry *legacyCanonicalNumberRegistry) length() int {
	return registry.count
}

func (registry *legacyCanonicalNumberRegistry) valid() bool {
	if registry.root == nil {
		return registry.count == 0 && registry.nodes == 0
	}
	return registry.count > 0 && registry.nodes > 0 && (registry.count-1)/legacyCanonicalNumberBucketValues < registry.nodes
}

func (registry *legacyCanonicalNumberRegistry) leaf(identity legacyCanonicalNumberIdentity) (*legacyCanonicalNumberRegistryNode, int, error) {
	if !registry.valid() || identity.size < 0 || identity.size > 0 && identity.start == nil {
		return nil, 0, ErrIntegrity
	}
	node, depth := registry.root, 0
	for node != nil {
		if !node.branched {
			if node.used <= 0 || node.used > legacyCanonicalNumberBucketValues {
				return nil, 0, ErrIntegrity
			}
			return node, depth, nil
		}
		if node.used != 0 || depth == legacyCanonicalNumberDigits {
			return nil, 0, ErrIntegrity
		}
		node = node.children[legacyCanonicalNumberDigit(identity, depth)]
		depth++
	}
	return nil, depth, nil
}

func (registry *legacyCanonicalNumberRegistry) get(identity legacyCanonicalNumberIdentity) (bool, bool) {
	entry, err := registry.lookup(identity)
	if err != nil {
		return false, false
	}
	return entry.proven, true
}

func (registry *legacyCanonicalNumberRegistry) lookup(identity legacyCanonicalNumberIdentity) (*legacyCanonicalNumberEntry, error) {
	entry, err := registry.lookupOptional(identity)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, ErrIntegrity
	}
	return entry, nil
}

func (registry *legacyCanonicalNumberRegistry) lookupOptional(identity legacyCanonicalNumberIdentity) (*legacyCanonicalNumberEntry, error) {
	node, _, err := registry.leaf(identity)
	if err != nil {
		return nil, err
	}
	if node != nil {
		for position := 0; position < node.used; position++ {
			entry := &node.entries[position]
			if entry.identity == identity {
				return entry, nil
			}
		}
	}
	return nil, nil
}

func (registry *legacyCanonicalNumberRegistry) plan(identity legacyCanonicalNumberIdentity) (int, bool, error) {
	node, depth, err := registry.leaf(identity)
	if err != nil {
		return 0, false, err
	}
	lookupBytes := int64(unsafe.Sizeof(*registry)) + int64(depth+1)*int64(legacyCanonicalNumberLookupNodeBytes)
	if node != nil {
		for position := 0; position < node.used; position++ {
			lookupBytes += int64(unsafe.Sizeof(legacyCanonicalNumberEntry{}))
			if node.entries[position].identity == identity {
				return int(2 * lookupBytes), true, nil
			}
		}
	}
	if registry.count == int(^uint(0)>>1) {
		return 0, false, ErrIntegrity
	}

	added, splits := 0, 0
	if node == nil {
		added = 1
	} else if node.used == legacyCanonicalNumberBucketValues {
		for {
			if depth == legacyCanonicalNumberDigits {
				return 0, false, ErrIntegrity
			}
			var occupied uint16
			for position := range node.entries {
				occupied |= uint16(1) << legacyCanonicalNumberDigit(node.entries[position].identity, depth)
			}
			lookupBytes += int64(unsafe.Sizeof(legacyCanonicalNumberEntry{})) * legacyCanonicalNumberBucketValues
			children := bits.OnesCount16(occupied)
			added += children
			splits++
			if occupied&(uint16(1)<<legacyCanonicalNumberDigit(identity, depth)) == 0 {
				added++
				break
			}
			if children > 1 {
				break
			}
			depth++
		}
	}
	if added > int(^uint(0)>>1)-registry.nodes {
		return 0, false, ErrIntegrity
	}
	var retained docRangeHeaderHeapEstimate
	legacyCanonicalStorageAllocation(&retained, registry.nodes+added, int64(unsafe.Sizeof(legacyCanonicalNumberRegistryNode{})))
	if retained.err != nil {
		return 0, false, retained.err
	}
	pathNodes := depth + 1
	if splits != 0 {
		pathNodes++
	}
	work := int64(added)*int64(unsafe.Sizeof(legacyCanonicalNumberRegistryNode{})) +
		2*int64(splits)*int64(unsafe.Sizeof([legacyCanonicalNumberBucketValues]legacyCanonicalNumberEntry{})) +
		int64(unsafe.Sizeof(legacyCanonicalNumberEntry{})) + 2*lookupBytes +
		int64(unsafe.Sizeof(*registry)) + int64(pathNodes)*int64(legacyCanonicalNumberLookupNodeBytes)
	if work <= 0 || work > LegacyCanonicalEncodingBytes {
		return 0, false, ErrIntegrity
	}
	return int(work), false, nil
}

func (registry *legacyCanonicalNumberRegistry) insertBytes(identity legacyCanonicalNumberIdentity) (int, error) {
	work, _, err := registry.plan(identity)
	return work, err
}

func (registry *legacyCanonicalNumberRegistry) insert(identity legacyCanonicalNumberIdentity) (bool, error) {
	_, exists, err := registry.plan(identity)
	if err != nil || exists {
		return false, err
	}
	registry.insertAdmitted(identity)
	return true, nil
}

func (registry *legacyCanonicalNumberRegistry) insertAdmitted(identity legacyCanonicalNumberIdentity) {
	registry.insertIndexed(identity, registry.count)
}

func (registry *legacyCanonicalNumberRegistry) insertIndexed(identity legacyCanonicalNumberIdentity, ordinal int) {
	link, depth := &registry.root, 0
	for {
		if *link == nil {
			*link = &legacyCanonicalNumberRegistryNode{}
			registry.nodes++
		}
		node := *link
		if !node.branched {
			if node.used < legacyCanonicalNumberBucketValues {
				node.entries[node.used] = legacyCanonicalNumberEntry{identity: identity, ordinal: ordinal}
				node.used++
				registry.count++
				return
			}
			for position := range node.entries {
				entry := &node.entries[position]
				branch := legacyCanonicalNumberDigit(entry.identity, depth)
				if node.children[branch] == nil {
					node.children[branch] = &legacyCanonicalNumberRegistryNode{}
					registry.nodes++
				}
				child := node.children[branch]
				child.entries[child.used] = *entry
				child.used++
			}
			clear(node.entries[:])
			node.used, node.branched = 0, true
		}
		link = &node.children[legacyCanonicalNumberDigit(identity, depth)]
		depth++
	}
}

func (registry *legacyCanonicalNumberRegistry) prove(identity legacyCanonicalNumberIdentity) error {
	node, _, err := registry.leaf(identity)
	if err != nil {
		return err
	}
	if node != nil {
		for position := 0; position < node.used; position++ {
			if node.entries[position].identity == identity {
				node.entries[position].proven = true
				return nil
			}
		}
	}
	return ErrIntegrity
}

func (registry *legacyCanonicalNumberRegistry) clear() {
	*registry = legacyCanonicalNumberRegistry{}
}

func (registry *legacyCanonicalNumberRegistry) retained(heap *docRangeHeaderHeapEstimate) {
	if !registry.valid() {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	legacyCanonicalStorageAllocation(heap, registry.nodes, int64(unsafe.Sizeof(legacyCanonicalNumberRegistryNode{})))
}
