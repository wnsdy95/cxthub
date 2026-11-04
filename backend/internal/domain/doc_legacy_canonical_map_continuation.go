package domain

import (
	"context"
	"reflect"
	"unsafe"
)

type legacyCanonicalMapContinuation struct {
	fields   legacyCanonicalPages[legacyCanonicalKeyField]
	position int
}

func (frame *legacyCanonicalPreparationFrame) nextMapValue() (string, any, bool, error) {
	if frame.mapResume == nil {
		if frame.iterator == nil {
			return "", nil, false, ErrIntegrity
		}
		if !frame.iterator.Next() {
			return "", nil, false, nil
		}
		return frame.iterator.Key().String(), frame.iterator.Value().Interface(), true, nil
	}
	resume := frame.mapResume
	if frame.iterator != nil || !resume.fields.valid() || resume.position < 0 || resume.position > resume.fields.length() {
		return "", nil, false, ErrIntegrity
	}
	if resume.position == resume.fields.length() {
		return "", nil, false, nil
	}
	field := resume.fields.at(resume.position)
	if field == nil {
		return "", nil, false, ErrIntegrity
	}
	resume.position++
	return field.key, field.value, true, nil
}

type legacyCanonicalMapCapture struct {
	owner        *LegacyNormalizationPreparation
	encoding     *LegacyCanonicalEncoding
	preparation  *legacyCanonicalPreparation
	frame        *legacyCanonicalPreparationFrame
	grant        *legacyNormalizationCapture
	iterator     *reflect.MapIter
	resume       *legacyCanonicalMapContinuation
	fields       legacyCanonicalPages[legacyCanonicalKeyField]
	initial      int
	expected     int
	sourceCursor int
	baseHeap     int64
	budget       int64
	ready        bool
	closed       bool
}

func newLegacyCanonicalMapCapture(ctx context.Context, owner *LegacyNormalizationPreparation, index int, grant *legacyNormalizationCapture, budget int64) (*legacyCanonicalMapCapture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == nil || grant == nil || !grant.valid(owner.mutation) {
		return nil, ErrConflict
	}
	if owner.phase != legacyNormalizationStage || owner.terminal != nil || owner.next < 0 || owner.next >= len(owner.pending) {
		return nil, ErrIntegrity
	}
	encoding := owner.pending[owner.next].Encoding
	if encoding == nil || encoding.preparation == nil || index < 0 || index >= encoding.preparation.frames.length() {
		return nil, ErrIntegrity
	}
	frame := encoding.preparation.frames.at(index)
	if frame == nil || frame.kind != 'o' || frame.phase != 0 || frame.position < 0 || frame.position > frame.count ||
		(frame.iterator == nil) == (frame.mapResume == nil) {
		return nil, ErrIntegrity
	}
	value, valid := frame.value.(map[string]any)
	if !valid || value == nil || len(value) != frame.count {
		return nil, ErrIntegrity
	}
	retained, err := owner.RetainedBytes()
	if err != nil {
		return nil, err
	}
	var heap docRangeHeaderHeapEstimate
	heap.include(docRangeHeaderHeapEstimate{bytes: retained})
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalMapCapture{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalMapContinuation{})))
	if frame.iterator != nil {
		heap.allocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
	}
	if heap.err != nil || budget <= 0 || heap.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyCanonicalMapCapture{owner: owner, encoding: encoding, preparation: encoding.preparation, frame: frame, grant: grant,
		initial: frame.position, expected: frame.count - frame.position, baseHeap: heap.bytes, budget: budget}
	if frame.iterator != nil {
		iterator := *frame.iterator
		work.iterator = &iterator
	} else {
		resume := frame.mapResume
		if !resume.fields.valid() || resume.position < 0 || resume.position > resume.fields.length() || resume.fields.length()-resume.position != work.expected {
			work.close()
			return nil, ErrIntegrity
		}
		work.resume, work.sourceCursor = resume, resume.position
	}
	return work, nil
}

func (work *legacyCanonicalMapCapture) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work == nil || work.closed || work.grant == nil || !work.grant.valid(work.owner.mutation) {
		return ErrConflict
	}
	if work.owner.phase != legacyNormalizationStage || work.owner.next < 0 || work.owner.next >= len(work.owner.pending) ||
		work.owner.pending[work.owner.next].Encoding != work.encoding || work.encoding.preparation != work.preparation || work.frame.position != work.initial ||
		work.frame.count-work.initial != work.expected || work.frame.kind != 'o' || work.frame.phase != 0 ||
		(work.frame.mapResume != nil) != (work.resume != nil) || work.frame.mapResume != work.resume {
		return ErrConflict
	}
	return nil
}

func (work *legacyCanonicalMapCapture) step(ctx context.Context) (bool, error) {
	if err := work.current(ctx); err != nil {
		return false, err
	}
	if work.ready {
		return true, nil
	}
	storageBytes := 0
	for actions := 0; actions < legacyCanonicalEncodingActions; actions++ {
		if err := work.current(ctx); err != nil {
			return false, err
		}
		if work.fields.length() == work.expected {
			if work.iterator != nil && work.iterator.Next() {
				return false, ErrIntegrity
			}
			work.iterator, work.ready = nil, true
			return true, nil
		}
		additional, forecast, err := work.fields.appendRequirements()
		if err != nil {
			return false, err
		}
		if additional+int(unsafe.Sizeof(legacyCanonicalKeyField{})) > LegacyCanonicalEncodingBytes-storageBytes {
			return false, nil
		}
		var heap docRangeHeaderHeapEstimate
		heap.include(docRangeHeaderHeapEstimate{bytes: work.baseHeap})
		heap.include(forecast)
		if heap.err != nil || heap.bytes > work.budget {
			return false, ErrDocumentResourceLimit
		}
		var field legacyCanonicalKeyField
		if work.iterator != nil {
			if !work.iterator.Next() {
				return false, ErrIntegrity
			}
			field.key, field.value = work.iterator.Key().String(), work.iterator.Value().Interface()
		} else {
			source := work.resume.fields.at(work.sourceCursor + work.fields.length())
			if source == nil {
				return false, ErrIntegrity
			}
			field = *source
		}
		if err := work.fields.append(field); err != nil {
			return false, err
		}
		storageBytes += additional + int(unsafe.Sizeof(legacyCanonicalKeyField{}))
	}
	return false, nil
}

func (work *legacyCanonicalMapCapture) take(ctx context.Context) (*legacyCanonicalMapContinuation, error) {
	if err := work.current(ctx); err != nil {
		return nil, err
	}
	if !work.ready || !work.fields.valid() || work.fields.length() != work.expected {
		return nil, ErrDocRangeIndexPending
	}
	resume := &legacyCanonicalMapContinuation{fields: work.fields}
	work.fields = legacyCanonicalPages[legacyCanonicalKeyField]{}
	work.close()
	return resume, nil
}

func (work *legacyCanonicalMapCapture) close() {
	if work != nil {
		*work = legacyCanonicalMapCapture{closed: true}
	}
}
