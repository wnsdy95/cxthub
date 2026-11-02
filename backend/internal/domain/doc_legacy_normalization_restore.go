package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"unsafe"
)

type LegacyNormalizationCheckpointSource struct {
	Header     LegacyByteSource
	ReadBuffer func(context.Context, int) ([]byte, error)
	ReadParts  func(context.Context, int, int, int) ([]ConversationManifestChunk, error)
	Parser     *LegacyNormalizationParserCheckpointSource
	Encoder    *LegacyNormalizationParserCheckpointSource
}

type LegacyNormalizationRestore struct {
	control       LegacyNormalizationControl
	source        LegacyNormalizationCheckpointSource
	work          *LegacyNormalizationPreparation
	slot          int
	partsBytes    int
	pageChecked   int
	eventChecked  int
	recordChecked bool
	verifiedPart  int
	verifiedBytes int
	composed      int
	composedBytes int64
	orderWindow   legacyNormalizationWindow
	hashState     []byte
	ready         bool
	closed        atomic.Bool
	busy          atomic.Bool
	terminal      error
	parser        *legacyNormalizationDecoderCheckpointRestore
	parserBudget  int64
	stage         *legacyNormalizationStageRestore
}

func NewLegacyNormalizationRestore(ctx context.Context, control LegacyNormalizationControl, source LegacyNormalizationCheckpointSource, storage LegacyNormalizationStorage) (*LegacyNormalizationRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := control.Validate(); err != nil {
		return nil, err
	}
	if storage == nil || source.Header.Bytes != control.HeaderBytes || control.HeaderBytes > 0 && source.Header.Read == nil {
		return nil, ErrIntegrity
	}
	if (source.Parser != nil) != control.InputPending() || (source.Encoder != nil) != control.Staging() {
		return nil, ErrIntegrity
	}
	for _, file := range control.Files {
		if file.BufferBytes > 0 && source.ReadBuffer == nil || file.Parts > 0 && source.ReadParts == nil {
			return nil, ErrIntegrity
		}
	}
	control = control.clone()
	work := &LegacyNormalizationPreparation{mutation: &legacyNormalizationMutation{}, phase: control.Phase,
		count: control.Count, written: control.Written, hashState: bytes.Clone(control.HashState),
		prefixBytes: control.PrefixBytes,
		active: legacyNormalizationRecord{sequence: control.Active.Sequence, ordinal: control.Active.Ordinal,
			offset: control.Active.Offset, length: control.Active.Length, hash: control.Active.Hash},
		position: control.Position, sourceSlot: control.SourceSlot, targetSlot: control.TargetSlot, width: control.Width,
		base: control.Base, left: control.Left, right: control.Right, segment: control.Segment, segmentPos: control.SegmentPos, verified: control.Verified}
	if control.HeaderBytes > 0 {
		work.headerSource = &LegacyByteSource{Bytes: control.HeaderBytes, Read: source.Header.Read}
	}
	for slot, file := range control.Files {
		work.files[slot] = legacyNormalizationFile{storage: storage, slot: slot, generation: file.Generation,
			size: file.Size, pageBytes: file.PageBytes, hashState: bytes.Clone(file.HashState)}
	}
	source.Header = LegacyByteSource{Bytes: control.HeaderBytes, Read: source.Header.Read}
	restore := &LegacyNormalizationRestore{control: control, source: source, work: work}
	if source.Parser != nil {
		parser := *source.Parser
		restore.source.Parser = nil
		retained, err := restore.retainedBytes()
		if err != nil || parser.Budget <= retained {
			restore.Close()
			if err != nil {
				return nil, err
			}
			return nil, ErrDocumentResourceLimit
		}
		restore.parser, err = newLegacyNormalizationDecoderCheckpointRestore(ctx,
			legacyNativeCheckpointRef{Hash: parser.Ref.Hash, Bytes: parser.Ref.Bytes},
			legacyNativeCheckpointIO{Put: parser.Store.Put, Read: parser.Store.Read}, parser.Source, parser.Binding, parser.Budget-retained)
		if err != nil {
			restore.Close()
			return nil, err
		}
		restore.parserBudget = parser.Budget
	}
	if source.Encoder != nil && (source.Encoder.Budget != restore.parserBudget || source.Parser == nil ||
		source.Encoder.Binding != source.Parser.Binding) {
		restore.Close()
		return nil, ErrIntegrity
	}
	return restore, nil
}

func (restore *LegacyNormalizationRestore) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if restore.closed.Load() || restore.work == nil {
		return ErrConflict
	}
	return nil
}

func (restore *LegacyNormalizationRestore) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if restore == nil || restore.closed.Load() {
		return ErrConflict
	}
	if !restore.busy.CompareAndSwap(false, true) {
		return ErrLegacyNormalizationBusy
	}
	if err := restore.current(ctx); err != nil {
		restore.leave()
		return err
	}
	return nil
}

func (restore *LegacyNormalizationRestore) release() {
	if restore.stage != nil {
		restore.stage.close()
		restore.stage = nil
	}
	if restore.parser != nil {
		restore.parser.close()
		restore.parser = nil
	}
	if restore.work != nil {
		restore.work.Close()
	}
	restore.work, restore.source, restore.control, restore.hashState = nil, LegacyNormalizationCheckpointSource{}, LegacyNormalizationControl{}, nil
	restore.orderWindow = legacyNormalizationWindow{}
}

func (restore *LegacyNormalizationRestore) leave() {
	if restore.closed.Load() {
		restore.release()
	}
	restore.busy.Store(false)
	if restore.closed.Load() && restore.busy.CompareAndSwap(false, true) {
		restore.release()
		restore.busy.Store(false)
	}
}

func (restore *LegacyNormalizationRestore) hash(ctx context.Context, file *legacyNormalizationFile, offset, length int) error {
	if err := restore.current(ctx); err != nil {
		return err
	}
	if offset < 0 || length < 0 || offset > file.size || length > file.size-offset || len(file.buffer) > file.size {
		return ErrPreparedDocRangeObject
	}
	length = min(length, LegacyCanonicalEncodingBytes)
	digest := sha256.New()
	if len(restore.hashState) != 0 {
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(restore.hashState); err != nil {
			return ErrIntegrity
		}
	}
	physicalBytes := file.size - len(file.buffer)
	storedBytes := min(length, max(0, physicalBytes-offset))
	if storedBytes > 0 {
		body, err := file.storage.Read(ctx, file.slot, offset, storedBytes)
		if current := restore.current(ctx); current != nil {
			return current
		}
		if err != nil || len(body) != storedBytes {
			return errors.Join(ErrPreparedDocRangeObject, err)
		}
		digest.Write(body)
	}
	if storedBytes < length {
		bufferOffset := max(0, offset-physicalBytes)
		digest.Write(file.buffer[bufferOffset : bufferOffset+length-storedBytes])
	}
	state, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
	if current := restore.current(ctx); current != nil {
		return current
	}
	if err == nil {
		restore.hashState = state
	}
	return err
}

func (restore *LegacyNormalizationRestore) file(ctx context.Context) error {
	file := &restore.work.files[restore.slot]
	want := restore.control.Files[restore.slot]
	if len(file.parts) < want.Parts {
		count := min(32, want.Parts-len(file.parts))
		if restore.parserBudget > 0 {
			var planned docRangeHeaderHeapEstimate
			if len(file.parts)+count > cap(file.parts) {
				planned.allocation(int64(2*(len(file.parts)+count)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
			}
			for range count {
				planned.allocation(int64(len(contentHashPrefix)+64), 1)
			}
			if err := restore.parserAdmission(planned); err != nil {
				return err
			}
		}
		parts, err := restore.source.ReadParts(ctx, restore.slot, len(file.parts), count)
		if current := restore.current(ctx); current != nil {
			return current
		}
		if err != nil || len(parts) != count {
			return errors.Join(ErrPreparedDocRangeObject, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, part := range parts {
			if ValidateContentHash(part.Hash) != nil || part.Bytes <= 0 || part.Bytes > ConversationManifestChunkBytes ||
				len(file.parts)+1 < want.Parts && part.Bytes != ConversationManifestChunkBytes || part.Bytes > int64(want.PartsBytes-restore.partsBytes) {
				return ErrPreparedDocRangeObject
			}
			part.Hash = ContentHash(strings.Clone(string(part.Hash)))
			file.parts = append(file.parts, part)
			file.partHeap.allocation(int64(len(part.Hash)), 1)
			restore.partsBytes += int(part.Bytes)
		}
		return file.partHeap.err
	}
	if restore.partsBytes != want.PartsBytes {
		return ErrPreparedDocRangeObject
	}
	if want.BufferBytes > 0 && file.buffer == nil {
		var planned docRangeHeaderHeapEstimate
		planned.allocation(int64(want.BufferBytes), 1)
		if err := restore.parserAdmission(planned); err != nil {
			return err
		}
		body, err := restore.source.ReadBuffer(ctx, restore.slot)
		if current := restore.current(ctx); current != nil {
			return current
		}
		if err != nil || len(body) != want.BufferBytes {
			return errors.Join(ErrPreparedDocRangeObject, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		file.buffer = bytes.Clone(body)
		return nil
	}
	if restore.pageChecked < want.PageBytes {
		length := min(LegacyCanonicalEncodingBytes, want.PageBytes-restore.pageChecked)
		if err := restore.hash(ctx, file, want.PartsBytes+restore.pageChecked, length); err != nil {
			return err
		}
		restore.pageChecked += length
		return nil
	}
	if !bytes.Equal(restore.hashState, want.HashState) {
		return ErrPreparedDocRangeObject
	}
	if err := legacySourceEOF(ctx, func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := restore.current(ctx); err != nil {
			return nil, err
		}
		body, err := file.storage.Read(ctx, file.slot, offset, length)
		if current := restore.current(ctx); current != nil {
			return nil, current
		}
		return body, err
	}, want.Size-want.BufferBytes); err != nil {
		return err
	}
	restore.slot++
	restore.partsBytes, restore.pageChecked, restore.hashState = 0, 0, nil
	return nil
}

func (restore *LegacyNormalizationRestore) Step(ctx context.Context) (complete bool, failure error) {
	if err := restore.enter(ctx); err != nil {
		if restore != nil && restore.parserBudget > 0 && ctx.Err() != nil {
			restore.Close()
		}
		return false, err
	}
	defer restore.leave()
	if restore.terminal != nil {
		return false, restore.terminal
	}
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		} else if restore.closed.Load() {
			failure = ErrConflict
		}
		if failure == nil && restore.parserBudget > 0 {
			failure = restore.parserAdmission(docRangeHeaderHeapEstimate{})
		}
		if failure != nil {
			complete, restore.terminal = false, failure
			if restore.stage != nil {
				restore.stage.close()
				restore.stage = nil
			}
			if restore.parser != nil {
				restore.parser.close()
				restore.parser = nil
			}
		}
	}()
	if restore.ready {
		return true, nil
	}
	if restore.slot < LegacyNormalizationSlots {
		return false, restore.file(ctx)
	}
	if restore.parser != nil {
		complete, err := restore.restoreParser(ctx)
		if err != nil || !complete {
			return false, err
		}
	}
	if restore.control.Staging() && restore.work.pending == nil {
		complete, err := restore.restoreStage(ctx)
		if err != nil || !complete {
			return false, err
		}
	}
	if restore.control.Phase >= legacyNormalizationCompose && restore.control.Phase < legacyNormalizationReady {
		if restore.composed < restore.control.Position {
			end := min(restore.control.Position, restore.composed+32)
			for restore.composed < end {
				if restore.composed%(ConversationManifestChunkBytes/legacyNormalizationRecordBytes) == 0 {
					restore.orderWindow = legacyNormalizationWindow{}
				}
				record, err := restore.work.record(ctx, restore.control.SourceSlot, restore.composed, &restore.orderWindow)
				if current := restore.current(ctx); current != nil {
					return false, current
				}
				if err != nil || record.length > uint64(MaxFinalizedDocBytes)-uint64(restore.composedBytes) {
					return false, errors.Join(ErrPreparedDocRangeObject, err)
				}
				restore.composedBytes += int64(record.length)
				restore.composed++
			}
			return false, nil
		}
		restore.orderWindow = legacyNormalizationWindow{}
		expected := int64(restore.control.PrefixBytes) + restore.composedBytes + int64(max(0, restore.control.Position-1)) + int64(restore.control.Written)
		if restore.control.Written > 0 && restore.control.Position > 0 {
			expected++
		}
		if restore.control.Phase >= legacyNormalizationVerify {
			expected += int64(len(canonicalDocSuffix))
		}
		if expected != int64(restore.control.Files[3].Size) {
			return false, ErrPreparedDocRangeObject
		}
	}
	if restore.control.Written > 0 {
		if !restore.recordChecked && !restore.control.Staging() {
			var window legacyNormalizationWindow
			record, err := restore.work.record(ctx, restore.control.SourceSlot, restore.control.Position, &window)
			if current := restore.current(ctx); current != nil {
				return false, current
			}
			if err != nil || record != restore.work.active {
				return false, errors.Join(ErrPreparedDocRangeObject, err)
			}
			restore.recordChecked = true
			return false, nil
		}
		if restore.eventChecked < restore.control.Written {
			length := min(LegacyCanonicalEncodingBytes, restore.control.Written-restore.eventChecked)
			if err := restore.hash(ctx, &restore.work.files[0], int(restore.control.Active.Offset)+restore.eventChecked, length); err != nil {
				return false, err
			}
			restore.eventChecked += length
			return false, nil
		}
		if !bytes.Equal(restore.hashState, restore.control.HashState) {
			return false, ErrPreparedDocRangeObject
		}
		restore.hashState = nil
	}
	if restore.verifiedPart < restore.control.Verified {
		part := restore.work.files[3].parts[restore.verifiedPart]
		if restore.verifiedBytes < int(part.Bytes) {
			length := min(LegacyCanonicalEncodingBytes, int(part.Bytes)-restore.verifiedBytes)
			if err := restore.hash(ctx, &restore.work.files[3], restore.verifiedPart*ConversationManifestChunkBytes+restore.verifiedBytes, length); err != nil {
				return false, err
			}
			restore.verifiedBytes += length
			return false, nil
		}
		digest := sha256.New()
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(restore.hashState); err != nil {
			return false, ErrIntegrity
		}
		if ContentHash("sha256:"+hex.EncodeToString(digest.Sum(nil))) != part.Hash {
			return false, ErrPreparedDocRangeObject
		}
		restore.verifiedPart++
		restore.verifiedBytes, restore.hashState = 0, nil
		return false, nil
	}
	restore.ready = true
	return true, nil
}

func (restore *LegacyNormalizationRestore) Result() (*LegacyNormalizationPreparation, error) {
	if err := restore.enter(context.Background()); err != nil {
		return nil, err
	}
	defer restore.leave()
	if restore.terminal != nil {
		return nil, restore.terminal
	}
	if !restore.ready {
		return nil, ErrDocRangeIndexPending
	}
	if !restore.closed.CompareAndSwap(false, true) {
		return nil, ErrConflict
	}
	work := restore.work
	restore.work = nil
	restore.source, restore.control, restore.hashState = LegacyNormalizationCheckpointSource{}, LegacyNormalizationControl{}, nil
	return work, nil
}

func (restore *LegacyNormalizationRestore) RetainedBytes() (int64, error) {
	if err := restore.enter(context.Background()); err != nil {
		return 0, err
	}
	defer restore.leave()
	return restore.retainedBytes()
}

func (restore *LegacyNormalizationRestore) retainedBytes() (int64, error) {
	retained, err := restore.work.RetainedBytes()
	if err != nil {
		return 0, err
	}
	estimate := docRangeHeaderHeapEstimate{bytes: retained}
	estimate.allocation(1, int64(unsafe.Sizeof(*restore)))
	estimate.allocation(int64(cap(restore.hashState)+cap(restore.control.HashState)), 1)
	for _, file := range restore.control.Files {
		estimate.allocation(int64(cap(file.HashState)), 1)
	}
	for _, page := range restore.orderWindow.pages {
		estimate.allocation(int64(cap(page)), 1)
	}
	if restore.parser != nil {
		retained, err := restore.parser.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if restore.stage != nil {
		retained, err := restore.stage.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if err := restore.current(context.Background()); err != nil {
		return 0, err
	}
	return estimate.bytes, estimate.err
}

func (restore *LegacyNormalizationRestore) Close() {
	if restore != nil {
		restore.closed.Store(true)
		if restore.busy.CompareAndSwap(false, true) {
			restore.release()
			restore.busy.Store(false)
		}
	}
}
