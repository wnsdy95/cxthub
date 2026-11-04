package domain

import (
	"context"
	"reflect"
	"unsafe"
)

type legacyNativeObjectCheckpointEventField struct {
	List legacyNativeCheckpointRef `json:"list"`
}

func (work *legacyNativeObjectCheckpointRestore) restoreEventListField(ctx context.Context) (bool, error) {
	field := work.native.metadata.fields[work.field]
	if !field.eventList || !work.state.HasEventCache {
		return false, nil
	}
	row := work.state.Fields[work.field]
	if row.Event == nil || work.eventFrame == nil || work.native.events == nil {
		return true, ErrIntegrity
	}
	value := work.native.destination.Elem().Field(field.index)
	if row.Present {
		value.Set(reflect.ValueOf([]CIREvent{}))
	}
	if row.Event.List == (legacyNativeCheckpointRef{}) {
		if row.Initialized {
			work.native.heaps[work.field].allocation(1, int64(value.Type().Size()))
		}
		work.field++
		return true, nil
	}
	var list *legacyNativeEventList
	var state legacyNativeEventListCheckpointControl
	if work.owner.list != nil {
		var err error
		list, state, err = work.owner.list.objectEventList(row.Event.List)
		if err != nil {
			return true, err
		}
	} else {
		if work.eventTask == nil {
			remaining, err := work.remaining()
			if err != nil {
				return true, err
			}
			work.eventTask, err = newLegacyNativeEventListCheckpointRestore(ctx, row.Event.List, work, remaining)
			return true, err
		}
		ready, err := work.eventTask.step(ctx)
		if err != nil || !ready {
			return true, err
		}
		list, state, err = work.eventTask.take(ctx)
		work.eventTask = nil
		if err != nil {
			return true, err
		}
	}
	active := work.state.EventActive && work.field == work.state.Field
	if list == nil || list.mutation != work.native.mutation || list.parentDepth != work.native.parentDepth+1 || list.frame != nil && list.frame != work.eventFrame ||
		(list.decoded != nil) != row.Present || (state.Borrowed != 0) != row.Initialized ||
		active && (!list.prefix || list.end != work.state.ValueEnd || list.position < work.state.ValueStart) ||
		!active && list.phase != legacyNativeEventsDone {
		return true, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	if active {
		if row.Initialized {
			heap.allocation(1, int64(value.Type().Size()))
		}
	} else {
		heap = list.estimate
		work.native.backingExcess[work.field] = list.excess
	}
	list.borrowed = heap
	work.native.heaps[work.field] = heap
	work.native.events[work.field] = list
	work.field++
	return true, nil
}

func (work *legacyNativeObjectCheckpoint) captureEventListField(ctx context.Context) (bool, error) {
	field := work.native.metadata.fields[work.field]
	if !field.eventList || !work.state.HasEventCache {
		return false, nil
	}
	guard := work.guards[work.field]
	var reference legacyNativeCheckpointRef
	if guard.event != nil {
		if work.eventOwner != nil {
			var err error
			reference, err = work.eventOwner.objectEventReference(guard.event)
			if err != nil {
				return true, err
			}
		} else {
			if work.eventTask == nil {
				remaining, err := work.remaining()
				if err != nil {
					return true, err
				}
				work.eventTask, err = newLegacyNativeEventListCheckpoint(ctx, work, work.field, remaining)
				return true, err
			}
			ready, err := work.eventTask.step(ctx)
			if err != nil || !ready {
				return true, err
			}
			reference, err = work.eventTask.take(ctx)
			work.eventTask = nil
			if err != nil {
				return true, err
			}
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectCheckpointEventField{})))
	planned.allocation(int64(len(reference.Hash)), 1)
	if err := work.admit(planned.bytes); err != nil {
		return true, err
	}
	work.state.Fields[work.field] = legacyNativeObjectCheckpointField{Index: work.field, Initialized: guard.heap.bytes != 0, Present: guard.present, Event: &legacyNativeObjectCheckpointEventField{List: reference}}
	work.field++
	return true, nil
}
