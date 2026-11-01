package domain

import (
	"context"
	"math"
	"strings"
	"unsafe"
)

type legacyNativeCheckpointGraphRestoredEntry struct {
	wire        [legacyNativeValueCheckpointBytes]byte
	body        legacyNativeCheckpointRef
	identity    uint64
	value       legacyNativeAnyValue
	object      *legacyNativeAnyMap
	array       *legacyNativeAnyArray
	owned       int64
	descendants int64
	active      bool
}

type legacyNativeCheckpointGraphRestoredLookup struct {
	children [16]*legacyNativeCheckpointGraphRestoredLookup
	entry    *legacyNativeCheckpointGraphRestoredEntry
}

type legacyNativeCheckpointGraphRestored struct {
	root    *legacyNativeCheckpointGraphRestoredLookup
	nodes   int64
	owners  int64
	owned   int64
	closed  bool
	failure error
}

func newLegacyNativeCheckpointGraphRestored() *legacyNativeCheckpointGraphRestored {
	return &legacyNativeCheckpointGraphRestored{}
}

func (registry *legacyNativeCheckpointGraphRestored) metadataBytes(nodes, owners int64) (int64, error) {
	if nodes < 0 || owners < 0 || uint64(owners) > legacyNativeCheckpointNodes || nodes > 17*owners {
		return 0, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestored{})))
	legacyCanonicalStorageAllocation(&heap, int(nodes), int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestoredLookup{})))
	var entry docRangeHeaderHeapEstimate
	entry.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestoredEntry{})))
	entry.allocation(int64(len(contentHashPrefix)+64), 1)
	if entry.err != nil || owners > math.MaxInt64/entry.bytes {
		return 0, ErrIntegrity
	}
	heap.include(docRangeHeaderHeapEstimate{bytes: owners * entry.bytes})
	return heap.bytes, heap.err
}

func (registry *legacyNativeCheckpointGraphRestored) retainedBytes() (int64, error) {
	if registry == nil || registry.closed {
		return 0, nil
	}
	if registry.owned < 0 || (registry.root == nil) != (registry.owners == 0) {
		return 0, ErrIntegrity
	}
	metadata, err := registry.metadataBytes(registry.nodes, registry.owners)
	if err != nil {
		return 0, err
	}
	heap := docRangeHeaderHeapEstimate{bytes: metadata}
	heap.include(docRangeHeaderHeapEstimate{bytes: registry.owned})
	return heap.bytes, heap.err
}

func (registry *legacyNativeCheckpointGraphRestored) close() {
	if registry != nil {
		failure := registry.failure
		*registry = legacyNativeCheckpointGraphRestored{closed: true, failure: failure}
	}
}

func (registry *legacyNativeCheckpointGraphRestored) fail(err error) error {
	if registry != nil {
		if registry.failure != nil {
			return registry.failure
		}
		registry.close()
		registry.failure = err
	}
	return err
}

func (registry *legacyNativeCheckpointGraphRestored) lookup(identity uint64) (*legacyNativeCheckpointGraphRestoredEntry, int) {
	node := registry.root
	if node == nil {
		return nil, 17
	}
	for level := 15; level >= 0; level-- {
		node = node.children[(identity>>uint(4*level))&15]
		if node == nil {
			return nil, level + 1
		}
	}
	return node.entry, 0
}

const (
	legacyNativeCheckpointGraphRestoreStart uint8 = iota
	legacyNativeCheckpointGraphRestoreBody
	legacyNativeCheckpointGraphRestoreMembers
	legacyNativeCheckpointGraphRestoreKey
	legacyNativeCheckpointGraphRestoreWaitingKey
	legacyNativeCheckpointGraphRestoreValue
	legacyNativeCheckpointGraphRestoreWaitingValue
	legacyNativeCheckpointGraphRestoreAttach
	legacyNativeCheckpointGraphRestoreClearDirty
	legacyNativeCheckpointGraphRestoreDirty
	legacyNativeCheckpointGraphRestoreFinish
	legacyNativeCheckpointGraphRestoreText
)

type legacyNativeCheckpointGraphRestoreFrame struct {
	parent    *legacyNativeCheckpointGraphRestoreFrame
	saved     legacyNativeCheckpointValue
	state     legacyNativeValueCheckpoint
	record    legacyNativeCheckpointOwner
	entry     *legacyNativeCheckpointGraphRestoredEntry
	members   *legacyNativeCheckpointListReader[legacyNativeCheckpointMember]
	dirty     *legacyNativeCheckpointListReader[uint32]
	member    legacyNativeCheckpointMember
	key       legacyNativeAnyValue
	pending   legacyNativeAnyValue
	position  uint64
	cleared   uint64
	dirtyRead uint64
	workspace int64
	phase     uint8
}

type legacyNativeCheckpointGraphRestore struct {
	sharedField *legacyNativeObjectAnyCheckpointRestore
	sharedAny   *legacyNativeAnyCheckpointRestore
	io          legacyNativeCheckpointIO
	registry    *legacyNativeCheckpointGraphRestored
	frame       *legacyNativeCheckpointGraphRestoreFrame
	text        *legacyNativeCheckpointScalarRestore
	result      legacyNativeAnyValue
	workspace   int64
	depth       int
	budget      int64
	ready       bool
	closed      bool
	failure     error
}

func legacyNativeCheckpointGraphRestoreDescriptor(ctx context.Context, saved legacyNativeCheckpointValue) (legacyNativeValueCheckpoint, error) {
	if saved.Text.Bytes < 0 || uint64(saved.Text.Bytes) > legacyNativeValueCheckpointPayloadLimit() {
		return legacyNativeValueCheckpoint{}, ErrIntegrity
	}
	state, err := decodeLegacyNativeValueCheckpoint(ctx, saved.Wire, legacyNativeValueCheckpointBounds{
		PayloadBytes: uint64(saved.Text.Bytes), Nodes: legacyNativeCheckpointNodes,
	})
	if err != nil {
		return legacyNativeValueCheckpoint{}, err
	}
	switch state.Kind {
	case legacyNativeCheckpointString:
		if saved.Body != (legacyNativeCheckpointRef{}) || state.Reference != 0 || state.Length != uint64(saved.Text.Bytes) ||
			!saved.Text.Tree.valid() || saved.Text.Tree.Count != state.Length ||
			(state.Length == 0) != (saved.Text.Tree == (legacyNativeCheckpointTree{})) {
			return legacyNativeValueCheckpoint{}, ErrIntegrity
		}
	case legacyNativeCheckpointArray, legacyNativeCheckpointObject:
		if saved.Text != (legacyNativeCheckpointText{}) ||
			state.Nil && saved.Body != (legacyNativeCheckpointRef{}) ||
			!state.Nil && (saved.Body == (legacyNativeCheckpointRef{}) || !saved.Body.valid()) {
			return legacyNativeValueCheckpoint{}, ErrIntegrity
		}
	default:
		if saved.Body != (legacyNativeCheckpointRef{}) || saved.Text != (legacyNativeCheckpointText{}) {
			return legacyNativeValueCheckpoint{}, ErrIntegrity
		}
	}
	return state, nil
}

func legacyNativeCheckpointGraphRestoreSavedHeap(saved legacyNativeCheckpointValue) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(cap(saved.Wire)), 1)
	heap.allocation(int64(len(saved.Body.Hash)), 1)
	heap.allocation(int64(len(saved.Text.Tree.Ref.Hash)), 1)
	return heap
}

func legacyNativeCheckpointGraphRestorePendingHeap(value legacyNativeAnyValue) docRangeHeaderHeapEstimate {
	if value.owner.object != nil || value.owner.array != nil {
		return docRangeHeaderHeapEstimate{}
	}
	return value.heap
}

func newLegacyNativeCheckpointGraphRestore(ctx context.Context, saved legacyNativeCheckpointValue, io legacyNativeCheckpointIO, registry *legacyNativeCheckpointGraphRestored, heapBudget int64) (*legacyNativeCheckpointGraphRestore, error) {
	return newLegacyNativeCheckpointGraphRestoreInField(ctx, saved, io, registry, heapBudget, nil, nil)
}

func newLegacyNativeCheckpointGraphRestoreInField(ctx context.Context, saved legacyNativeCheckpointValue, io legacyNativeCheckpointIO, registry *legacyNativeCheckpointGraphRestored, heapBudget int64, field *legacyNativeObjectAnyCheckpointRestore, nested *legacyNativeAnyCheckpointRestore) (result *legacyNativeCheckpointGraphRestore, failure error) {
	var registered *legacyNativeCheckpointGraphRestore
	defer func() {
		if failure != nil && field != nil {
			if nested != nil && nested.graph == registered {
				nested.graph = nil
			} else if nested == nil && field.graph == registered {
				field.graph = nil
			}
		}
	}()
	if field != nil {
		parent, err := field.sharedRestoreRoot()
		if err != nil {
			return nil, err
		}
		if parent.anyGraph != registry || nested != nil && (field.child != nested || nested.sharedField != field || nested.graph != nil) || nested == nil && field.graph != nil {
			return nil, ErrConflict
		}
		var own docRangeHeaderHeapEstimate
		own.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestore{})))
		if own.err != nil {
			return nil, own.err
		}
		if err := parent.admit(own.bytes); err != nil {
			return nil, err
		}
		io = parent.store
	}
	if registry != nil && registry.failure != nil {
		return nil, registry.failure
	}
	if err := ctx.Err(); err != nil {
		return nil, registry.fail(err)
	}
	if registry == nil || registry.closed || heapBudget <= 0 {
		return nil, registry.fail(ErrIntegrity)
	}
	work := &legacyNativeCheckpointGraphRestore{sharedField: field, sharedAny: nested, io: io, registry: registry, budget: heapBudget}
	if field != nil {
		registered = work
		if nested != nil {
			nested.graph = work
		} else {
			field.graph = work
		}
	}
	if err := work.push(ctx, saved); err != nil {
		_, failure := work.fail(err)
		return nil, failure
	}
	return work, nil
}

func (work *legacyNativeCheckpointGraphRestore) admit(additional, released int64) error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
	if work.registry != nil && work.registry.failure != nil {
		return work.registry.failure
	}
	if work.registry == nil || work.registry.closed || additional < 0 || released < 0 {
		return ErrIntegrity
	}
	workspace, err := work.retainedBytes()
	if err != nil {
		return err
	}
	registered, err := work.registry.retainedBytes()
	if err != nil {
		return err
	}
	heap := docRangeHeaderHeapEstimate{bytes: workspace}
	heap.include(docRangeHeaderHeapEstimate{bytes: registered})
	if heap.err != nil {
		return heap.err
	}
	if released > heap.bytes {
		return ErrIntegrity
	}
	heap.bytes -= released
	heap.include(docRangeHeaderHeapEstimate{bytes: additional})
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointGraphRestore) available() (int64, error) {
	if err := work.admit(0, 0); err != nil {
		return 0, err
	}
	workspace, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	registered, err := work.registry.retainedBytes()
	if err != nil {
		return 0, err
	}
	remaining := work.budget - workspace - registered
	if remaining <= 0 {
		return 0, ErrDocumentResourceLimit
	}
	return remaining, nil
}

func (work *legacyNativeCheckpointGraphRestore) push(ctx context.Context, saved legacyNativeCheckpointValue) error {
	state, err := legacyNativeCheckpointGraphRestoreDescriptor(ctx, saved)
	if err != nil {
		return err
	}
	if work.depth >= legacyNativeHeapMaxDepth {
		return ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestoreFrame{})))
	planned.allocation(int64(len(saved.Wire)), 1)
	planned.allocation(int64(len(saved.Body.Hash)), 1)
	planned.allocation(int64(len(saved.Text.Tree.Ref.Hash)), 1)
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes, 0); err != nil {
		return err
	}
	saved.Wire = append([]byte(nil), saved.Wire...)
	saved.Body.Hash = ContentHash(strings.Clone(string(saved.Body.Hash)))
	saved.Text.Tree.Ref.Hash = ContentHash(strings.Clone(string(saved.Text.Tree.Ref.Hash)))
	frame := &legacyNativeCheckpointGraphRestoreFrame{parent: work.frame, saved: saved, state: state}
	work.frame, work.depth = frame, work.depth+1
	return work.refresh(frame)
}

func (work *legacyNativeCheckpointGraphRestore) refresh(frame *legacyNativeCheckpointGraphRestoreFrame) error {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*frame)))
	heap.include(legacyNativeCheckpointGraphRestoreSavedHeap(frame.saved))
	heap.include(legacyNativeCheckpointGraphRestoreSavedHeap(frame.member.Key))
	heap.include(legacyNativeCheckpointGraphRestoreSavedHeap(frame.member.Value))
	heap.allocation(int64(len(frame.record.Members.Ref.Hash)), 1)
	heap.allocation(int64(len(frame.record.Dirty.Ref.Hash)), 1)
	heap.include(legacyNativeCheckpointGraphRestorePendingHeap(frame.key))
	heap.include(legacyNativeCheckpointGraphRestorePendingHeap(frame.pending))
	if frame.members != nil {
		bytes, err := frame.members.retainedBytes()
		if err != nil {
			return err
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes})
	}
	if frame.dirty != nil {
		bytes, err := frame.dirty.retainedBytes()
		if err != nil {
			return err
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes})
	}
	if heap.err != nil || work.workspace < frame.workspace {
		return ErrIntegrity
	}
	total := docRangeHeaderHeapEstimate{bytes: work.workspace - frame.workspace}
	total.include(heap)
	if total.err != nil {
		return total.err
	}
	work.workspace, frame.workspace = total.bytes, heap.bytes
	return nil
}

func (work *legacyNativeCheckpointGraphRestore) begin(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	state := frame.state
	switch state.Kind {
	case legacyNativeCheckpointString:
		remaining, err := work.available()
		if err != nil {
			return err
		}
		scalar := legacyNativeCheckpointScalar{
			State: legacyNativeScalarCheckpoint{Version: 1, Maximum: int(legacyNativeValueCheckpointPayloadLimit()),
				Bytes: int(state.Length), Capacity: int(state.Capacity), Present: true},
			Text: frame.saved.Text,
		}
		work.text, err = newLegacyNativeCheckpointScalarRestore(ctx, scalar, work.io, 0, remaining)
		if err != nil {
			return err
		}
		frame.saved, frame.phase = legacyNativeCheckpointValue{}, legacyNativeCheckpointGraphRestoreText
		return work.refresh(frame)
	case legacyNativeCheckpointArray, legacyNativeCheckpointObject:
		if state.Nil {
			var value legacyNativeAnyValue
			if state.Kind == legacyNativeCheckpointArray {
				value.heap.allocation(1, int64(unsafe.Sizeof([]any(nil))))
			} else {
				value.heap.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
			}
			if err := work.admit(value.heap.bytes, 0); err != nil {
				return err
			}
			if state.Kind == legacyNativeCheckpointArray {
				value.value = []any(nil)
			} else {
				value.value = map[string]any(nil)
			}
			return work.complete(frame, value)
		}
		entry, missing := work.registry.lookup(state.Reference)
		if missing == 0 {
			var wire [legacyNativeValueCheckpointBytes]byte
			copy(wire[:], frame.saved.Wire)
			if entry == nil || entry.active || entry.wire != wire || entry.body != frame.saved.Body {
				return ErrIntegrity
			}
			if err := entry.value.validateOwnership(); err != nil {
				return err
			}
			return work.complete(frame, entry.value)
		}
		current, err := work.registry.metadataBytes(work.registry.nodes, work.registry.owners)
		if err != nil {
			return err
		}
		future, err := work.registry.metadataBytes(work.registry.nodes+int64(missing), work.registry.owners+1)
		if err != nil {
			return err
		}
		if err := work.admit(future, current); err != nil {
			return err
		}
		entry = &legacyNativeCheckpointGraphRestoredEntry{body: frame.saved.Body, identity: state.Reference, active: true}
		copy(entry.wire[:], frame.saved.Wire)
		if work.registry.root == nil {
			work.registry.root = &legacyNativeCheckpointGraphRestoredLookup{}
			work.registry.nodes++
		}
		node := work.registry.root
		for level := 15; level >= 0; level-- {
			position := (state.Reference >> uint(4*level)) & 15
			if node.children[position] == nil {
				node.children[position] = &legacyNativeCheckpointGraphRestoredLookup{}
				work.registry.nodes++
			}
			node = node.children[position]
		}
		node.entry = entry
		work.registry.owners++
		frame.entry, frame.saved, frame.phase = entry, legacyNativeCheckpointValue{}, legacyNativeCheckpointGraphRestoreBody
		return work.refresh(frame)
	default:
		var value legacyNativeAnyValue
		switch state.Kind {
		case legacyNativeCheckpointNull:
		case legacyNativeCheckpointBool:
			value.heap.allocation(1, int64(unsafe.Sizeof(false)))
		case legacyNativeCheckpointFloat64:
			value.heap.allocation(1, int64(unsafe.Sizeof(float64(0))))
		default:
			return ErrIntegrity
		}
		if err := work.admit(value.heap.bytes, 0); err != nil {
			return err
		}
		if state.Kind == legacyNativeCheckpointBool {
			value.value = state.Bits == 1
		} else if state.Kind == legacyNativeCheckpointFloat64 {
			value.value = math.Float64frombits(state.Bits)
		}
		return work.complete(frame, value)
	}
}

func (work *legacyNativeCheckpointGraphRestore) loadOwner(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	var record legacyNativeCheckpointOwner
	if err := legacyNativeCheckpointReadJSON(ctx, work.io, frame.entry.body, &record); err != nil {
		return err
	}
	state := frame.state
	if err := record.validateBacking(); err != nil {
		return err
	}
	if record.Version != legacyNativeCheckpointVersion || record.Identity != state.Reference || record.Kind != state.Kind ||
		record.logicalLength() != state.Length || record.Length > legacyNativeCheckpointNodes || record.Revision < record.Length ||
		!record.Members.valid() || record.Members.Count != record.Length ||
		(record.Length == 0) != (record.Members == (legacyNativeCheckpointTree{})) || !record.Dirty.valid() {
		return ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	if state.Kind == legacyNativeCheckpointObject {
		if record.Capacity != 0 || record.Maximum != 0 || record.Dirty.Count > record.Length ||
			record.Length == 0 && record.Revision != 0 ||
			(record.Dirty.Count == 0) != (record.Dirty == (legacyNativeCheckpointTree{})) {
			return ErrIntegrity
		}
		planned = legacyNativeAnyMapBacking(0, int64(unsafe.Sizeof(any(nil))))
		planned.allocation(1, int64(unsafe.Sizeof(map[string]any(nil))))
		planned.include(legacyNativeAnyMapMetadata(0, docRangeHeaderHeapEstimate{}))
	} else {
		if record.Capacity != state.Capacity || record.Capacity > record.Maximum || record.Maximum > legacyNativeCheckpointNodes ||
			record.Dirty != (legacyNativeCheckpointTree{}) || record.Capacity > uint64(int(^uint(0)>>1))/uint64(unsafe.Sizeof(any(nil))) {
			return ErrIntegrity
		}
		planned = legacyNativeAnyArrayBacking(int(record.Capacity))
		planned.allocation(1, int64(unsafe.Sizeof([]any(nil))))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyArray{})))
	}
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes, 0); err != nil {
		return err
	}
	frame.record = record
	var err error
	if state.Kind == legacyNativeCheckpointObject {
		frame.entry.object = newLegacyNativeAnyMap()
	} else {
		frame.entry.array, err = newLegacyNativeAnyArray(int(record.Maximum))
		if err == nil {
			frame.entry.array.decoded = make([]any, 0, int(record.Capacity))
			frame.entry.array.tracked = true
			frame.entry.array.estimate.include(legacyNativeAnyArrayBacking(int(record.Capacity)))
		}
	}
	if err != nil {
		return err
	}
	frame.members, err = newLegacyNativeCheckpointListReader[legacyNativeCheckpointMember](record.Members, work.io)
	if err != nil {
		return err
	}
	if err := work.account(frame.entry, 0); err != nil {
		return err
	}
	frame.phase = legacyNativeCheckpointGraphRestoreMembers
	return work.refresh(frame)
}

func (entry *legacyNativeCheckpointGraphRestoredEntry) nativeHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	if entry.object != nil && entry.array == nil {
		heap = entry.object.heap()
		entry.object.retained(&heap)
	} else if entry.array != nil && entry.object == nil {
		heap = entry.array.heap()
		entry.array.retained(&heap)
	} else {
		heap.err = ErrIntegrity
	}
	return heap
}

func (work *legacyNativeCheckpointGraphRestore) account(entry *legacyNativeCheckpointGraphRestoredEntry, descendants int64) error {
	heap := entry.nativeHeap()
	if heap.err != nil {
		return heap.err
	}
	if descendants < 0 || descendants > heap.bytes || entry.owned > work.registry.owned {
		return ErrIntegrity
	}
	owned := heap.bytes - descendants
	next := docRangeHeaderHeapEstimate{bytes: work.registry.owned - entry.owned}
	next.include(docRangeHeaderHeapEstimate{bytes: owned})
	if next.err != nil {
		return next.err
	}
	work.registry.owned, entry.owned, entry.descendants = next.bytes, owned, descendants
	return nil
}

func (work *legacyNativeCheckpointGraphRestore) nextMember(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	member, found, err := frame.members.next(ctx)
	if err != nil {
		return err
	}
	frame.member = member
	if frame.entry.object != nil {
		if found {
			state, decodeErr := legacyNativeCheckpointGraphRestoreDescriptor(ctx, frame.member.Key)
			if decodeErr != nil {
				return decodeErr
			}
			if state.Kind != legacyNativeCheckpointString {
				return ErrIntegrity
			}
			frame.phase = legacyNativeCheckpointGraphRestoreKey
		}
	} else {
		if found {
			if frame.member.Key.Wire != nil || frame.member.Key.Body != (legacyNativeCheckpointRef{}) ||
				frame.member.Key.Text != (legacyNativeCheckpointText{}) {
				return ErrIntegrity
			}
			frame.phase = legacyNativeCheckpointGraphRestoreValue
		}
	}
	if found {
		if frame.position >= frame.record.Length {
			return ErrIntegrity
		}
		return work.refresh(frame)
	}
	if frame.position != frame.record.Length {
		return ErrIntegrity
	}
	frame.member = legacyNativeCheckpointMember{}
	frame.members.close()
	frame.members = nil
	if frame.entry.object != nil {
		frame.phase = legacyNativeCheckpointGraphRestoreClearDirty
	} else {
		frame.phase = legacyNativeCheckpointGraphRestoreFinish
	}
	return work.refresh(frame)
}

func (work *legacyNativeCheckpointGraphRestore) attach(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	entry := frame.entry
	descendants := docRangeHeaderHeapEstimate{bytes: entry.descendants}
	if frame.pending.owner.object != nil || frame.pending.owner.array != nil {
		childState, err := legacyNativeCheckpointGraphRestoreDescriptor(ctx, frame.member.Value)
		if err != nil {
			return err
		}
		child, _ := work.registry.lookup(childState.Reference)
		if child == nil || child.active || child.value.owner != frame.pending.owner {
			return ErrIntegrity
		}
		childHeap := frame.pending.heap
		childHeap.include(docRangeHeaderHeapEstimate{bytes: frame.pending.owner.bytes})
		descendants.include(childHeap)
	}
	if descendants.err != nil {
		return descendants.err
	}
	released := docRangeHeaderHeapEstimate{bytes: entry.owned}
	released.include(legacyNativeCheckpointGraphRestorePendingHeap(frame.key))
	released.include(legacyNativeCheckpointGraphRestorePendingHeap(frame.pending))
	if released.err != nil {
		return released.err
	}
	guard := func(future docRangeHeaderHeapEstimate) error {
		if future.err != nil {
			return future.err
		}
		if future.bytes < descendants.bytes {
			return ErrIntegrity
		}
		return work.admit(future.bytes-descendants.bytes, released.bytes)
	}
	if entry.object != nil {
		key, ok := frame.key.value.(string)
		if !ok {
			return ErrIntegrity
		}
		if _, exists := entry.object.decoded[key]; exists {
			return ErrIntegrity
		}
		if _, err := entry.object.putValueGuarded(key, frame.key.capacity, frame.pending, guard); err != nil {
			return err
		}
	} else {
		input := &legacyNormalizationInput{}
		var appended bool
		var err error
		if frame.position < frame.record.logicalLength() {
			appended, err = entry.array.appendValueGuarded(ctx, input, frame.pending, guard)
		} else {
			appended, err = entry.array.appendTailValueGuarded(ctx, input, frame.pending, guard)
		}
		if err != nil {
			return err
		}
		if !appended || input.workBytes > canonicalCheckBytes || entry.array.previous != nil || cap(entry.array.decoded) != int(frame.record.Capacity) {
			return ErrIntegrity
		}
	}
	if err := work.account(entry, descendants.bytes); err != nil {
		return err
	}
	frame.key, frame.pending, frame.member = legacyNativeAnyValue{}, legacyNativeAnyValue{}, legacyNativeCheckpointMember{}
	frame.position++
	frame.phase = legacyNativeCheckpointGraphRestoreMembers
	return work.refresh(frame)
}

func (work *legacyNativeCheckpointGraphRestore) clearDirty(frame *legacyNativeCheckpointGraphRestoreFrame) error {
	native := frame.entry.object
	if frame.cleared < frame.record.Length {
		link := native.link(uint32(frame.cleared + 1))
		if link == nil {
			return ErrIntegrity
		}
		link.dirty, link.dirtyNextID = false, 0
		frame.cleared++
		return nil
	}
	native.dirtyHeadID, native.dirtyTailID, native.dirtyCount = 0, 0, 0
	reader, err := newLegacyNativeCheckpointListReader[uint32](frame.record.Dirty, work.io)
	if err != nil {
		return err
	}
	frame.dirty, frame.phase = reader, legacyNativeCheckpointGraphRestoreDirty
	return work.refresh(frame)
}

func (work *legacyNativeCheckpointGraphRestore) nextDirty(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	identity, found, err := frame.dirty.next(ctx)
	if err != nil {
		return err
	}
	if !found {
		if frame.dirtyRead != frame.record.Dirty.Count {
			return ErrIntegrity
		}
		frame.dirty.close()
		frame.dirty = nil
		frame.phase = legacyNativeCheckpointGraphRestoreFinish
		return work.refresh(frame)
	}
	native := frame.entry.object
	link := native.link(identity)
	if identity == 0 || uint64(identity) > frame.record.Length || link == nil || link.dirty || frame.dirtyRead >= frame.record.Dirty.Count {
		return ErrIntegrity
	}
	if native.dirtyCount == 0 {
		native.dirtyHeadID = identity
	} else {
		tail := native.link(native.dirtyTailID)
		if tail == nil || tail.dirtyNextID != 0 {
			return ErrIntegrity
		}
		tail.dirtyNextID = identity
	}
	link.dirty, link.dirtyNextID = true, 0
	native.dirtyTailID, native.dirtyCount = identity, native.dirtyCount+1
	frame.dirtyRead++
	return work.refresh(frame)
}

func (work *legacyNativeCheckpointGraphRestore) finishOwner(frame *legacyNativeCheckpointGraphRestoreFrame) error {
	entry := frame.entry
	var value legacyNativeAnyValue
	var metadata docRangeHeaderHeapEstimate
	if entry.object != nil {
		entry.object.revision = frame.record.Revision
		if !entry.object.valid() || uint64(len(entry.object.decoded)) != frame.record.Length || uint64(entry.object.dirtyCount) != frame.record.Dirty.Count {
			return ErrIntegrity
		}
		entry.object.retained(&metadata)
		value = legacyNativeAnyValue{value: entry.object.values(), heap: entry.object.heap(), excess: entry.object.backingExcess(),
			owner: legacyNativeAnyValueOwner{object: entry.object, revision: entry.object.revision, bytes: metadata.bytes}}
	} else {
		entry.array.revision = frame.record.Revision
		if entry.array.previous != nil || uint64(len(entry.array.decoded)) != frame.record.logicalLength() ||
			uint64(cap(entry.array.decoded)) != frame.record.Capacity || entry.array.children.length() != len(entry.array.decoded) {
			return ErrIntegrity
		}
		if (entry.array.tail != nil) != frame.record.FullBacking {
			return ErrIntegrity
		}
		if frame.record.FullBacking {
			if err := entry.array.validateTail(true); err != nil {
				return err
			}
		}
		entry.array.retained(&metadata)
		value = legacyNativeAnyValue{value: entry.array.values(), heap: entry.array.heap(), excess: entry.array.backingExcess(),
			owner: legacyNativeAnyValueOwner{array: entry.array, revision: entry.array.revision, bytes: metadata.bytes}}
	}
	if metadata.err != nil {
		return metadata.err
	}
	if err := value.validateOwnership(); err != nil {
		return err
	}
	if err := work.account(entry, entry.descendants); err != nil {
		return err
	}
	entry.value, entry.active = value, false
	return work.complete(frame, value)
}

func (work *legacyNativeCheckpointGraphRestore) restoreText(ctx context.Context, frame *legacyNativeCheckpointGraphRestoreFrame) error {
	if work.text == nil {
		return ErrIntegrity
	}
	if err := work.refreshSharedTextBudget(ctx); err != nil {
		return err
	}
	ready, err := work.text.step(ctx)
	if err != nil || !ready {
		return err
	}
	scalar, err := work.text.take(ctx)
	if err != nil {
		return err
	}
	work.text = nil
	if scalar == nil || scalar.text == nil || scalar.previous != nil || scalar.position != 0 || scalar.end != 0 ||
		scalar.text.Len() != int(frame.state.Length) || scalar.text.Cap() != int(frame.state.Capacity) && frame.state.Capacity != frame.state.Length {
		return ErrIntegrity
	}
	text := scalar.text.String()
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(text)))
	heap.allocation(int64(scalar.text.Cap()), 1)
	if heap.err != nil {
		return heap.err
	}
	value := legacyNativeAnyValue{value: text, heap: heap, capacity: scalar.text.Cap(), excess: 2 * int64(scalar.text.Cap()-len(text))}
	return work.complete(frame, value)
}

func (work *legacyNativeCheckpointGraphRestore) complete(frame *legacyNativeCheckpointGraphRestoreFrame, value legacyNativeAnyValue) error {
	if frame != work.frame || work.workspace < frame.workspace || work.depth <= 0 {
		return ErrIntegrity
	}
	if err := value.validateOwnership(); err != nil {
		return err
	}
	work.frame, work.depth, work.workspace = frame.parent, work.depth-1, work.workspace-frame.workspace
	if work.frame == nil {
		work.result, work.ready = value, true
		return nil
	}
	parent := work.frame
	switch parent.phase {
	case legacyNativeCheckpointGraphRestoreWaitingKey:
		parent.key, parent.phase = value, legacyNativeCheckpointGraphRestoreValue
	case legacyNativeCheckpointGraphRestoreWaitingValue:
		parent.pending, parent.phase = value, legacyNativeCheckpointGraphRestoreAttach
	default:
		return ErrIntegrity
	}
	return work.refresh(parent)
}

func (work *legacyNativeCheckpointGraphRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0, 0); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	frame := work.frame
	if frame == nil {
		return work.fail(ErrIntegrity)
	}
	switch frame.phase {
	case legacyNativeCheckpointGraphRestoreBody, legacyNativeCheckpointGraphRestoreMembers, legacyNativeCheckpointGraphRestoreDirty:
		if err := work.admit(legacyNativeAnyCheckpointScratch, 0); err != nil {
			return work.fail(err)
		}
	}
	var err error
	switch frame.phase {
	case legacyNativeCheckpointGraphRestoreStart:
		err = work.begin(ctx, frame)
	case legacyNativeCheckpointGraphRestoreBody:
		err = work.loadOwner(ctx, frame)
	case legacyNativeCheckpointGraphRestoreMembers:
		err = work.nextMember(ctx, frame)
	case legacyNativeCheckpointGraphRestoreKey:
		frame.phase = legacyNativeCheckpointGraphRestoreWaitingKey
		err = work.push(ctx, frame.member.Key)
	case legacyNativeCheckpointGraphRestoreValue:
		frame.phase = legacyNativeCheckpointGraphRestoreWaitingValue
		err = work.push(ctx, frame.member.Value)
	case legacyNativeCheckpointGraphRestoreAttach:
		err = work.attach(ctx, frame)
	case legacyNativeCheckpointGraphRestoreClearDirty:
		err = work.clearDirty(frame)
	case legacyNativeCheckpointGraphRestoreDirty:
		err = work.nextDirty(ctx, frame)
	case legacyNativeCheckpointGraphRestoreFinish:
		err = work.finishOwner(frame)
	case legacyNativeCheckpointGraphRestoreText:
		err = work.restoreText(ctx, frame)
	default:
		err = ErrIntegrity
	}
	if err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0, 0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointGraphRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	if work.workspace < 0 {
		return 0, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(docRangeHeaderHeapEstimate{bytes: work.workspace})
	heap.include(legacyNativeCheckpointGraphRestorePendingHeap(work.result))
	if work.text != nil {
		bytes, err := work.text.retainedBytes()
		if err != nil {
			return 0, err
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointGraphRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		err = work.registry.fail(err)
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointGraphRestore) take(ctx context.Context) (legacyNativeAnyValue, error) {
	if work == nil {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeAnyValue{}, work.failure
	}
	if work.closed {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	if !work.ready || work.frame != nil || work.text != nil {
		_, failure := work.fail(ErrIntegrity)
		return legacyNativeAnyValue{}, failure
	}
	if err := work.result.validateOwnership(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	if err := work.admit(0, 0); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyValue{}, failure
	}
	value := work.result
	work.close()
	return value, nil
}

func (work *legacyNativeCheckpointGraphRestore) close() {
	if work == nil || work.closed {
		return
	}
	if !work.ready && work.registry != nil {
		work.registry.fail(ErrIntegrity)
	}
	if work.text != nil {
		work.text.close()
	}
	for frame := work.frame; frame != nil; frame = frame.parent {
		if frame.members != nil {
			frame.members.close()
		}
		if frame.dirty != nil {
			frame.dirty.close()
		}
	}
	failure := work.failure
	*work = legacyNativeCheckpointGraphRestore{closed: true, failure: failure}
}
