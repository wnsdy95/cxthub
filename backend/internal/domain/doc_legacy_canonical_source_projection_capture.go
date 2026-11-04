package domain

import (
	"context"
	"reflect"
	"strconv"
)

const legacyCanonicalProjectionRootPosition = -1

type legacyCanonicalSourceProjectionCapture struct {
	source        *legacyCanonicalSourceCheckpoint
	record        *legacyNativeEventValue
	root          map[string]any
	keysPosition  int
	fieldPosition int
}

func (work *legacyCanonicalSourceCheckpoint) captureSourceProjection(ctx context.Context, keysPosition, fieldPosition int) (legacyCanonicalSourceProjectionCapture, error) {
	var capture legacyCanonicalSourceProjectionCapture
	if err := ctx.Err(); err != nil {
		return capture, err
	}
	if work == nil || work.closed || work.failure != nil || work.child == nil || work.child.closed || work.child.failure != nil {
		return capture, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		return capture, err
	}
	if err := work.child.checkSource(); err != nil {
		return capture, err
	}
	if err := work.admit(0); err != nil {
		return capture, err
	}
	var record *legacyNativeEventValue
	var root map[string]any
	if keysPosition == legacyCanonicalProjectionRootPosition {
		if fieldPosition != legacyCanonicalProjectionRootPosition || work.child.source == nil || work.child.source.count != 1 ||
			work.child.source.records.length() != 1 || !work.child.source.records.valid() {
			return capture, ErrIntegrity
		}
		record = work.child.source.records.at(0)
		root, _ = work.encoding.root.(map[string]any)
	} else {
		if keysPosition < 0 || !work.encoding.mapKeys.valid() {
			return capture, ErrIntegrity
		}
		keys := work.encoding.mapKeys.at(keysPosition)
		if keys == nil || !keys.fields.valid() {
			return capture, ErrIntegrity
		}
		replacement, valid := keys.source.(*legacyCanonicalReplacement)
		if !valid || replacement == nil || replacement.native == nil || replacement.events != nil ||
			!replacement.native.records.valid() || replacement.native.count < 0 || replacement.native.count > replacement.native.records.length() ||
			keys.fields.length() > replacement.native.count {
			return capture, ErrIntegrity
		}
		field := keys.fields.at(fieldPosition)
		if field == nil || len(field.key) != 32 {
			return capture, ErrIntegrity
		}
		for index := 0; index < len(field.key); index++ {
			character := field.key[index]
			if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
				return capture, ErrIntegrity
			}
		}
		ordinal, err := strconv.ParseUint(field.key[16:], 16, 64)
		if err != nil || ordinal >= uint64(replacement.native.count) {
			return capture, ErrIntegrity
		}
		record = replacement.native.at(int(ordinal))
		sequence, err := strconv.ParseUint(field.key[:16], 16, 64)
		if err != nil || record == nil || sequence != uint64(int64(record.event.Seq))^(uint64(1)<<63) {
			return capture, ErrIntegrity
		}
		root, _ = field.value.(map[string]any)
	}
	if record == nil || root == nil {
		return capture, ErrIntegrity
	}
	return legacyCanonicalSourceProjectionCapture{source: work, record: record, root: root, keysPosition: keysPosition, fieldPosition: fieldPosition}, nil
}

func (capture legacyCanonicalSourceProjectionCapture) reference(ctx context.Context, value any) (legacyCanonicalProjectionReference, bool, error) {
	var reference legacyCanonicalProjectionReference
	current, err := capture.source.captureSourceProjection(ctx, capture.keysPosition, capture.fieldPosition)
	if err != nil {
		return reference, false, err
	}
	if capture.record != current.record || reflect.ValueOf(capture.root).Pointer() != reflect.ValueOf(current.root).Pointer() {
		return reference, false, ErrConflict
	}
	source, record, err := capture.source.sourceRecordReference(ctx)
	if err != nil {
		return reference, false, err
	}
	if record != capture.record {
		return reference, false, nil
	}
	slot := capture.slot(value)
	if slot == "" {
		return reference, false, nil
	}
	return legacyCanonicalProjectionReference{Source: source, Slot: slot}, true, nil
}

func (capture legacyCanonicalSourceProjectionCapture) slot(value any) legacyCanonicalProjectionSlot {
	if capture.record == nil || capture.root == nil {
		return ""
	}
	switch current := value.(type) {
	case map[string]any:
		if current == nil {
			return ""
		}
		if reflect.ValueOf(current).Pointer() == reflect.ValueOf(capture.root).Pointer() {
			return legacyCanonicalProjectionRoot
		}
		for _, member := range [...]struct {
			key     string
			slot    legacyCanonicalProjectionSlot
			emitted bool
		}{
			{key: "provider_metadata", slot: legacyCanonicalProjectionMetadata, emitted: capture.record.event.ProviderMetadata != nil},
			{key: "locked", slot: legacyCanonicalProjectionLocked, emitted: capture.record.event.Locked != nil},
			{key: "input", slot: legacyCanonicalProjectionEmptyInput, emitted: capture.record.event.Kind == EventToolCall && capture.record.event.Input == nil},
		} {
			projected, valid := capture.root[member.key].(map[string]any)
			if member.emitted && valid && projected != nil && reflect.ValueOf(current).Pointer() == reflect.ValueOf(projected).Pointer() {
				return member.slot
			}
		}
	case *legacyNativeBlockValues:
		projected, valid := capture.root["blocks"].(*legacyNativeBlockValues)
		if current != nil && valid && current == projected && current == capture.record.blocks {
			return legacyCanonicalProjectionBlocks
		}
	case *legacyCanonicalReplacement:
		projected, valid := capture.root["replacement"].(*legacyCanonicalReplacement)
		if current != nil && valid && current == projected && current.native != nil && current.events == nil && current.native == capture.record.replacement {
			return legacyCanonicalProjectionReplacement
		}
	}
	return ""
}
