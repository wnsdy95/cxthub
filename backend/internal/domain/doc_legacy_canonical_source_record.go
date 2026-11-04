package domain

import "context"

type legacyCanonicalSourceRecordReference struct {
	Record uint64                    `json:"record"`
	Fields legacyNativeCheckpointRef `json:"fields"`
}

func (reference legacyCanonicalSourceRecordReference) validate() error {
	if reference.Record == 0 || reference.Record > 8*uint64(MaxFinalizedDocBytes) || !reference.Fields.valid() {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyCanonicalSourceCheckpoint) sourceRecord() *legacyNativeEventValuesCheckpointFrame {
	if work == nil || work.closed || work.failure != nil || work.child == nil || work.child.closed || work.child.failure != nil {
		return nil
	}
	frame := work.child.current
	if frame == nil || frame.phase != 7 || frame.recordRow == 0 || !frame.fields.valid() {
		return nil
	}
	return frame
}

func (work *legacyCanonicalSourceCheckpoint) sourceRecordReference(ctx context.Context) (legacyCanonicalSourceRecordReference, *legacyNativeEventValue, error) {
	var reference legacyCanonicalSourceRecordReference
	if err := ctx.Err(); err != nil {
		return reference, nil, err
	}
	frame := work.sourceRecord()
	if frame == nil {
		return reference, nil, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		return reference, nil, err
	}
	if err := work.child.checkSource(); err != nil {
		return reference, nil, err
	}
	if err := work.admit(0); err != nil {
		return reference, nil, err
	}
	if frame.source == nil || frame.index < 0 || frame.index >= frame.source.records.length() ||
		frame.recordRow > 8*uint64(work.child.state.SourceBytes) {
		return reference, nil, ErrIntegrity
	}
	record := frame.source.records.at(frame.index)
	if record == nil || record.heap != frame.record.heap || record.replacement != frame.record.replacement || record.blocks != frame.record.blocks {
		return reference, nil, ErrConflict
	}
	reference = legacyCanonicalSourceRecordReference{Record: frame.recordRow, Fields: frame.fields}
	if err := reference.validate(); err != nil {
		return legacyCanonicalSourceRecordReference{}, nil, err
	}
	return reference, record, nil
}

func (work *legacyCanonicalInitialRestore) sourceRecord() *legacyNativeEventValuesRestoreFrame {
	if work == nil || work.closed || work.failure != nil || work.child == nil || work.child.closed || work.child.failure != nil {
		return nil
	}
	frame := work.child.current
	if frame == nil || frame.phase != 7 || frame.recordRow == 0 || !frame.fields.valid() {
		return nil
	}
	return frame
}

func (work *legacyCanonicalInitialRestore) resolveSourceRecord(ctx context.Context, reference legacyCanonicalSourceRecordReference) (legacyNativeEventValue, bool, error) {
	var record legacyNativeEventValue
	if err := ctx.Err(); err != nil {
		return record, false, err
	}
	if err := reference.validate(); err != nil {
		return record, false, err
	}
	frame := work.sourceRecord()
	if frame == nil {
		return record, false, ErrIntegrity
	}
	if err := work.admit(0); err != nil {
		return record, false, err
	}
	if reference.Record != frame.recordRow {
		return record, false, nil
	}
	if reference.Fields != frame.fields || frame.recordRow > 8*uint64(work.child.sourceBytes) || frame.record.heap.err != nil ||
		frame.record.heap.bytes < 0 || frame.record.excess < 0 || frame.record.excess > frame.record.heap.bytes {
		return record, false, ErrIntegrity
	}
	return frame.record, true, nil
}
