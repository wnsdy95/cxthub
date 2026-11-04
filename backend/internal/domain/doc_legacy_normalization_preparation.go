package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"sort"
	"unsafe"
)

const (
	legacyNormalizationDecode uint8 = iota
	legacyNormalizationStage
	legacyNormalizationRuns
	legacyNormalizationMerge
	legacyNormalizationPrefix
	legacyNormalizationCompose
	legacyNormalizationSuffix
	legacyNormalizationVerify
	legacyNormalizationReady
)

type legacyNormalizationRecord struct {
	sequence int64
	ordinal  uint64
	offset   uint64
	length   uint64
	hash     [sha256.Size]byte
}

func (record legacyNormalizationRecord) bytes() []byte {
	raw := make([]byte, legacyNormalizationRecordBytes)
	binary.BigEndian.PutUint64(raw[0:8], uint64(record.sequence))
	binary.BigEndian.PutUint64(raw[8:16], record.ordinal)
	binary.BigEndian.PutUint64(raw[16:24], record.offset)
	binary.BigEndian.PutUint64(raw[24:32], record.length)
	copy(raw[32:], record.hash[:])
	return raw
}

func legacyNormalizationLess(left, right legacyNormalizationRecord) bool {
	return left.sequence < right.sequence || left.sequence == right.sequence && left.ordinal < right.ordinal
}

type LegacyNormalizationPreparation struct {
	mutation     *legacyNormalizationMutation
	decoder      *LegacyNormalizationDecoder
	files        [LegacyNormalizationSlots]legacyNormalizationFile
	phase        uint8
	pending      []LegacyNormalizedEvent
	next         int
	written      int
	count        int
	header       []byte
	headerSource *LegacyByteSource
	prefixBytes  int
	hashState    []byte
	active       legacyNormalizationRecord
	position     int
	sourceSlot   int
	targetSlot   int
	width        int
	base         int
	left         int
	right        int
	segment      int
	segmentPos   int
	verified     int
	terminal     error
}

func NewLegacyNormalizationPreparation(ctx context.Context, source LegacyByteSource, storage LegacyNormalizationStorage) (*LegacyNormalizationPreparation, error) {
	decoder, err := NewLegacyNormalizationDecoder(ctx, source)
	if err != nil {
		return nil, err
	}
	if storage == nil {
		return nil, ErrIntegrity
	}
	work := &LegacyNormalizationPreparation{mutation: decoder.mutation, decoder: decoder}
	for slot := range work.files {
		work.files[slot] = legacyNormalizationFile{storage: storage, slot: slot}
		if err := work.files[slot].reset(ctx); err != nil {
			return nil, err
		}
	}
	return work, nil
}

func (work *LegacyNormalizationPreparation) decode(ctx context.Context) error {
	step, err := work.decoder.Step(ctx)
	if err != nil {
		return err
	}
	if step.Reset {
		for _, slot := range []int{0, 1} {
			if err := work.files[slot].reset(ctx); err != nil {
				return err
			}
		}
		work.count = 0
	}
	if step.Complete {
		if step.Count != work.count || len(step.Events) != 0 || len(step.Envelope) == 0 {
			return ErrIntegrity
		}
		work.header = bytes.Clone(step.Envelope)
		work.decoder = nil
		for _, slot := range []int{0, 1} {
			if err := work.files[slot].flush(ctx); err != nil {
				return err
			}
			if err := work.files[slot].eof(ctx); err != nil {
				return err
			}
		}
		work.sourceSlot, work.targetSlot = 1, 2
		work.phase = legacyNormalizationRuns
		return nil
	}
	if len(step.Events) != 0 {
		work.pending, work.next = step.Events, 0
		work.phase = legacyNormalizationStage
	}
	return nil
}

func (work *LegacyNormalizationPreparation) stage(ctx context.Context) error {
	remaining := ConversationManifestChunkBytes
	for work.next < len(work.pending) && remaining >= LegacyCanonicalEncodingBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		event := &work.pending[work.next]
		digest := sha256.New()
		if work.written == 0 {
			if event.Encoding == nil {
				return ErrIntegrity
			}
			work.active = legacyNormalizationRecord{sequence: int64(event.Sequence), ordinal: uint64(work.count), offset: uint64(work.files[0].size)}
		} else if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(work.hashState); err != nil {
			return ErrIntegrity
		}
		preparing := event.Encoding.preparation != nil
		raw, complete, err := event.Encoding.Next(ctx)
		if err != nil {
			if preparing && ctx.Err() == nil && work.decoder != nil && work.written == 0 {
				work.decoder.invalid = true
				*event = LegacyNormalizedEvent{}
				work.next++
				work.hashState, work.active = nil, legacyNormalizationRecord{}
				if work.next == len(work.pending) {
					work.pending = nil
					work.phase = legacyNormalizationDecode
				}
				return nil
			}
			return err
		}
		if preparing {
			remaining -= LegacyCanonicalEncodingBytes
		}
		if len(raw) > LegacyCanonicalEncodingBytes || len(raw) > remaining {
			return ErrIntegrity
		}
		digest.Write(raw)
		if err := work.files[0].append(ctx, raw); err != nil {
			return err
		}
		work.written += len(raw)
		remaining -= len(raw)
		if complete {
			if work.written == 0 {
				return ErrIntegrity
			}
			work.active.length = uint64(work.written)
			copy(work.active.hash[:], digest.Sum(nil))
			if err := work.files[1].append(ctx, work.active.bytes()); err != nil {
				return err
			}
			*event = LegacyNormalizedEvent{}
			work.next++
			work.count++
			work.written, work.hashState = 0, nil
		} else {
			var err error
			work.hashState, err = digest.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				return err
			}
			if preparing && event.Encoding.preparation == nil {
				continue
			}
			return ctx.Err()
		}
	}
	if work.next == len(work.pending) {
		work.pending = nil
		work.phase = legacyNormalizationDecode
	}
	return ctx.Err()
}

func (work *LegacyNormalizationPreparation) record(ctx context.Context, slot, position int, window *legacyNormalizationWindow) (legacyNormalizationRecord, error) {
	if position < 0 || position >= work.count || position > int(^uint(0)>>1)/legacyNormalizationRecordBytes {
		return legacyNormalizationRecord{}, ErrPreparedDocRangeObject
	}
	raw, err := window.read(ctx, &work.files[slot], position*legacyNormalizationRecordBytes, legacyNormalizationRecordBytes)
	if err != nil {
		return legacyNormalizationRecord{}, err
	}
	record := legacyNormalizationRecord{sequence: int64(binary.BigEndian.Uint64(raw[:8])), ordinal: binary.BigEndian.Uint64(raw[8:16]), offset: binary.BigEndian.Uint64(raw[16:24]), length: binary.BigEndian.Uint64(raw[24:32])}
	copy(record.hash[:], raw[32:])
	if int64(int(record.sequence)) != record.sequence || record.ordinal >= uint64(work.count) || record.offset > uint64(work.files[0].size) || record.length == 0 || record.length > uint64(work.files[0].size)-record.offset {
		return legacyNormalizationRecord{}, ErrPreparedDocRangeObject
	}
	return record, nil
}

func (work *LegacyNormalizationPreparation) runs(ctx context.Context) error {
	var window legacyNormalizationWindow
	end := min(work.count, work.position+legacyNormalizationRunRecords)
	records := make([]legacyNormalizationRecord, 0, end-work.position)
	for ordinal := work.position; ordinal < end; ordinal++ {
		record, err := work.record(ctx, work.sourceSlot, ordinal, &window)
		if err != nil || record.ordinal != uint64(ordinal) {
			return errors.Join(ErrPreparedDocRangeObject, err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool { return legacyNormalizationLess(records[left], records[right]) })
	for _, record := range records {
		if err := work.files[work.targetSlot].append(ctx, record.bytes()); err != nil {
			return err
		}
	}
	work.position = end
	if work.position == work.count {
		if err := work.finishOrderPass(ctx); err != nil {
			return err
		}
		work.width = legacyNormalizationRunRecords
		if work.count <= work.width {
			work.phase = legacyNormalizationPrefix
		} else {
			work.phase = legacyNormalizationMerge
		}
	}
	return ctx.Err()
}

func (work *LegacyNormalizationPreparation) finishOrderPass(ctx context.Context) error {
	file := &work.files[work.targetSlot]
	if err := file.flush(ctx); err != nil {
		return err
	}
	if work.count > int(^uint(0)>>1)/legacyNormalizationRecordBytes || file.size != work.count*legacyNormalizationRecordBytes {
		return ErrPreparedDocRangeObject
	}
	if err := file.eof(ctx); err != nil {
		return err
	}
	work.sourceSlot, work.targetSlot = work.targetSlot, work.sourceSlot
	work.position, work.base, work.left, work.right = 0, 0, 0, 0
	return work.files[work.targetSlot].reset(ctx)
}

func (work *LegacyNormalizationPreparation) merge(ctx context.Context) error {
	var window legacyNormalizationWindow
	for processed := 0; processed < legacyNormalizationRunRecords && work.position < work.count; processed++ {
		if work.left == 0 && work.right == 0 {
			work.left = work.base
			work.right = min(work.base+work.width, work.count)
		}
		leftEnd, rightEnd := min(work.base+work.width, work.count), min(work.base+2*work.width, work.count)
		var selected legacyNormalizationRecord
		if work.left < leftEnd {
			var err error
			selected, err = work.record(ctx, work.sourceSlot, work.left, &window)
			if err != nil {
				return err
			}
		}
		if work.right < rightEnd {
			right, err := work.record(ctx, work.sourceSlot, work.right, &window)
			if err != nil {
				return err
			}
			if work.left == leftEnd || legacyNormalizationLess(right, selected) {
				selected = right
				work.right++
			} else {
				work.left++
			}
		} else {
			work.left++
		}
		if err := work.files[work.targetSlot].append(ctx, selected.bytes()); err != nil {
			return err
		}
		work.position++
		if work.left == leftEnd && work.right == rightEnd {
			work.base = rightEnd
			work.left, work.right = 0, 0
		}
	}
	if work.position == work.count {
		if err := work.finishOrderPass(ctx); err != nil {
			return err
		}
		if work.width >= work.count-work.width {
			work.phase = legacyNormalizationPrefix
		} else {
			work.width *= 2
		}
	}
	return ctx.Err()
}

func (work *LegacyNormalizationPreparation) prefix(ctx context.Context) error {
	segments := [][]byte{[]byte(canonicalDocPrefix), work.header, []byte(canonicalDocMiddle)}
	remaining := ConversationManifestChunkBytes
	for work.segment < len(segments) && remaining > 0 {
		segment := segments[work.segment]
		segmentBytes := len(segment)
		if work.segment == 1 && work.headerSource != nil {
			segmentBytes = work.headerSource.Bytes
		}
		length := min(remaining, segmentBytes-work.segmentPos)
		var body []byte
		if work.segment == 1 && work.headerSource != nil {
			length = min(length, LegacyCanonicalEncodingBytes)
			var err error
			body, err = work.headerSource.Read(ctx, work.segmentPos, length)
			if err != nil || len(body) != length {
				return errors.Join(ErrPreparedDocRangeObject, err)
			}
			if work.mutation.closed.Load() {
				return ErrConflict
			}
		} else {
			body = segment[work.segmentPos : work.segmentPos+length]
		}
		if err := work.files[3].append(ctx, body); err != nil {
			return err
		}
		remaining -= length
		work.segmentPos += length
		if work.segmentPos == segmentBytes {
			work.segment++
			work.segmentPos = 0
		}
	}
	if work.segment == len(segments) {
		work.header = nil
		work.headerSource = nil
		work.prefixBytes = work.files[3].size
		work.phase = legacyNormalizationCompose
	}
	return ctx.Err()
}

func (work *LegacyNormalizationPreparation) compose(ctx context.Context) error {
	var window legacyNormalizationWindow
	remaining := ConversationManifestChunkBytes
	for events := 0; events < LegacyNormalizationStepEvents && remaining > 0 && work.position < work.count; events++ {
		digest := sha256.New()
		if work.written == 0 {
			record, err := work.record(ctx, work.sourceSlot, work.position, &window)
			if err != nil {
				return err
			}
			work.active = record
			if work.position > 0 {
				if remaining < 2 {
					break
				}
				if err := work.files[3].append(ctx, []byte(",")); err != nil {
					return err
				}
				remaining--
			}
		} else if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(work.hashState); err != nil {
			return ErrIntegrity
		}
		length := min(remaining, int(work.active.length)-work.written)
		raw, err := work.files[0].exact(ctx, int(work.active.offset)+work.written, length)
		if err != nil {
			return err
		}
		digest.Write(raw)
		if err := work.files[3].append(ctx, raw); err != nil {
			return err
		}
		work.written += length
		remaining -= length
		if work.written == int(work.active.length) {
			if !bytes.Equal(digest.Sum(nil), work.active.hash[:]) {
				return ErrPreparedDocRangeObject
			}
			work.position++
			work.written, work.hashState = 0, nil
		} else {
			work.hashState, err = digest.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				return err
			}
		}
	}
	if work.position == work.count {
		if err := work.files[0].eof(ctx); err != nil {
			return err
		}
		work.phase = legacyNormalizationSuffix
	}
	return ctx.Err()
}

func (work *LegacyNormalizationPreparation) Step(ctx context.Context) (complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil {
		return false, ErrIntegrity
	}
	if work.mutation == nil {
		return false, ErrIntegrity
	}
	if !work.mutation.stepping.CompareAndSwap(false, true) {
		return false, ErrLegacyNormalizationBusy
	}
	defer work.mutation.stepping.Store(false)
	if err := work.mutation.enter(); err != nil {
		return false, err
	}
	defer work.mutation.leave()
	if work.terminal != nil {
		return false, work.terminal
	}
	wasReady := work.phase == legacyNormalizationReady
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			complete = false
			if !wasReady {
				work.terminal = failure
			}
		}
	}()
	switch work.phase {
	case legacyNormalizationDecode:
		failure = work.decode(ctx)
	case legacyNormalizationStage:
		failure = work.stage(ctx)
	case legacyNormalizationRuns:
		failure = work.runs(ctx)
	case legacyNormalizationMerge:
		failure = work.merge(ctx)
	case legacyNormalizationPrefix:
		failure = work.prefix(ctx)
	case legacyNormalizationCompose:
		failure = work.compose(ctx)
	case legacyNormalizationSuffix:
		failure = work.files[3].append(ctx, []byte(canonicalDocSuffix))
		if failure == nil {
			failure = work.files[3].flush(ctx)
		}
		if failure == nil {
			work.phase = legacyNormalizationVerify
		}
	case legacyNormalizationVerify:
		file := &work.files[3]
		if work.verified < len(file.parts) {
			part := file.parts[work.verified]
			var raw []byte
			raw, failure = file.exact(ctx, work.verified*ConversationManifestChunkBytes, int(part.Bytes))
			if failure == nil {
				var hash ContentHash
				hash, failure = HashContentContext(ctx, raw)
				if failure == nil && hash != part.Hash {
					failure = ErrPreparedDocRangeObject
				}
			}
			if failure == nil {
				work.verified++
			}
		} else {
			failure = file.eof(ctx)
			if failure == nil {
				work.phase = legacyNormalizationReady
				for slot := 0; slot < 3; slot++ {
					work.files[slot].parts = nil
					work.files[slot].partHeap = docRangeHeaderHeapEstimate{}
				}
			}
		}
	case legacyNormalizationReady:
	default:
		failure = ErrIntegrity
	}
	return work.phase == legacyNormalizationReady && failure == nil, failure
}

func (work *LegacyNormalizationPreparation) InputConsumed() (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if err := work.mutation.enter(); err != nil {
		return false, err
	}
	defer work.mutation.leave()
	if work.terminal != nil {
		return false, work.terminal
	}
	if work.phase > legacyNormalizationReady || work.phase < legacyNormalizationRuns && work.decoder == nil {
		return false, ErrIntegrity
	}
	if work.phase < legacyNormalizationRuns {
		return false, nil
	}
	if work.decoder != nil || work.pending != nil {
		return false, ErrIntegrity
	}
	return true, nil
}

func (work *LegacyNormalizationPreparation) Source() (LegacyByteSource, error) {
	if work == nil || work.terminal != nil || work.phase != legacyNormalizationReady {
		return LegacyByteSource{}, ErrDocRangeIndexPending
	}
	file := &work.files[3]
	return LegacyByteSource{Bytes: file.size, Parts: append([]ConversationManifestChunk(nil), file.parts...),
		Read: func(ctx context.Context, offset, length int) ([]byte, error) {
			return file.storage.Read(ctx, file.slot, offset, length)
		}}, nil
}

func (work *LegacyNormalizationPreparation) Close() {
	if work != nil {
		work.mutation.stop()
	}
}

func (work *LegacyNormalizationPreparation) RetainedBytes() (int64, error) {
	if work == nil {
		return 0, ErrIntegrity
	}
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(*work)))
	if work.mutation != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*work.mutation)))
	}
	if work.decoder != nil {
		retained, err := work.decoder.RetainedBytes()
		if err != nil {
			return 0, err
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
	}
	estimate.allocation(int64(cap(work.header)+cap(work.hashState)), 1)
	if work.headerSource != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*work.headerSource)))
	}
	estimate.allocation(int64(cap(work.pending)), int64(unsafe.Sizeof(LegacyNormalizedEvent{})))
	for _, event := range work.pending[:cap(work.pending)] {
		if event.Encoding != nil {
			retained, err := event.Encoding.RetainedBytes()
			if err != nil {
				return 0, err
			}
			estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
		}
	}
	for _, file := range work.files {
		estimate.allocation(int64(cap(file.buffer)+cap(file.hashState)), 1)
		estimate.allocation(int64(cap(file.parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
		if file.partHeap.err != nil {
			return 0, file.partHeap.err
		}
		estimate.include(file.partHeap)
	}
	return estimate.bytes, estimate.err
}
