package domain

import (
	"context"
	"reflect"
	"unsafe"
)

var legacyEventNativeFields = newLegacyNativeObjectFields(reflect.TypeOf(wire{}))

type legacyNormalizationNativeEvent struct {
	decoded        wire
	object         *legacyNativeObject
	canonicalReady bool
}

func newLegacyNormalizationNativeEvent() (*legacyNormalizationNativeEvent, error) {
	native := &legacyNormalizationNativeEvent{}
	object, err := newLegacyNativeObject(reflect.ValueOf(&native.decoded), &legacyEventNativeFields, 3, true)
	if err != nil {
		return nil, err
	}
	native.object = object
	return native, nil
}

func (native *legacyNormalizationNativeEvent) reset(from, to int) error {
	if err := native.object.mutation.enter(); err != nil {
		return err
	}
	defer native.object.mutation.leave()
	if err := native.releaseValues(); err != nil {
		return err
	}
	return native.object.start(from, to)
}

func (native *legacyNormalizationNativeEvent) releaseValues() error {
	if err := native.object.mutation.enter(); err != nil {
		return err
	}
	defer native.object.mutation.leave()
	native.canonicalReady = false
	native.decoded = wire{}
	return native.object.releaseValues()
}

func (native *legacyNormalizationNativeEvent) step(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (CIREvent, bool, error) {
	if err := ctx.Err(); err != nil {
		if native != nil && native.object != nil {
			if mutationErr := native.object.mutation.enter(); mutationErr == nil {
				native.canonicalReady = false
				native.object.mutation.leave()
			}
		}
		return CIREvent{}, false, err
	}
	if err := native.object.mutation.enter(); err != nil {
		return CIREvent{}, false, err
	}
	defer native.object.mutation.leave()
	native.canonicalReady = false
	complete, err := native.object.step(ctx, input, frame)
	if err != nil || !complete {
		return CIREvent{}, false, err
	}
	event := CIREvent(native.decoded)
	event.fieldPresence = native.object.presence
	native.canonicalReady = true
	return event, true, nil
}

func (native *legacyNormalizationNativeEvent) retained() (int64, error) {
	retained, err := native.object.retained()
	if err != nil {
		return 0, err
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*native)))
	estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
	return estimate.bytes, estimate.err
}
