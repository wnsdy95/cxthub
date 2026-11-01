package domain

import (
	"context"
	"unsafe"
)

const legacyNativeCheckpointGraphCaptureWorkspace = 512 * 1024
const legacyNativeCheckpointGraphCaptureDepth = 10001

type legacyNativeCheckpointGraphLookup struct {
	children [16]*legacyNativeCheckpointGraphLookup
	entry    *legacyNativeCheckpointGraphCaptured
}

type legacyNativeCheckpointGraphCaptured struct {
	next       *legacyNativeCheckpointGraphCaptured
	owner      legacyNativeAnyValueOwner
	state      legacyNativeValueCheckpoint
	record     legacyNativeCheckpointOwner
	body       legacyNativeCheckpointRef
	dirtyHead  uint32
	dirtyTail  uint32
	dirtyCount int
	finished   bool
}

type legacyNativeCheckpointGraphIdentity struct {
	root    *legacyNativeCheckpointGraphLookup
	first   *legacyNativeCheckpointGraphCaptured
	last    *legacyNativeCheckpointGraphCaptured
	nextID  uint64
	nodes   int64
	owners  int64
	views   int64
	closed  bool
	failure error
}

func newLegacyNativeCheckpointGraphIdentity() *legacyNativeCheckpointGraphIdentity {
	return &legacyNativeCheckpointGraphIdentity{}
}

func (identity *legacyNativeCheckpointGraphIdentity) retainedBytes() (int64, error) {
	if identity == nil || identity.closed {
		return 0, nil
	}
	if identity.nodes < 0 || identity.owners < 0 || identity.nextID > legacyNativeCheckpointNodes {
		return 0, ErrIntegrity
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*identity)))
	legacyCanonicalStorageAllocation(&estimate, int(identity.nodes), int64(unsafe.Sizeof(legacyNativeCheckpointGraphLookup{})))
	legacyCanonicalStorageAllocation(&estimate, int(identity.owners), int64(unsafe.Sizeof(legacyNativeCheckpointGraphCaptured{})))
	legacyCanonicalStorageAllocation(&estimate, 3*int(identity.owners), int64(len(contentHashPrefix)+64))
	legacyCanonicalStorageAllocation(&estimate, int(identity.views), int64(unsafe.Sizeof(legacyNativeCheckpointArrayView{})))
	legacyCanonicalStorageAllocation(&estimate, int(identity.views), legacyNativeValueCheckpointBytes)
	legacyCanonicalStorageAllocation(&estimate, 3*int(identity.views), int64(len(contentHashPrefix)+64))
	return estimate.bytes, estimate.err
}

func (identity *legacyNativeCheckpointGraphIdentity) close() {
	if identity != nil {
		failure := identity.failure
		*identity = legacyNativeCheckpointGraphIdentity{closed: true, failure: failure}
	}
}

func (identity *legacyNativeCheckpointGraphIdentity) fail(err error) error {
	if identity != nil {
		if identity.failure != nil {
			return identity.failure
		}
		identity.close()
		identity.failure = err
	}
	return err
}

func legacyNativeCheckpointGraphOwnerAddress(owner legacyNativeAnyValueOwner) (uintptr, error) {
	if (owner.object == nil) == (owner.array == nil) {
		return 0, ErrIntegrity
	}
	if owner.object != nil {
		return uintptr(unsafe.Pointer(owner.object)), nil
	}
	return uintptr(unsafe.Pointer(owner.array)), nil
}

func (identity *legacyNativeCheckpointGraphIdentity) lookup(address uintptr) (*legacyNativeCheckpointGraphCaptured, int) {
	const levels = int(unsafe.Sizeof(uintptr(0))) * 2
	node := identity.root
	if node == nil {
		return nil, levels + 1
	}
	for level := levels - 1; level >= 0; level-- {
		node = node.children[(address>>uint(4*level))&15]
		if node == nil {
			return nil, level + 1
		}
	}
	return node.entry, 0
}

func (captured *legacyNativeCheckpointGraphCaptured) validate() error {
	if captured == nil {
		return ErrIntegrity
	}
	if err := captured.owner.validate(); err != nil {
		return err
	}
	if err := captured.record.validateBacking(); err != nil {
		return err
	}
	if native := captured.owner.object; native != nil {
		if !native.valid() {
			return ErrIntegrity
		}
		if uint64(len(native.decoded)) != captured.record.Length || native.dirtyHeadID != captured.dirtyHead ||
			native.dirtyTailID != captured.dirtyTail || native.dirtyCount != captured.dirtyCount {
			return ErrConflict
		}
		return nil
	}
	list := captured.owner.array
	if list != nil && list.resource != nil {
		if err := list.validateResource(); err != nil {
			return err
		}
		if captured.finished && (captured.record.Allocation == nil || captured.record.Allocation.Offset != uint64(list.offset)) {
			return ErrConflict
		}
	} else if captured.record.Allocation != nil {
		return ErrIntegrity
	}
	if list == nil || !list.children.valid() || list.children.length() != len(list.decoded) || len(list.decoded) > 0 && !list.tracked ||
		list.maximum < 0 || list.copied < 0 || list.previous != nil && (len(list.previous) != len(list.decoded) || list.copied > len(list.previous)) {
		return ErrIntegrity
	}
	if uint64(len(list.values())) != captured.record.logicalLength() || uint64(cap(list.decoded)) != captured.record.Capacity ||
		uint64(list.maximum) != captured.record.Maximum {
		return ErrConflict
	}
	if (list.tail != nil) != captured.record.FullBacking {
		return ErrIntegrity
	}
	if captured.record.FullBacking {
		if err := list.validateTail(true); err != nil {
			return err
		}
	}
	return nil
}

type legacyNativeCheckpointGraphCaptureTextWork interface {
	step(context.Context) (bool, error)
	take(context.Context) (legacyNativeCheckpointText, error)
	retainedBytes() (int64, error)
	close()
}

const (
	legacyNativeCheckpointGraphCaptureValue uint8 = iota
	legacyNativeCheckpointGraphCaptureText
	legacyNativeCheckpointGraphCaptureMembers
	legacyNativeCheckpointGraphCaptureKey
	legacyNativeCheckpointGraphCaptureChild
	legacyNativeCheckpointGraphCaptureAppend
	legacyNativeCheckpointGraphCaptureMembersFinish
	legacyNativeCheckpointGraphCaptureDirty
	legacyNativeCheckpointGraphCaptureDirtyFinish
	legacyNativeCheckpointGraphCaptureOwner
	legacyNativeCheckpointGraphCaptureWaitingKey
	legacyNativeCheckpointGraphCaptureWaitingChild
	legacyNativeCheckpointGraphCaptureTextFinish
	legacyNativeCheckpointGraphCaptureBacking
	legacyNativeCheckpointGraphCaptureWaitingBacking
)

type legacyNativeCheckpointGraphCaptureFrame struct {
	parent        *legacyNativeCheckpointGraphCaptureFrame
	captured      *legacyNativeCheckpointGraphCaptured
	snapshot      *legacyNativeValueSnapshot
	text          legacyNativeCheckpointGraphCaptureTextWork
	cursor        *legacyNativeAnyMapCheckpoint
	members       *legacyNativeCheckpointListWriter[legacyNativeCheckpointMember]
	dirty         *legacyNativeCheckpointListWriter[uint32]
	seen          legacyCanonicalPages[uint64]
	source        legacyNativeAnyMapCheckpointEntry
	child         legacyNativeAnyValue
	member        legacyNativeCheckpointMember
	state         legacyNativeValueCheckpoint
	result        legacyNativeCheckpointValue
	position      uint64
	dirtyPosition int
	markedCount   int
	phase         uint8
	bytes         int64
}

type legacyNativeCheckpointGraphWriter struct {
	sharedField *legacyNativeObjectAnyCheckpoint
	sharedAny   *legacyNativeAnyCheckpoint
	store       legacyNativeCheckpointIO
	identity    *legacyNativeCheckpointGraphIdentity
	frame       *legacyNativeCheckpointGraphCaptureFrame
	verify      *legacyNativeCheckpointGraphCaptured
	root        *legacyNativeCheckpointGraphCaptured
	result      legacyNativeCheckpointValue
	frameBytes  int64
	budget      int64
	depth       int
	verifying   bool
	ready       bool
	closed      bool
	failure     error
}

func newLegacyNativeCheckpointGraphWriter(ctx context.Context, value legacyNativeAnyValue, store legacyNativeCheckpointIO, identity *legacyNativeCheckpointGraphIdentity, heapBudget int64) (*legacyNativeCheckpointGraphWriter, error) {
	return newLegacyNativeCheckpointGraphWriterInField(ctx, value, store, identity, heapBudget, nil, nil)
}

func newLegacyNativeCheckpointGraphWriterInField(ctx context.Context, value legacyNativeAnyValue, store legacyNativeCheckpointIO, identity *legacyNativeCheckpointGraphIdentity, heapBudget int64, field *legacyNativeObjectAnyCheckpoint, nested *legacyNativeAnyCheckpoint) (result *legacyNativeCheckpointGraphWriter, failure error) {
	var registered *legacyNativeCheckpointGraphWriter
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
		parent, err := field.sharedCaptureRoot()
		if err != nil {
			return nil, err
		}
		if parent.anyGraph != identity || nested != nil && (field.child != nested || nested.sharedField != field || nested.graph != nil) || nested == nil && field.graph != nil {
			return nil, ErrConflict
		}
		var own docRangeHeaderHeapEstimate
		own.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphWriter{})))
		if own.err != nil {
			return nil, own.err
		}
		if err := parent.admit(own.bytes); err != nil {
			return nil, err
		}
		store = parent.store
	}
	if identity != nil && identity.failure != nil {
		return nil, identity.failure
	}
	if err := ctx.Err(); err != nil {
		return nil, identity.fail(err)
	}
	if identity == nil || identity.closed || store.Put == nil || heapBudget <= 0 {
		return nil, identity.fail(ErrIntegrity)
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphWriter{})))
	bytes, err := identity.retainedBytes()
	planned.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	if planned.err != nil {
		return nil, identity.fail(planned.err)
	}
	if planned.bytes > heapBudget {
		return nil, identity.fail(ErrDocumentResourceLimit)
	}
	work := &legacyNativeCheckpointGraphWriter{sharedField: field, sharedAny: nested, store: store, identity: identity, budget: heapBudget}
	if field != nil {
		registered = work
		if nested != nil {
			nested.graph = work
		} else {
			field.graph = work
		}
	}
	if err := work.push(ctx, value); err != nil {
		_, failure := work.fail(err)
		return nil, failure
	}
	work.root = work.frame.captured
	return work, nil
}

func (work *legacyNativeCheckpointGraphWriter) admit(extra int64) error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
	if extra < 0 || work == nil || work.closed || work.identity == nil || work.identity.closed {
		return ErrIntegrity
	}
	current, err := work.retainedBytes()
	if err != nil {
		return err
	}
	shared, err := work.identity.retainedBytes()
	if err != nil {
		return err
	}
	var planned docRangeHeaderHeapEstimate
	planned.include(docRangeHeaderHeapEstimate{bytes: current})
	planned.include(docRangeHeaderHeapEstimate{bytes: shared})
	planned.include(docRangeHeaderHeapEstimate{bytes: extra})
	if planned.err != nil {
		return planned.err
	}
	if planned.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointGraphWriter) captureOwner(owner legacyNativeAnyValueOwner) (*legacyNativeCheckpointGraphCaptured, bool, error) {
	address, err := legacyNativeCheckpointGraphOwnerAddress(owner)
	if err != nil {
		return nil, false, err
	}
	const levels = int(unsafe.Sizeof(uintptr(0))) * 2
	captured, missing := work.identity.lookup(address)
	if missing == 0 {
		if captured == nil || captured.owner.object != owner.object || captured.owner.array != owner.array {
			return nil, false, ErrIntegrity
		}
		if captured.owner.revision != owner.revision {
			return nil, false, ErrConflict
		}
		if err := captured.validate(); err != nil {
			return nil, false, err
		}
		if !captured.finished {
			return nil, false, ErrIntegrity
		}
		return captured, false, nil
	}
	if work.identity.nextID >= legacyNativeCheckpointNodes {
		return nil, false, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	legacyCanonicalStorageAllocation(&planned, missing, int64(unsafe.Sizeof(legacyNativeCheckpointGraphLookup{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphCaptured{})))
	legacyCanonicalStorageAllocation(&planned, 3, int64(len(contentHashPrefix)+64))
	if planned.err != nil {
		return nil, false, planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return nil, false, err
	}
	if work.identity.root == nil {
		work.identity.root = &legacyNativeCheckpointGraphLookup{}
		work.identity.nodes++
	}
	node := work.identity.root
	for level := levels - 1; level >= 0; level-- {
		position := (address >> uint(4*level)) & 15
		if node.children[position] == nil {
			node.children[position] = &legacyNativeCheckpointGraphLookup{}
			work.identity.nodes++
		}
		node = node.children[position]
	}
	work.identity.nextID++
	captured = &legacyNativeCheckpointGraphCaptured{owner: owner, record: legacyNativeCheckpointOwner{
		Version: legacyNativeCheckpointVersion, Identity: work.identity.nextID, Revision: owner.revision,
	}}
	if native := owner.object; native != nil {
		captured.record.Kind, captured.record.Length = legacyNativeCheckpointObject, uint64(len(native.decoded))
		captured.dirtyHead, captured.dirtyTail, captured.dirtyCount = native.dirtyHeadID, native.dirtyTailID, native.dirtyCount
	} else {
		list := owner.array
		captured.record.Kind, captured.record.Length = legacyNativeCheckpointArray, uint64(len(list.values()))
		captured.record.Capacity, captured.record.Maximum = uint64(cap(list.decoded)), uint64(list.maximum)
		if list.tail != nil {
			captured.record.FullBacking = true
			captured.record.Visible = captured.record.Length
			captured.record.Length = captured.record.Capacity
		}
		if list.maximum < 0 || captured.record.Length > captured.record.Capacity || captured.record.Capacity > captured.record.Maximum ||
			captured.record.Maximum > legacyNativeCheckpointNodes || owner.revision < captured.record.Length {
			return nil, false, ErrIntegrity
		}
	}
	if captured.record.Length > legacyNativeCheckpointNodes {
		return nil, false, ErrIntegrity
	}
	if err := captured.validate(); err != nil {
		return nil, false, err
	}
	node.entry = captured
	if work.identity.last == nil {
		work.identity.first = captured
	} else {
		work.identity.last.next = captured
	}
	work.identity.last = captured
	work.identity.owners++
	return captured, true, nil
}

func (work *legacyNativeCheckpointGraphWriter) push(ctx context.Context, value legacyNativeAnyValue) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work.depth >= legacyNativeCheckpointGraphCaptureDepth {
		return ErrIntegrity
	}
	if err := value.validateOwnership(); err != nil {
		return err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphCaptureFrame{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeValueSnapshot{})))
	planned.allocation(legacyNativeValueCheckpointBytes, 1)
	if planned.err != nil {
		return planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return err
	}
	frame := &legacyNativeCheckpointGraphCaptureFrame{parent: work.frame}
	work.frame, work.depth = frame, work.depth+1
	if err := work.measure(frame); err != nil {
		return err
	}
	var reference uint64
	fresh := false
	if value.owner.object != nil || value.owner.array != nil {
		captured, discovered, err := work.captureOwner(value.owner)
		if err != nil {
			return err
		}
		frame.captured, reference, fresh = captured, captured.record.Identity, discovered
	} else {
		if work.identity.nextID >= legacyNativeCheckpointNodes {
			return ErrIntegrity
		}
		work.identity.nextID++
	}
	var snapshotHeap docRangeHeaderHeapEstimate
	snapshotHeap.allocation(1, int64(unsafe.Sizeof(legacyNativeValueSnapshot{})))
	if err := work.admit(snapshotHeap.bytes); err != nil {
		return err
	}
	bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
	snapshot, err := value.checkpoint(ctx, reference, bounds)
	if err != nil {
		return err
	}
	frame.state = snapshot.state
	if frame.captured != nil {
		if frame.state.Kind == legacyNativeCheckpointArray {
			frame.state.Capacity = frame.captured.record.Capacity
		}
		if fresh {
			frame.captured.state = frame.state
			frame.phase = legacyNativeCheckpointGraphCaptureMembers
			if frame.captured.owner.array != nil && frame.captured.owner.array.resource != nil {
				frame.phase = legacyNativeCheckpointGraphCaptureBacking
			}
		} else if frame.captured.state != frame.state {
			snapshot.close()
			return ErrConflict
		}
		frame.result.Body = frame.captured.body
	}
	if frame.state.Kind == legacyNativeCheckpointString {
		frame.snapshot = snapshot
	} else {
		snapshot.close()
	}
	if err := work.measure(frame); err != nil {
		return err
	}
	return work.admit(0)
}

func legacyNativeCheckpointGraphCapturedValueHeap(estimate *docRangeHeaderHeapEstimate, saved legacyNativeCheckpointValue) {
	estimate.allocation(int64(cap(saved.Wire)), 1)
	estimate.allocation(int64(len(saved.Body.Hash)), 1)
	estimate.allocation(int64(len(saved.Text.Tree.Ref.Hash)), 1)
}

func (work *legacyNativeCheckpointGraphWriter) measure(frame *legacyNativeCheckpointGraphCaptureFrame) error {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*frame)))
	if frame.snapshot != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*frame.snapshot)))
	}
	if frame.cursor != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*frame.cursor)))
	}
	if frame.members != nil {
		bytes, err := frame.members.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	if frame.dirty != nil {
		bytes, err := frame.dirty.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	if frame.text != nil {
		bytes, err := frame.text.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	frame.seen.retained(&estimate)
	legacyNativeCheckpointGraphCapturedValueHeap(&estimate, frame.member.Key)
	legacyNativeCheckpointGraphCapturedValueHeap(&estimate, frame.member.Value)
	legacyNativeCheckpointGraphCapturedValueHeap(&estimate, frame.result)
	if estimate.err != nil || work.frameBytes < frame.bytes {
		return ErrIntegrity
	}
	var total docRangeHeaderHeapEstimate
	total.include(docRangeHeaderHeapEstimate{bytes: work.frameBytes - frame.bytes})
	total.include(estimate)
	if total.err != nil {
		return total.err
	}
	frame.bytes, work.frameBytes = estimate.bytes, total.bytes
	return nil
}

func (work *legacyNativeCheckpointGraphWriter) finishFrame(ctx context.Context, frame *legacyNativeCheckpointGraphCaptureFrame) error {
	bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
	if err := work.admit(64 + 2*legacyNativeValueCheckpointBytes); err != nil {
		return err
	}
	wire, err := frame.state.encode(ctx, bounds)
	if err != nil {
		return err
	}
	frame.result.Wire = wire
	parent, result := frame.parent, frame.result
	work.frameBytes -= frame.bytes
	work.frame, work.depth = parent, work.depth-1
	frame.close()
	if parent == nil {
		work.result = result
		work.verify, work.verifying = work.identity.first, true
		return work.admit(0)
	}
	switch parent.phase {
	case legacyNativeCheckpointGraphCaptureWaitingBacking:
		parent.captured.record.Allocation = &legacyNativeCheckpointArrayView{Backing: result, Offset: uint64(parent.captured.owner.array.offset)}
		work.identity.views++
		parent.phase = legacyNativeCheckpointGraphCaptureMembers
	case legacyNativeCheckpointGraphCaptureWaitingKey:
		parent.member.Key, parent.phase = result, legacyNativeCheckpointGraphCaptureChild
	case legacyNativeCheckpointGraphCaptureWaitingChild:
		parent.member.Value, parent.phase = result, legacyNativeCheckpointGraphCaptureAppend
	default:
		return ErrIntegrity
	}
	return work.measure(parent)
}

func (work *legacyNativeCheckpointGraphWriter) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed || work.identity == nil {
		return false, ErrIntegrity
	}
	if work.identity.failure != nil {
		return work.fail(work.identity.failure)
	}
	if work.identity.closed {
		return work.fail(ErrIntegrity)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	if work.ready {
		if work.root != nil {
			if err := work.root.validate(); err != nil {
				return work.fail(err)
			}
		}
		return true, nil
	}
	if work.verifying {
		if work.verify != nil {
			if !work.verify.finished {
				return work.fail(ErrConflict)
			}
			if err := work.verify.validate(); err != nil {
				return work.fail(err)
			}
			work.verify = work.verify.next
			return false, nil
		}
		work.ready = true
		return true, nil
	}
	frame := work.frame
	if frame == nil {
		return work.fail(ErrIntegrity)
	}
	if frame.captured != nil {
		if err := frame.captured.validate(); err != nil {
			return work.fail(err)
		}
	}
	if err := work.stepFrame(ctx, frame); err != nil {
		return work.fail(err)
	}
	if work.frame == frame {
		if err := work.measure(frame); err != nil {
			return work.fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return false, nil
}

func (work *legacyNativeCheckpointGraphWriter) stepFrame(ctx context.Context, frame *legacyNativeCheckpointGraphCaptureFrame) error {
	switch frame.phase {
	case legacyNativeCheckpointGraphCaptureBacking:
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		frame.phase = legacyNativeCheckpointGraphCaptureWaitingBacking
		return work.push(ctx, frame.captured.owner.array.resource.root)
	case legacyNativeCheckpointGraphCaptureValue:
		if frame.state.Kind != legacyNativeCheckpointString {
			return work.finishFrame(ctx, frame)
		}
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		if frame.state.Length <= 64 {
			body, err := frame.snapshot.read(ctx, 0, int(frame.state.Length))
			if err != nil {
				return err
			}
			if len(body) > 0 {
				ref, err := legacyNativeCheckpointWrite(ctx, work.store, body)
				if err != nil {
					return err
				}
				frame.result.Text = legacyNativeCheckpointText{Bytes: len(body), Tree: legacyNativeCheckpointTree{Ref: ref, Count: uint64(len(body))}}
			}
			frame.snapshot.close()
			frame.snapshot = nil
			return work.finishFrame(ctx, frame)
		}
		text, err := newLegacyNativeCheckpointTextWriter(ctx, int(frame.state.Length), frame.snapshot.read, work.store)
		if err != nil {
			return err
		}
		frame.text, frame.phase = text, legacyNativeCheckpointGraphCaptureText
		return nil
	case legacyNativeCheckpointGraphCaptureText:
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		ready, err := frame.text.step(ctx)
		if err != nil || !ready {
			return err
		}
		frame.phase = legacyNativeCheckpointGraphCaptureTextFinish
		return nil
	case legacyNativeCheckpointGraphCaptureTextFinish:
		saved, err := frame.text.take(ctx)
		if err != nil {
			return err
		}
		if saved.Bytes != int(frame.state.Length) || saved.Tree.Count != frame.state.Length || !saved.Tree.valid() {
			return ErrIntegrity
		}
		frame.result.Text = saved
		frame.text.close()
		frame.text = nil
		frame.snapshot.close()
		frame.snapshot = nil
		return work.finishFrame(ctx, frame)
	case legacyNativeCheckpointGraphCaptureMembers:
		return work.nextMember(ctx, frame)
	case legacyNativeCheckpointGraphCaptureKey:
		key := frame.source.Key
		capacity := frame.source.KeyCapacity
		if capacity < len(key) {
			return ErrIntegrity
		}
		var estimate docRangeHeaderHeapEstimate
		estimate.allocation(1, int64(unsafe.Sizeof(key)))
		estimate.allocation(int64(capacity), 1)
		if estimate.err != nil {
			return estimate.err
		}
		value := legacyNativeAnyValue{value: key, capacity: capacity, heap: estimate, excess: 2 * int64(capacity-len(key))}
		frame.phase = legacyNativeCheckpointGraphCaptureWaitingKey
		return work.push(ctx, value)
	case legacyNativeCheckpointGraphCaptureChild:
		frame.phase = legacyNativeCheckpointGraphCaptureWaitingChild
		return work.push(ctx, frame.child)
	case legacyNativeCheckpointGraphCaptureAppend:
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		if err := frame.members.append(ctx, work.store, frame.member); err != nil {
			return err
		}
		frame.position++
		frame.member, frame.source, frame.child = legacyNativeCheckpointMember{}, legacyNativeAnyMapCheckpointEntry{}, legacyNativeAnyValue{}
		frame.phase = legacyNativeCheckpointGraphCaptureMembers
		return nil
	case legacyNativeCheckpointGraphCaptureMembersFinish:
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		members, err := frame.members.finish(ctx, work.store)
		if err != nil {
			return err
		}
		if members.Count != frame.captured.record.Length || !members.valid() {
			return ErrIntegrity
		}
		frame.captured.record.Members = members
		frame.members.close()
		frame.members = nil
		if frame.captured.owner.object != nil {
			frame.phase = legacyNativeCheckpointGraphCaptureDirty
		} else {
			frame.phase = legacyNativeCheckpointGraphCaptureOwner
		}
		return nil
	case legacyNativeCheckpointGraphCaptureDirty:
		return work.nextDirty(ctx, frame)
	case legacyNativeCheckpointGraphCaptureDirtyFinish:
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		dirty, err := frame.dirty.finish(ctx, work.store)
		if err != nil {
			return err
		}
		if dirty.Count != uint64(frame.captured.dirtyCount) || !dirty.valid() {
			return ErrIntegrity
		}
		frame.captured.record.Dirty = dirty
		frame.dirty.close()
		frame.dirty = nil
		frame.seen.clear()
		frame.phase = legacyNativeCheckpointGraphCaptureOwner
		return nil
	case legacyNativeCheckpointGraphCaptureOwner:
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		body, err := legacyNativeCheckpointWriteJSON(ctx, work.store, frame.captured.record)
		if err != nil {
			return err
		}
		if err := frame.captured.validate(); err != nil {
			return err
		}
		frame.captured.body, frame.captured.finished = body, true
		frame.result.Body = body
		return work.finishFrame(ctx, frame)
	default:
		return ErrIntegrity
	}
}

func (work *legacyNativeCheckpointGraphWriter) nextMember(ctx context.Context, frame *legacyNativeCheckpointGraphCaptureFrame) error {
	if frame.members == nil {
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		frame.members = &legacyNativeCheckpointListWriter[legacyNativeCheckpointMember]{}
		if frame.captured.owner.object != nil {
			cursor, err := frame.captured.owner.mapCheckpoint(ctx, true)
			if err != nil {
				return err
			}
			frame.cursor = cursor
		}
		return nil
	}
	if frame.position > frame.captured.record.Length {
		return ErrIntegrity
	}
	if frame.captured.owner.array != nil {
		if frame.position == frame.captured.record.Length {
			frame.phase = legacyNativeCheckpointGraphCaptureMembersFinish
			return nil
		}
		var value legacyNativeAnyValue
		var err error
		if frame.position < frame.captured.record.logicalLength() {
			value, err = frame.captured.owner.arrayValue(ctx, int(frame.position))
		} else {
			if err := frame.captured.owner.validate(); err != nil {
				return err
			}
			value, err = frame.captured.owner.array.tailValue(ctx, int(frame.position))
		}
		if err != nil {
			return err
		}
		frame.child, frame.phase = value, legacyNativeCheckpointGraphCaptureChild
		return nil
	}
	identity := frame.cursor.nextID
	entry, ok, err := frame.cursor.next(ctx)
	if err != nil {
		return err
	}
	if !ok {
		if frame.position != frame.captured.record.Length || frame.markedCount != frame.captured.dirtyCount {
			return ErrIntegrity
		}
		frame.cursor.close()
		frame.cursor = nil
		frame.phase = legacyNativeCheckpointGraphCaptureMembersFinish
		return nil
	}
	if frame.position >= frame.captured.record.Length || uint64(identity) != frame.position+1 {
		return ErrIntegrity
	}
	link := frame.captured.owner.object.link(identity)
	if link == nil {
		return ErrIntegrity
	}
	if link.dirty {
		frame.markedCount++
	}
	if frame.captured.dirtyCount > 0 && frame.position%64 == 0 {
		storage, future, err := frame.seen.appendRequirements()
		if err != nil {
			return err
		}
		current, err := frame.seen.ownedBytes()
		if err != nil || future.err != nil {
			return ErrIntegrity
		}
		if err := work.admit(max(0, future.bytes-current) + int64(storage)); err != nil {
			return err
		}
		if err := frame.seen.append(0); err != nil {
			return err
		}
	}
	value := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
	if text, ok := entry.Value.(string); ok {
		extra := entry.ValueExcess / 2
		if entry.ValueExcess < 0 || entry.ValueExcess%2 != 0 || extra > int64(int(^uint(0)>>1)-len(text)) {
			return ErrIntegrity
		}
		value.capacity = len(text) + int(extra)
	}
	frame.source, frame.child, frame.phase = entry, value, legacyNativeCheckpointGraphCaptureKey
	return nil
}

func (work *legacyNativeCheckpointGraphWriter) nextDirty(ctx context.Context, frame *legacyNativeCheckpointGraphCaptureFrame) error {
	if frame.dirty == nil {
		if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
			return err
		}
		frame.dirty = &legacyNativeCheckpointListWriter[uint32]{}
		cursor, err := frame.captured.owner.mapCheckpoint(ctx, false)
		if err != nil {
			return err
		}
		frame.cursor = cursor
		return nil
	}
	identity := frame.cursor.nextID
	_, ok, err := frame.cursor.next(ctx)
	if err != nil {
		return err
	}
	if !ok {
		if frame.dirtyPosition != frame.captured.dirtyCount {
			return ErrIntegrity
		}
		frame.cursor.close()
		frame.cursor = nil
		frame.phase = legacyNativeCheckpointGraphCaptureDirtyFinish
		return nil
	}
	if identity == 0 || uint64(identity) > frame.captured.record.Length || frame.dirtyPosition >= frame.captured.dirtyCount {
		return ErrIntegrity
	}
	seen := frame.seen.at(int((uint64(identity) - 1) / 64))
	mask := uint64(1) << ((uint64(identity) - 1) % 64)
	if seen == nil || *seen&mask != 0 || frame.dirtyPosition == frame.captured.dirtyCount-1 && identity != frame.captured.dirtyTail {
		return ErrIntegrity
	}
	if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
		return err
	}
	if err := frame.dirty.append(ctx, work.store, identity); err != nil {
		return err
	}
	*seen |= mask
	frame.dirtyPosition++
	return nil
}

func (work *legacyNativeCheckpointGraphWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	if work.frameBytes < 0 || work.depth < 0 || work.depth > legacyNativeCheckpointGraphCaptureDepth {
		return 0, ErrIntegrity
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*work)))
	estimate.include(docRangeHeaderHeapEstimate{bytes: work.frameBytes})
	legacyNativeCheckpointGraphCapturedValueHeap(&estimate, work.result)
	return estimate.bytes, estimate.err
}

func (work *legacyNativeCheckpointGraphWriter) take(ctx context.Context) (legacyNativeCheckpointValue, error) {
	if work == nil {
		return legacyNativeCheckpointValue{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointValue{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointValue{}, failure
	}
	if work.closed {
		return legacyNativeCheckpointValue{}, ErrIntegrity
	}
	if work.identity != nil && work.identity.failure != nil {
		_, err := work.fail(work.identity.failure)
		return legacyNativeCheckpointValue{}, err
	}
	if !work.ready || work.identity == nil || work.identity.closed {
		_, err := work.fail(ErrIntegrity)
		return legacyNativeCheckpointValue{}, err
	}
	if err := work.admit(0); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointValue{}, failure
	}
	if work.root != nil {
		if err := work.root.validate(); err != nil {
			_, failure := work.fail(err)
			return legacyNativeCheckpointValue{}, failure
		}
	}
	if err := work.admit(0); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointValue{}, failure
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointGraphWriter) fail(err error) (bool, error) {
	if work != nil {
		if work.failure != nil {
			return false, work.failure
		}
		err = work.identity.fail(err)
		work.close()
		work.failure = err
	}
	return false, err
}

func (frame *legacyNativeCheckpointGraphCaptureFrame) close() {
	if frame != nil {
		frame.snapshot.close()
		frame.cursor.close()
		if frame.members != nil {
			frame.members.close()
		}
		if frame.dirty != nil {
			frame.dirty.close()
		}
		if frame.text != nil {
			frame.text.close()
		}
		*frame = legacyNativeCheckpointGraphCaptureFrame{}
	}
}

func (work *legacyNativeCheckpointGraphWriter) close() {
	if work != nil && !work.closed {
		if !work.ready && work.identity != nil {
			work.identity.fail(ErrIntegrity)
		}
		for frame := work.frame; frame != nil; {
			parent := frame.parent
			frame.close()
			frame = parent
		}
		failure := work.failure
		*work = legacyNativeCheckpointGraphWriter{closed: true, failure: failure}
	}
}
