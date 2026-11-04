package domain

import (
	"context"
	"reflect"
	"strconv"
	"unsafe"
)

type legacyEnvelopeScalarCheckpointControl struct {
	Kind     byte `json:"kind"`
	Phase    byte `json:"phase"`
	Position int  `json:"position"`
}

func legacyEnvelopeScalarControlFor(encoding *LegacyEnvelopeEncoding) (*legacyEnvelopeScalarCheckpointControl, error) {
	if encoding == nil {
		return nil, ErrIntegrity
	}
	if encoding.scalar == nil {
		return nil, nil
	}
	root, err := legacyEnvelopeScalarRoot(encoding)
	if err != nil {
		return nil, err
	}
	scalar := encoding.scalar
	if scalar.err != nil {
		return nil, scalar.err
	}
	if !scalar.initialized || scalar.complete || scalar.nativeOwner != nil || scalar.preparation != nil ||
		scalar.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) ||
		scalar.pendingKeysHeap != (docRangeHeaderHeapEstimate{}) || scalar.order != (legacyCanonicalKeyOrder{}) ||
		scalar.keyWork != 0 || scalar.ordering || scalar.numbers != (legacyCanonicalNumberValidation{}) ||
		scalar.eagerNumbers || scalar.inspectWork != 2 || scalar.lastStorageBytes != 0 ||
		!scalar.frames.valid() || scalar.frames.single == nil || scalar.frames.length() != 1 ||
		!legacyEnvelopeScalarSameRoot(scalar.root, root) {
		return nil, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	switch value := root.(type) {
	case string:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
		heap.allocation(int64(len(value)), 1)
	case int:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	}
	if heap.err != nil || scalar.rootHeap != heap {
		return nil, ErrIntegrity
	}
	frame := scalar.frames.at(0)
	if !legacyEnvelopeScalarSameRoot(frame.value, root) || frame.block != (ContentBlock{}) ||
		frame.fields != (legacyCanonicalPages[legacyCanonicalKeyField]{}) || frame.keysPosition != 0 ||
		frame.keysEnd != 0 || frame.ordered {
		return nil, ErrIntegrity
	}
	state := &legacyEnvelopeScalarCheckpointControl{Kind: frame.kind, Phase: frame.phase, Position: frame.position}
	text, owned, err := state.scalarText(root)
	if err != nil {
		return nil, err
	}
	if frame.ownedLiteral != owned || owned && frame.text != text || !owned && !legacyEnvelopeScalarSameRoot(frame.text, text) {
		return nil, ErrIntegrity
	}
	return state, nil
}

func restoreLegacyEnvelopeScalar(ctx context.Context, encoding *LegacyEnvelopeEncoding, state *legacyEnvelopeScalarCheckpointControl) (*LegacyCanonicalEncoding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state == nil {
		return nil, nil
	}
	root, err := legacyEnvelopeScalarRoot(encoding)
	if err != nil {
		return nil, err
	}
	text, owned, err := state.scalarText(root)
	if err != nil {
		return nil, err
	}
	scalar, err := newLegacyCanonicalEncodingPending(ctx, root)
	if err != nil {
		return nil, err
	}
	frame := scalar.frames.at(0)
	frame.kind, frame.text, frame.ownedLiteral = state.Kind, text, owned
	frame.phase, frame.position = state.Phase, state.Position
	if err := ctx.Err(); err != nil {
		scalar.release()
		return nil, err
	}
	return scalar, nil
}

func legacyEnvelopeScalarRoot(encoding *LegacyEnvelopeEncoding) (any, error) {
	if encoding == nil || !encoding.initialized || encoding.complete {
		return nil, ErrIntegrity
	}
	if encoding.err != nil {
		return nil, encoding.err
	}
	field := encoding.field
	switch encoding.phase {
	case legacyEnvelopeEncodingFieldNext:
		if field <= 0 || field > len(encoding.fields) {
			return nil, ErrIntegrity
		}
		field--
	case legacyEnvelopeEncodingModelNext:
		if field < 0 || field >= len(encoding.fields) || encoding.model <= 0 {
			return nil, ErrIntegrity
		}
	default:
		return nil, ErrIntegrity
	}
	value := reflect.ValueOf(&encoding.envelope).Elem()
	index := encoding.fields[field].index
	if index < 0 || index >= value.NumField() {
		return nil, ErrIntegrity
	}
	member := value.Field(index)
	if encoding.phase == legacyEnvelopeEncodingModelNext {
		if member.Type() != reflect.TypeFor[[]string]() || encoding.model > member.Len() {
			return nil, ErrIntegrity
		}
		return member.Index(encoding.model - 1).String(), nil
	}
	switch member.Kind() {
	case reflect.String:
		return member.String(), nil
	case reflect.Int:
		return int(member.Int()), nil
	case reflect.Slice:
		if member.Type() == reflect.TypeFor[[]string]() && member.IsNil() {
			return nil, nil
		}
	}
	return nil, ErrIntegrity
}

func legacyEnvelopeScalarSameRoot(value, root any) bool {
	switch expected := root.(type) {
	case nil:
		return value == nil
	case string:
		text, ok := value.(string)
		return ok && len(text) == len(expected) && (len(text) == 0 || unsafe.StringData(text) == unsafe.StringData(expected))
	case int:
		number, ok := value.(int)
		return ok && number == expected
	default:
		return false
	}
}

func (state legacyEnvelopeScalarCheckpointControl) scalarText(root any) (string, bool, error) {
	if state.Position < 0 {
		return "", false, ErrIntegrity
	}
	switch state.Kind {
	case 0:
		if state.Phase == 0 && state.Position == 0 {
			return "", false, nil
		}
	case 's':
		text, ok := root.(string)
		if ok && state.Phase <= 1 && state.Position <= len(text) && (state.Phase == 1 || state.Position == 0) {
			return text, false, nil
		}
	case 'l':
		text := "null"
		switch value := root.(type) {
		case nil:
		case int:
			text = strconv.Itoa(value)
		default:
			return "", false, ErrIntegrity
		}
		if state.Phase == 0 && state.Position < len(text) {
			return text, true, nil
		}
	}
	return "", false, ErrIntegrity
}
