package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"strings"
	"sync/atomic"
	"unsafe"
)

type LegacyNormalizationFileControl struct {
	Generation  uint64
	Size        int
	BufferBytes int
	Parts       int
	PartsBytes  int
	PageBytes   int
	HashState   []byte
}

type LegacyNormalizationRecordControl struct {
	Sequence int64
	Ordinal  uint64
	Offset   uint64
	Length   uint64
	Hash     [sha256.Size]byte
}

type LegacyNormalizationControl struct {
	Version      uint8
	StagePending int `json:",omitempty"`
	Phase        uint8
	Count        int
	HeaderBytes  int
	PrefixBytes  int
	Written      int
	HashState    []byte
	Active       LegacyNormalizationRecordControl
	Position     int
	SourceSlot   int
	TargetSlot   int
	Width        int
	Base         int
	Left         int
	Right        int
	Segment      int
	SegmentPos   int
	Verified     int
	Files        [LegacyNormalizationSlots]LegacyNormalizationFileControl
}

func (control LegacyNormalizationControl) Ready() bool {
	return control.Phase == legacyNormalizationReady
}

func (control LegacyNormalizationControl) Parsing() bool {
	return control.Phase == legacyNormalizationDecode
}

func (control LegacyNormalizationControl) Staging() bool {
	return control.Version == 3 && control.Phase == legacyNormalizationStage
}

func (control LegacyNormalizationControl) InputPending() bool {
	return control.Parsing() || control.Staging()
}

func (control LegacyNormalizationControl) clone() LegacyNormalizationControl {
	control.HashState = bytes.Clone(control.HashState)
	for slot := range control.Files {
		control.Files[slot].HashState = bytes.Clone(control.Files[slot].HashState)
	}
	return control
}

func legacyNormalizationHashState(state []byte, length int) bool {
	if length == 0 {
		return len(state) == 0
	}
	if length < 0 || len(state) != 108 || binary.BigEndian.Uint64(state[len(state)-8:]) != uint64(length) {
		return false
	}
	digest := sha256.New()
	if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return false
	}
	canonical, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
	return err == nil && bytes.Equal(canonical, state)
}

func (control LegacyNormalizationControl) Validate() error {
	if control.Version != 1 && control.Version != 2 && control.Version != 3 ||
		control.Version != 3 && control.StagePending != 0 ||
		control.Version == 1 && (control.Phase < legacyNormalizationRuns || control.Phase > legacyNormalizationReady ||
			control.SourceSlot < 1 || control.SourceSlot > 2 || control.SourceSlot+control.TargetSlot != 3) ||
		control.Count < 0 || control.Count > MaxFinalizedDocBytes/legacyNormalizationRecordBytes || control.Position < 0 || control.Position > control.Count ||
		control.HeaderBytes < 0 || control.HeaderBytes > MaxFinalizedDocBytes || control.Written < 0 ||
		control.Segment < 0 || control.Segment > 3 || control.SegmentPos < 0 || control.Verified < 0 {
		return ErrIntegrity
	}
	for _, file := range control.Files {
		if file.Generation == 0 || file.Size < 0 || file.Size > MaxFinalizedDocBytes || file.BufferBytes < 0 || file.BufferBytes >= legacyNormalizationBufferBytes ||
			file.PageBytes < 0 || file.PageBytes >= ConversationManifestChunkBytes || file.Parts < 0 || file.Parts > (MaxFinalizedDocBytes+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes ||
			file.PartsBytes < 0 || file.PartsBytes > file.Size || file.Size-file.PartsBytes != file.PageBytes+file.BufferBytes ||
			file.Parts == 0 && file.PartsBytes != 0 || file.Parts > 0 && (file.PartsBytes <= (file.Parts-1)*ConversationManifestChunkBytes || file.PartsBytes > file.Parts*ConversationManifestChunkBytes) ||
			!legacyNormalizationHashState(file.HashState, file.PageBytes) {
			return ErrIntegrity
		}
	}
	if control.Version == 2 {
		return control.validateParser()
	}
	if control.Version == 3 {
		return control.validateStage()
	}
	closed := func(slot int) bool { return control.Files[slot].BufferBytes == 0 && control.Files[slot].PageBytes == 0 }
	if control.Phase != legacyNormalizationReady && (!closed(0) || !closed(control.SourceSlot) || control.Files[control.SourceSlot].Size != control.Count*legacyNormalizationRecordBytes) {
		return ErrIntegrity
	}
	if control.Phase <= legacyNormalizationPrefix {
		if control.HeaderBytes == 0 || control.PrefixBytes != 0 || control.Written != 0 || len(control.HashState) != 0 {
			return ErrIntegrity
		}
	} else if control.HeaderBytes != 0 || control.Segment != 3 || control.SegmentPos != 0 ||
		control.PrefixBytes < len(canonicalDocPrefix)+len(canonicalDocMiddle)+2 || control.PrefixBytes > control.Files[3].Size {
		return ErrIntegrity
	}
	if control.Phase != legacyNormalizationCompose && (control.Written != 0 || len(control.HashState) != 0) {
		return ErrIntegrity
	}
	switch control.Phase {
	case legacyNormalizationRuns:
		if control.SourceSlot != 1 || control.TargetSlot != 2 || control.Width != 0 || control.Position%legacyNormalizationRunRecords != 0 ||
			control.Files[2].Size != control.Position*legacyNormalizationRecordBytes || control.Files[3].Size != 0 || control.Segment != 0 || control.SegmentPos != 0 {
			return ErrIntegrity
		}
	case legacyNormalizationMerge:
		if control.Width < legacyNormalizationRunRecords || control.Width%legacyNormalizationRunRecords != 0 || control.Width&(control.Width-1) != 0 || control.Width >= control.Count ||
			control.Position >= control.Count || control.Base < 0 || control.Base > control.Position || control.Base%(2*control.Width) != 0 ||
			control.Files[control.TargetSlot].Size != control.Position*legacyNormalizationRecordBytes || control.Files[3].Size != 0 || control.Segment != 0 || control.SegmentPos != 0 {
			return ErrIntegrity
		}
		leftEnd, rightEnd := min(control.Base+control.Width, control.Count), min(control.Base+2*control.Width, control.Count)
		if control.Left == 0 && control.Right == 0 {
			if control.Position != control.Base {
				return ErrIntegrity
			}
		} else if control.Left < control.Base || control.Left > leftEnd || control.Right < leftEnd || control.Right > rightEnd ||
			control.Left == leftEnd && control.Right == rightEnd || control.Position != control.Left+control.Right-leftEnd {
			return ErrIntegrity
		}
	case legacyNormalizationPrefix:
		lengths := [3]int{len(canonicalDocPrefix), control.HeaderBytes, len(canonicalDocMiddle)}
		if control.Position != 0 || control.Base != 0 || control.Left != 0 || control.Right != 0 || control.Segment >= len(lengths) ||
			control.SegmentPos >= lengths[control.Segment] || control.Files[control.TargetSlot].Size != 0 {
			return ErrIntegrity
		}
		written := control.SegmentPos
		for segment := 0; segment < control.Segment; segment++ {
			written += lengths[segment]
		}
		if control.Files[3].Size != written {
			return ErrIntegrity
		}
	case legacyNormalizationCompose:
		if !legacyNormalizationHashState(control.HashState, control.Written) {
			return ErrIntegrity
		}
		minimumBytes := int64(control.PrefixBytes) + int64(control.Written) + int64(control.Position) + int64(max(0, control.Position-1))
		if control.Written > 0 && control.Position > 0 {
			minimumBytes++
		}
		if int64(control.Files[3].Size) < minimumBytes {
			return ErrIntegrity
		}
		if control.Written > 0 && (control.Position >= control.Count || control.Active.Ordinal >= uint64(control.Count) ||
			int64(int(control.Active.Sequence)) != control.Active.Sequence || control.Active.Offset > uint64(control.Files[0].Size) ||
			control.Active.Length == 0 || control.Active.Length > uint64(control.Files[0].Size)-control.Active.Offset || uint64(control.Written) >= control.Active.Length) {
			return ErrIntegrity
		}
	case legacyNormalizationSuffix, legacyNormalizationVerify, legacyNormalizationReady:
		if control.Position != control.Count {
			return ErrIntegrity
		}
	}
	if control.Phase < legacyNormalizationVerify && control.Verified != 0 || control.Verified > control.Files[3].Parts ||
		control.Phase >= legacyNormalizationVerify && !closed(3) || control.Phase == legacyNormalizationReady && control.Verified != control.Files[3].Parts {
		return ErrIntegrity
	}
	return nil
}

type LegacyNormalizationCheckpointCapture struct {
	work          *LegacyNormalizationPreparation
	capture       *legacyNormalizationCapture
	control       LegacyNormalizationControl
	parser        *legacyNormalizationDecoderCheckpoint
	parserRef     LegacyNormalizationParserCheckpointRef
	parserBinding ContentHash
	parserBudget  int64
	parserBusy    atomic.Bool
	stage         *legacyNormalizationStageCheckpoint
	stageRef      LegacyNormalizationParserCheckpointRef
	closed        atomic.Bool
}

func (work *LegacyNormalizationPreparation) BeginCheckpoint(ctx context.Context) (_ *LegacyNormalizationCheckpointCapture, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work == nil {
		return nil, ErrIntegrity
	}
	capture, err := work.mutation.beginCapture()
	if err != nil {
		return nil, err
	}
	defer func() {
		if failure != nil {
			capture.release()
		}
	}()
	if work.terminal != nil {
		return nil, work.terminal
	}
	parsing := work.phase == legacyNormalizationDecode
	staging := work.phase == legacyNormalizationStage
	pending := 0
	if staging {
		if work.next < 0 || work.next >= len(work.pending) {
			return nil, ErrIntegrity
		}
		pending = len(work.pending) - work.next
		for index, event := range work.pending[work.next:] {
			if event.Encoding == nil || !event.Encoding.initialized || event.Encoding.complete || event.Encoding.err != nil {
				return nil, ErrIntegrity
			}
			if event.Encoding.preparation != nil {
				if index == 0 && work.written != 0 {
					return nil, ErrIntegrity
				}
				if !legacyCanonicalInitialState(event.Encoding) && (index != 0 || !legacyCanonicalSourceCheckpointState(event.Encoding)) {
					return nil, ErrDocRangeIndexPending
				}
			}
		}
	}
	if !staging && work.pending != nil || (parsing || staging) && (work.decoder == nil || work.decoder.mutation != work.mutation ||
		!legacyNormalizationParserCount(work.decoder, work.count+pending)) || !parsing && !staging && work.decoder != nil {
		return nil, ErrIntegrity
	}
	control := LegacyNormalizationControl{Version: 1, Phase: work.phase, Count: work.count, HeaderBytes: len(work.header), PrefixBytes: work.prefixBytes, Written: work.written,
		HashState: bytes.Clone(work.hashState), Active: LegacyNormalizationRecordControl{Sequence: work.active.sequence, Ordinal: work.active.ordinal,
			Offset: work.active.offset, Length: work.active.length, Hash: work.active.hash}, Position: work.position,
		SourceSlot: work.sourceSlot, TargetSlot: work.targetSlot, Width: work.width, Base: work.base, Left: work.left, Right: work.right,
		Segment: work.segment, SegmentPos: work.segmentPos, Verified: work.verified}
	if parsing {
		control.Version = 2
		control.Active = LegacyNormalizationRecordControl{}
	}
	if staging {
		control.Version, control.StagePending = 3, pending
		if control.Written == 0 {
			control.HashState = nil
			control.Active = LegacyNormalizationRecordControl{Sequence: int64(work.pending[work.next].Sequence),
				Ordinal: uint64(work.count), Offset: uint64(work.files[0].size)}
		}
	}
	if work.headerSource != nil {
		control.HeaderBytes = work.headerSource.Bytes
	}
	for slot, file := range work.files {
		control.Files[slot] = LegacyNormalizationFileControl{Generation: file.generation, Size: file.size, BufferBytes: len(file.buffer),
			Parts: len(file.parts), PartsBytes: file.size - len(file.buffer) - file.pageBytes, PageBytes: file.pageBytes, HashState: bytes.Clone(file.hashState)}
		if work.phase == legacyNormalizationReady && slot < 3 {
			control.Files[slot] = LegacyNormalizationFileControl{Generation: file.generation}
		}
	}
	if err := control.Validate(); err != nil {
		return nil, err
	}
	return &LegacyNormalizationCheckpointCapture{work: work, capture: capture, control: control}, nil
}

func (capture *LegacyNormalizationCheckpointCapture) valid(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if capture == nil || capture.closed.Load() || capture.work == nil || !capture.capture.valid(capture.work.mutation) {
		return ErrConflict
	}
	return nil
}

func (capture *LegacyNormalizationCheckpointCapture) Control(ctx context.Context) (LegacyNormalizationControl, error) {
	if err := capture.valid(ctx); err != nil {
		return LegacyNormalizationControl{}, err
	}
	return capture.control.clone(), nil
}

func (capture *LegacyNormalizationCheckpointCapture) ReadHeader(ctx context.Context, offset, length int) ([]byte, error) {
	if err := capture.valid(ctx); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > LegacyCanonicalEncodingBytes || offset > capture.control.HeaderBytes || length > capture.control.HeaderBytes-offset {
		return nil, ErrIntegrity
	}
	if capture.work.headerSource != nil {
		body, err := capture.work.headerSource.Read(ctx, offset, length)
		if err != nil || len(body) != length {
			return nil, errors.Join(ErrPreparedDocRangeObject, err)
		}
		if err := capture.valid(ctx); err != nil {
			return nil, err
		}
		return bytes.Clone(body), nil
	}
	return bytes.Clone(capture.work.header[offset : offset+length]), ctx.Err()
}

func (capture *LegacyNormalizationCheckpointCapture) ReadBuffer(ctx context.Context, slot int) ([]byte, error) {
	if err := capture.valid(ctx); err != nil {
		return nil, err
	}
	if slot < 0 || slot >= LegacyNormalizationSlots {
		return nil, ErrIntegrity
	}
	return bytes.Clone(capture.work.files[slot].buffer[:capture.control.Files[slot].BufferBytes]), ctx.Err()
}

func (capture *LegacyNormalizationCheckpointCapture) ReadParts(ctx context.Context, slot, first, count int) ([]ConversationManifestChunk, error) {
	if err := capture.valid(ctx); err != nil {
		return nil, err
	}
	if slot < 0 || slot >= LegacyNormalizationSlots || first < 0 || count < 0 || count > 32 || first > capture.control.Files[slot].Parts || count > capture.control.Files[slot].Parts-first {
		return nil, ErrIntegrity
	}
	parts := append([]ConversationManifestChunk(nil), capture.work.files[slot].parts[first:first+count]...)
	for ordinal := range parts {
		parts[ordinal].Hash = ContentHash(strings.Clone(string(parts[ordinal].Hash)))
	}
	return parts, ctx.Err()
}

func (capture *LegacyNormalizationCheckpointCapture) RetainedBytes() (int64, error) {
	if err := capture.valid(context.Background()); err != nil {
		return 0, err
	}
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(*capture)))
	estimate.allocation(1, int64(unsafe.Sizeof(*capture.capture)))
	estimate.allocation(int64(cap(capture.control.HashState)), 1)
	for _, file := range capture.control.Files {
		estimate.allocation(int64(cap(file.HashState)), 1)
	}
	estimate.allocation(int64(len(capture.parserBinding)+len(capture.parserRef.Hash)), 1)
	estimate.allocation(int64(len(capture.stageRef.Hash)), 1)
	if capture.stage != nil {
		retained, err := capture.stage.retainedBytes()
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if capture.parser != nil {
		retained, err := capture.parser.retainedBytes()
		if err != nil {
			return 0, err
		}
		shared, err := capture.work.decoder.RetainedBytes()
		var grant docRangeHeaderHeapEstimate
		grant.allocation(1, int64(unsafe.Sizeof(*capture.capture)))
		if err != nil || grant.err != nil || retained < shared+grant.bytes {
			return 0, ErrIntegrity
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained - shared - grant.bytes})
	}
	return estimate.bytes, estimate.err
}

func (capture *LegacyNormalizationCheckpointCapture) Close() {
	if capture != nil {
		capture.closed.Store(true)
		if capture.parserBusy.CompareAndSwap(false, true) {
			capture.release()
			capture.parserBusy.Store(false)
		}
	}
}

func (capture *LegacyNormalizationCheckpointCapture) release() {
	if capture.stage != nil {
		capture.stage.close()
		capture.stage = nil
	}
	if capture.parser != nil {
		capture.parser.close()
		capture.parser = nil
	}
	if capture.capture != nil {
		capture.capture.release()
		capture.capture, capture.work = nil, nil
	}
}
