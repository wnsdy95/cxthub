package domain

import "context"

type legacyNativeAnyMapCheckpointEntry struct {
	Key         string
	Value       any
	KeyCapacity int
	ValueBytes  int64
	ValueExcess int64
	owner       legacyNativeAnyValueOwner
}

type legacyNativeAnyMapCheckpoint struct {
	owner     *legacyNativeAnyMap
	revision  uint64
	nextID    uint32
	remaining int
	full      bool
	finished  bool
	failure   error
}

func (native *legacyNativeAnyMap) checkpoint(ctx context.Context, full bool) (*legacyNativeAnyMapCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !native.valid() || native.payload.err != nil || native.payload.bytes < 0 || native.excess < 0 {
		return nil, ErrIntegrity
	}
	checkpoint := &legacyNativeAnyMapCheckpoint{owner: native, revision: native.revision, full: full}
	if full {
		checkpoint.nextID, checkpoint.remaining = native.firstID, len(native.ownership)
	} else {
		checkpoint.nextID, checkpoint.remaining = native.dirtyHeadID, native.dirtyCount
	}
	return checkpoint, nil
}

func (checkpoint *legacyNativeAnyMapCheckpoint) validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkpoint == nil {
		return ErrIntegrity
	}
	if checkpoint.failure != nil {
		return checkpoint.failure
	}
	if checkpoint.owner == nil || !checkpoint.owner.valid() || checkpoint.remaining < 0 {
		return ErrIntegrity
	}
	if checkpoint.owner.revision != checkpoint.revision {
		return ErrConflict
	}
	return nil
}

func (checkpoint *legacyNativeAnyMapCheckpoint) fail(err error) error {
	if checkpoint != nil {
		checkpoint.close()
		checkpoint.failure = err
	}
	return err
}

func (checkpoint *legacyNativeAnyMapCheckpoint) next(ctx context.Context) (legacyNativeAnyMapCheckpointEntry, bool, error) {
	if err := checkpoint.validate(ctx); err != nil {
		return legacyNativeAnyMapCheckpointEntry{}, false, checkpoint.fail(err)
	}
	if checkpoint.remaining == 0 {
		checkpoint.finished = true
		checkpoint.nextID = 0
		return legacyNativeAnyMapCheckpointEntry{}, false, nil
	}
	owner := checkpoint.owner
	node := owner.link(checkpoint.nextID)
	if node == nil {
		return legacyNativeAnyMapCheckpointEntry{}, false, checkpoint.fail(ErrIntegrity)
	}
	row, found := owner.ownership[node.canonicalKey]
	value, exists := owner.decoded[node.canonicalKey]
	if !found || !exists || row.linkID != checkpoint.nextID || row.keyCapacity < len(node.canonicalKey) ||
		row.valueBytes < 0 || row.valueExcess < 0 || row.valueExcess > row.valueBytes || !checkpoint.full && !node.dirty {
		return legacyNativeAnyMapCheckpointEntry{}, false, checkpoint.fail(ErrIntegrity)
	}
	next := node.nextID
	if !checkpoint.full {
		next = node.dirtyNextID
	}
	if (next != 0) != (checkpoint.remaining > 1) {
		return legacyNativeAnyMapCheckpointEntry{}, false, checkpoint.fail(ErrIntegrity)
	}
	if err := row.owner.matches(value); err != nil {
		return legacyNativeAnyMapCheckpointEntry{}, false, checkpoint.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return legacyNativeAnyMapCheckpointEntry{}, false, checkpoint.fail(err)
	}
	checkpoint.nextID = next
	checkpoint.remaining--
	return legacyNativeAnyMapCheckpointEntry{Key: node.canonicalKey, Value: value, KeyCapacity: row.keyCapacity, ValueBytes: row.valueBytes, ValueExcess: row.valueExcess, owner: row.owner}, true, nil
}

func (checkpoint *legacyNativeAnyMapCheckpoint) acknowledge(ctx context.Context, limit int) (bool, error) {
	if err := checkpoint.validate(ctx); err != nil {
		return false, checkpoint.fail(err)
	}
	if !checkpoint.finished || limit <= 0 || limit > 1024 {
		return false, checkpoint.fail(ErrIntegrity)
	}
	owner := checkpoint.owner
	var pending [1024]uint32
	count := min(limit, owner.dirtyCount)
	seen := make(map[uint32]struct{}, count)
	identity := owner.dirtyHeadID
	for processed := 0; processed < count; processed++ {
		if err := ctx.Err(); err != nil {
			return false, checkpoint.fail(err)
		}
		node := owner.link(identity)
		if node == nil {
			return false, checkpoint.fail(ErrIntegrity)
		}
		row, found := owner.ownership[node.canonicalKey]
		if !found || row.linkID != identity || !node.dirty || (node.dirtyNextID != 0) != (owner.dirtyCount-processed > 1) {
			return false, checkpoint.fail(ErrIntegrity)
		}
		if _, repeated := seen[identity]; repeated {
			return false, checkpoint.fail(ErrIntegrity)
		}
		seen[identity] = struct{}{}
		pending[processed] = identity
		identity = node.dirtyNextID
	}
	if owner.dirtyCount > count {
		survivor := owner.link(identity)
		if survivor == nil {
			return false, checkpoint.fail(ErrIntegrity)
		}
		row, found := owner.ownership[survivor.canonicalKey]
		if !found || row.linkID != identity || !survivor.dirty || (survivor.dirtyNextID != 0) != (owner.dirtyCount-count > 1) {
			return false, checkpoint.fail(ErrIntegrity)
		}
		if _, repeated := seen[identity]; repeated {
			return false, checkpoint.fail(ErrIntegrity)
		}
	}
	if err := ctx.Err(); err != nil {
		return false, checkpoint.fail(err)
	}
	for _, identity := range pending[:count] {
		node := owner.link(identity)
		node.dirty, node.dirtyNextID = false, 0
		owner.dirtyCount--
	}
	owner.dirtyHeadID = identity
	if owner.dirtyCount == 0 {
		owner.dirtyHeadID, owner.dirtyTailID = 0, 0
	}
	return owner.dirtyCount == 0, nil
}

func (checkpoint *legacyNativeAnyMapCheckpoint) close() {
	if checkpoint != nil {
		failure := checkpoint.failure
		*checkpoint = legacyNativeAnyMapCheckpoint{failure: failure}
	}
}
