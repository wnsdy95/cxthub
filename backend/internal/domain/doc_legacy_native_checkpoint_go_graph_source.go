package domain

import (
	"context"
	"reflect"
	"unsafe"
)

func newLegacyNativeCheckpointGoGraphSetForObject(ctx context.Context, owner *legacyNativeObjectCheckpoint) (*legacyNativeCheckpointGoGraphSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := owner.checkInitialGraphSource(); err != nil {
		return nil, err
	}
	return newLegacyNativeCheckpointGoGraphSetForSource(ctx, owner.initialValues, owner.state.Binding, owner.store, owner.budget, owner, nil)
}

func (owner *legacyNativeObjectCheckpoint) checkInitialGraphSource() error {
	if owner == nil || owner.closed || owner.ready || owner.failure != nil || owner.phase != 6 || owner.initialTask != nil ||
		owner.state.InitialGraphCount == 0 || int(owner.state.InitialGraphCount) != len(owner.initialValues) {
		return ErrConflict
	}
	if err := owner.checkSource(); err != nil {
		return err
	}
	return owner.checkInitialGraphRoots()
}

func (owner *legacyNativeObjectCheckpoint) checkInitialGraphRoots() error {
	if owner.native == nil || len(owner.initialSlots) != len(owner.native.metadata.fields) || len(owner.guards) != len(owner.native.metadata.fields) || len(owner.initialValues) == 0 || len(owner.initialValues) > 32 {
		return ErrConflict
	}
	count := 0
	for index, field := range owner.native.metadata.fields {
		value := owner.native.destination.Elem().Field(field.index)
		eligible := (field.genericMap || field.interfaceString) && owner.guards[index].dynamic == 2 && !value.IsNil() && owner.guards[index].container == nil
		if eligible && field.interfaceString {
			value = value.Elem()
		}
		eligible = eligible && (value.Kind() == reflect.Map || value.Kind() == reflect.Slice) && !value.IsNil()
		if !eligible {
			if owner.initialSlots[index] != 0 {
				return ErrConflict
			}
			continue
		}
		count++
		if int(owner.initialSlots[index]) != count || count > len(owner.initialValues) || !legacyNativeCheckpointGoGraphSetSame(value.Interface(), owner.initialValues[count-1]) {
			return ErrConflict
		}
	}
	if count != len(owner.initialValues) {
		return ErrConflict
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraphSet) checkOwner() error {
	if err := work.checkOwnerScope(); err != nil {
		return err
	}
	if work.owner != nil {
		return work.owner.checkSource()
	}
	if work.record != nil {
		return work.record.checkSource()
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraphSet) checkOwnerScope() error {
	if record := work.record; record != nil {
		if work.owner != nil || work.registry == nil || work.registry.source != work || work.registry.capture != record.capture ||
			work.position < 0 || work.position > len(work.values) || work.phase > 1 {
			return ErrIntegrity
		}
		if record.closed || record.ready || record.failure != nil || record.graph != work || record.capture == nil || !record.capture.valid(record.capture.scope) ||
			record.state.Binding != work.state.Binding || len(work.values) != len(record.roots) || len(work.borrowed) != len(record.roots) ||
			cap(work.borrowed) != len(record.roots) || unsafe.SliceData(work.borrowed) != &record.roots[0] {
			return ErrConflict
		}
		for index, value := range work.values {
			if !legacyNativeCheckpointGoGraphSetSame(value, record.roots[index]) {
				return ErrConflict
			}
		}
		return nil
	}
	if work.owner == nil {
		return nil
	}
	owner := work.owner
	if work.registry == nil || work.registry.source != work || work.position < 0 || work.position > len(work.values) || work.phase > 1 {
		return ErrIntegrity
	}
	if owner.closed || owner.ready || owner.failure != nil || owner.phase != 6 || owner.initialTask != work ||
		owner.native == nil || !owner.capture.valid(owner.native.mutation) ||
		owner.state.Binding != work.state.Binding || int(owner.state.InitialGraphCount) != len(work.values) ||
		len(owner.initialValues) != len(work.values) || cap(owner.initialValues) != cap(work.borrowed) || unsafe.SliceData(owner.initialValues) != unsafe.SliceData(work.borrowed) {
		return ErrConflict
	}
	for index, value := range work.values {
		if !legacyNativeCheckpointGoGraphSetSame(value, owner.initialValues[index]) {
			return ErrConflict
		}
	}
	return owner.checkInitialGraphRoots()
}
