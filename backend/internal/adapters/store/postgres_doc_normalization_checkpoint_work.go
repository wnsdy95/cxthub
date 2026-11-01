//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyNormalizationBufferRef struct {
	Hash  domain.ContentHash `json:"h"`
	Bytes int                `json:"n"`
}

type legacyNormalizationCheckpointFile struct {
	Data   legacyNormalizationPrefix    `json:"d"`
	Parts  legacyNormalizationPrefix    `json:"p"`
	Buffer legacyNormalizationBufferRef `json:"b"`
}

type legacyNormalizationCheckpointRoot struct {
	Version uint8                                                              `json:"v"`
	State   domain.LegacyNormalizationControl                                  `json:"s"`
	Header  legacyNormalizationPrefix                                          `json:"h"`
	Files   [domain.LegacyNormalizationSlots]legacyNormalizationCheckpointFile `json:"f"`
	Parser  *legacyNormalizationParserCheckpoint                               `json:"parser,omitempty"`
	Encoder *legacyNormalizationBufferRef                                      `json:"encoder,omitempty"`
}

func (root legacyNormalizationCheckpointRoot) valid(state legacyByteCheckpoint) bool {
	if root.Version != 1 || root.State.Validate() != nil {
		return false
	}
	if root.State.InputPending() != (root.Parser != nil) || root.Parser != nil && !root.Parser.valid(state) {
		return false
	}
	if root.State.Staging() != (root.Encoder != nil) || root.Encoder != nil &&
		(domain.ValidateContentHash(root.Encoder.Hash) != nil || root.Encoder.Bytes < 1 || root.Encoder.Bytes > domain.LegacyCanonicalEncodingBytes) {
		return false
	}
	valid := func(prefix legacyNormalizationPrefix, generation uint64, chunkBytes, size int) bool {
		return prefix.valid() && prefix.Stage == state.stage.incarnation && prefix.Generation == generation &&
			prefix.ChunkBytes == chunkBytes && prefix.Tree.DataBytes == size
	}
	if !valid(root.Header, 1, domain.LegacyCanonicalEncodingBytes, root.State.HeaderBytes) {
		return false
	}
	for slot, file := range root.Files {
		control := root.State.Files[slot]
		if !valid(file.Data, control.Generation, domain.LegacyCanonicalEncodingBytes, control.Size-control.BufferBytes) ||
			!valid(file.Parts, control.Generation, legacyNormalizationReceiptChunkBytes, control.Parts*legacyNormalizationReceiptBytes) ||
			file.Buffer.Bytes != control.BufferBytes || control.BufferBytes == 0 && file.Buffer.Hash != "" ||
			control.BufferBytes > 0 && domain.ValidateContentHash(file.Buffer.Hash) != nil {
			return false
		}
	}
	return true
}

type legacyNormalizationCheckpointWork struct {
	store              *PostgresStore
	job                domain.DocRangeIndexJob
	state              legacyByteCheckpoint
	root               legacyNormalizationCheckpointRoot
	next               legacyNormalizationCheckpointRoot
	capture            *domain.LegacyNormalizationCheckpointCapture
	position           int
	pending            legacyByteCheckpointObject
	uncertain          bool
	restoring          bool
	recovered          bool
	restoreSlot        int
	restore            *domain.LegacyNormalizationRestore
	restoreFailure     error
	auditSlot          int
	auditOffset        int
	reclaim            bool
	reclaimRoot        bool
	parserParts        []domain.ConversationManifestChunk
	parserPins         [8][legacyNormalizationPrefixFanout]legacyNormalizationBufferRef
	parserPinCounts    [8]uint8
	parserPosition     int
	nextParserPosition int
}

func (work *legacyNormalizationCheckpointWork) busy() bool {
	return work != nil && (work.capture != nil || work.restoring || work.restoreFailure != nil)
}

func (work *legacyNormalizationCheckpointWork) begin(ctx context.Context, preparation *domain.LegacyNormalizationPreparation) error {
	if work.busy() {
		return domain.ErrLegacyNormalizationBusy
	}
	capture, err := preparation.BeginCheckpoint(ctx)
	if err != nil {
		return err
	}
	return work.beginCapture(ctx, capture)
}

func (work *legacyNormalizationCheckpointWork) beginCapture(ctx context.Context, capture *domain.LegacyNormalizationCheckpointCapture) error {
	if work.busy() {
		capture.Close()
		return domain.ErrLegacyNormalizationBusy
	}
	control, err := capture.Control(ctx)
	if err != nil {
		capture.Close()
		return err
	}
	work.next = work.root
	work.next.Version, work.next.State = 1, control
	work.next.Parser, work.next.Encoder = nil, nil
	work.parserPins, work.parserPinCounts = [8][legacyNormalizationPrefixFanout]legacyNormalizationBufferRef{}, [8]uint8{}
	if control.InputPending() {
		work.next.Parser = &legacyNormalizationParserCheckpoint{}
		if work.root.Parser != nil {
			work.next.Parser.Input = work.root.Parser.Input
		}
		work.nextParserPosition, err = capture.ParserInputPosition(ctx)
		if err != nil {
			capture.Close()
			return err
		}
	}
	work.capture, work.position = capture, 0
	return nil
}

func (work *legacyNormalizationCheckpointWork) pressure(ctx context.Context) error {
	transaction, _, err := work.store.openLegacyByteCheckpoint(ctx, work.job, work.state.stage, &work.state, false)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	var objects, size int64
	if err := transaction.QueryRow(ctx, `SELECT checkpoint_objects,checkpoint_bytes FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, work.job.DocHash).
		Scan(&objects, &size); err != nil {
		return err
	}
	if err := liveLegacyByteJob(ctx, transaction, work.job); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	work.reclaim = work.state.hash != "" && (objects >= 60000 || size >= 2<<30)
	work.reclaimRoot = work.reclaim
	return nil
}

func (work *legacyNormalizationCheckpointWork) commit(ctx context.Context, object legacyByteCheckpointObject) (legacyByteCheckpoint, error) {
	if work.uncertain {
		current, found, err := work.store.loadLegacyNormalizationCheckpoint(ctx, work.job)
		if err != nil {
			return legacyByteCheckpoint{}, err
		}
		if found && current.stage.incarnation == work.state.stage.incarnation && current.sequence == work.state.sequence+1 &&
			current.hash == object.hash && current.bytes == len(object.data) {
			work.uncertain = false
			return current, nil
		}
		if current != work.state {
			return legacyByteCheckpoint{}, domain.ErrConflict
		}
	}
	current, err := work.store.saveLegacyNormalizationCheckpoint(ctx, work.job, work.state, object.hash, len(object.data), []legacyByteCheckpointObject{object})
	work.uncertain = err != nil
	return current, err
}

func (work *legacyNormalizationCheckpointWork) parts(ctx context.Context, slot, offset, length int) ([]byte, error) {
	if offset%legacyNormalizationReceiptBytes != 0 || length%legacyNormalizationReceiptBytes != 0 {
		return nil, domain.ErrIntegrity
	}
	parts, err := work.capture.ReadParts(ctx, slot, offset/legacyNormalizationReceiptBytes, length/legacyNormalizationReceiptBytes)
	if err != nil {
		return nil, err
	}
	body := make([]byte, length)
	for ordinal, part := range parts {
		if domain.ValidateContentHash(part.Hash) != nil || part.Bytes < 1 || part.Bytes > domain.ConversationManifestChunkBytes {
			return nil, domain.ErrIntegrity
		}
		position := ordinal * legacyNormalizationReceiptBytes
		binary.BigEndian.PutUint32(body[position:], uint32(part.Bytes))
		if _, err := hex.Decode(body[position+4:position+legacyNormalizationReceiptBytes], []byte(part.Hash[7:])); err != nil {
			return nil, domain.ErrIntegrity
		}
	}
	return body, ctx.Err()
}

func (work *legacyNormalizationCheckpointWork) write(ctx context.Context, storage *legacyNormalizationSpool) (bool, error) {
	if work.capture == nil {
		return false, domain.ErrIntegrity
	}
	if work.position == 0 {
		if err := work.pressure(ctx); err != nil {
			return false, err
		}
		work.position = 1
	}
	if work.reclaim {
		if work.reclaimRoot {
			body, err := json.Marshal(work.root)
			if err != nil {
				return false, err
			}
			state, err := work.commit(ctx, legacyByteCheckpointObject{hash: work.state.hash, data: body, structure: 2})
			if err != nil {
				return false, err
			}
			work.state, work.reclaimRoot = state, false
			return false, nil
		}
		complete, _, err := work.store.reclaimLegacyNormalizationCheckpoint(ctx, work.job, work.state)
		if err != nil {
			return false, err
		}
		work.reclaim = !complete
		return false, nil
	}
	for work.position < 14 {
		if work.position == 1 && work.next.Header.Stage == work.state.stage.incarnation &&
			work.next.Header.Tree.DataBytes == work.next.State.HeaderBytes {
			work.position++
			continue
		}
		if work.position == 1 {
			prefix, err := work.store.stageLegacyNormalizationPrefix(ctx, work.job, work.state, work.next.Header, 1,
				domain.LegacyCanonicalEncodingBytes, work.next.State.HeaderBytes, work.capture.ReadHeader)
			if err != nil {
				return false, err
			}
			work.next.Header = prefix
			if prefix.Tree.DataBytes == work.next.State.HeaderBytes {
				work.position++
			}
			return false, nil
		}
		if work.position < 10 {
			slot := (work.position - 2) / 2
			control := work.next.State.Files[slot]
			if work.position%2 == 0 {
				previous := work.next.Files[slot].Data
				if previous.Stage == work.state.stage.incarnation && previous.Generation == control.Generation && previous.Tree.DataBytes == control.Size-control.BufferBytes {
					work.position++
					continue
				}
				prefix, err := work.store.stageLegacyNormalizationPrefix(ctx, work.job, work.state, work.next.Files[slot].Data, control.Generation,
					domain.LegacyCanonicalEncodingBytes, control.Size-control.BufferBytes,
					func(ctx context.Context, offset, length int) ([]byte, error) {
						return storage.Read(ctx, slot, offset, length)
					})
				if err != nil {
					return false, err
				}
				work.next.Files[slot].Data = prefix
				if prefix.Tree.DataBytes == control.Size-control.BufferBytes {
					work.position++
				}
			} else {
				previous := work.next.Files[slot].Parts
				if previous.Stage == work.state.stage.incarnation && previous.Generation == control.Generation && previous.Tree.DataBytes == control.Parts*legacyNormalizationReceiptBytes {
					work.position++
					continue
				}
				prefix, err := work.store.stageLegacyNormalizationPrefix(ctx, work.job, work.state, work.next.Files[slot].Parts, control.Generation,
					legacyNormalizationReceiptChunkBytes, control.Parts*legacyNormalizationReceiptBytes,
					func(ctx context.Context, offset, length int) ([]byte, error) {
						return work.parts(ctx, slot, offset, length)
					})
				if err != nil {
					return false, err
				}
				work.next.Files[slot].Parts = prefix
				if prefix.Tree.DataBytes == control.Parts*legacyNormalizationReceiptBytes {
					work.position++
				}
			}
			return false, nil
		}
		if work.position < 14 {
			slot := work.position - 10
			body, err := work.capture.ReadBuffer(ctx, slot)
			if err != nil {
				return false, err
			}
			reference := legacyNormalizationBufferRef{}
			if len(body) > 0 {
				object := legacyByteCheckpointObject{hash: domain.HashContent(body), data: body}
				if work.next.Files[slot].Buffer == (legacyNormalizationBufferRef{Hash: object.hash, Bytes: len(body)}) {
					work.position++
					continue
				}
				if err := work.store.stageLegacyNormalizationCheckpointObjects(ctx, work.job, work.state, []legacyByteCheckpointObject{object}); err != nil {
					return false, err
				}
				reference = legacyNormalizationBufferRef{Hash: object.hash, Bytes: len(body)}
			}
			work.next.Files[slot].Buffer = reference
			work.position++
			if len(body) > 0 {
				return false, nil
			}
		}
	}
	if work.pending.hash == "" {
		if work.next.Parser != nil {
			ready, err := work.writeParser(ctx)
			if err != nil || !ready {
				return false, err
			}
		}
		if !work.next.valid(work.state) {
			return false, domain.ErrIntegrity
		}
		body, err := json.Marshal(work.next)
		if err != nil || len(body) > domain.LegacyCanonicalEncodingBytes {
			return false, errors.Join(domain.ErrIntegrity, err)
		}
		work.pending = legacyByteCheckpointObject{hash: domain.HashContent(body), data: body, structure: 2}
		return false, nil
	}
	state, err := work.commit(ctx, work.pending)
	if err != nil {
		return false, err
	}
	work.state, work.root = state, work.next
	work.parserPosition = work.nextParserPosition
	work.nextParserPosition = 0
	work.capture.Close()
	work.capture, work.next, work.pending = nil, legacyNormalizationCheckpointRoot{}, legacyByteCheckpointObject{}
	work.parserPins, work.parserPinCounts = [8][legacyNormalizationPrefixFanout]legacyNormalizationBufferRef{}, [8]uint8{}
	return true, nil
}

func decodeLegacyNormalizationCheckpoint(body []byte, state legacyByteCheckpoint) (legacyNormalizationCheckpointRoot, error) {
	var root legacyNormalizationCheckpointRoot
	if len(body) != state.bytes || len(body) > domain.LegacyCanonicalEncodingBytes {
		return root, domain.ErrPreparedDocRangeObject
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&root); err != nil || !root.valid(state) {
		return legacyNormalizationCheckpointRoot{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return legacyNormalizationCheckpointRoot{}, domain.ErrPreparedDocRangeObject
	}
	canonical, err := json.Marshal(root)
	if err != nil || !bytes.Equal(body, canonical) {
		return legacyNormalizationCheckpointRoot{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return root, nil
}

func (work *legacyNormalizationCheckpointWork) retainedBytes() (int64, error) {
	estimate := rootPublicationHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(*work)))
	estimate.allocation(int64(cap(work.pending.data)), 1)
	for _, root := range []*legacyNormalizationCheckpointRoot{&work.root, &work.next} {
		if root.Parser != nil {
			estimate.allocation(1, int64(unsafe.Sizeof(*root.Parser)))
			estimate.allocation(int64(len(root.Parser.Ref.Hash)+len(root.Parser.Pins.Hash)+len(root.Parser.Input.Stage)+len(root.Parser.Input.Tree.Hash)), 1)
		}
		if root.Encoder != nil {
			estimate.allocation(1, int64(unsafe.Sizeof(*root.Encoder)))
			estimate.allocation(int64(len(root.Encoder.Hash)), 1)
		}
		estimate.allocation(int64(cap(root.State.HashState)), 1)
		for _, file := range root.State.Files {
			estimate.allocation(int64(cap(file.HashState)), 1)
		}
		prefixes := []legacyNormalizationPrefix{root.Header}
		for _, file := range root.Files {
			prefixes = append(prefixes, file.Data, file.Parts)
			estimate.allocation(int64(len(file.Buffer.Hash)), 1)
		}
		for _, prefix := range prefixes {
			estimate.allocation(int64(len(prefix.Stage)+len(prefix.Tree.Hash)), 1)
		}
	}
	estimate.allocation(int64(cap(work.parserParts)), int64(unsafe.Sizeof(domain.ConversationManifestChunk{})))
	for level, refs := range work.parserPins {
		for _, ref := range refs[:work.parserPinCounts[level]] {
			estimate.allocation(int64(len(ref.Hash)), 1)
		}
	}
	for _, part := range work.parserParts {
		estimate.allocation(int64(len(part.Hash)), 1)
	}
	for _, value := range []string{string(work.job.RepoID), string(work.job.DocHash), string(work.job.BaseHash), work.job.State, work.job.Reason,
		work.job.RepositoryEpoch, work.state.stage.sourceID, work.state.stage.incarnation, string(work.state.stage.plan), string(work.state.hash), string(work.pending.hash)} {
		estimate.allocation(int64(len(value)), 1)
	}
	if work.capture != nil {
		retained, err := work.capture.RetainedBytes()
		if err != nil {
			return 0, err
		}
		estimate.include(rootPublicationHeapEstimate{bytes: retained})
	}
	if work.restore != nil {
		retained, err := work.restore.RetainedBytes()
		if err != nil {
			return 0, err
		}
		estimate.include(rootPublicationHeapEstimate{bytes: retained})
	}
	if work.restoreFailure != nil {
		estimate.allocation(int64(len(work.restoreFailure.Error())), 1)
	}
	return estimate.bytes, estimate.err
}

func (work *legacyNormalizationCheckpointWork) Close() {
	if work != nil {
		work.capture.Close()
		work.restore.Close()
		work.capture, work.restore = nil, nil
		work.root, work.next = legacyNormalizationCheckpointRoot{}, legacyNormalizationCheckpointRoot{}
		work.pending, work.restoreFailure = legacyByteCheckpointObject{}, nil
		work.parserParts = nil
		work.parserPins, work.parserPinCounts = [8][legacyNormalizationPrefixFanout]legacyNormalizationBufferRef{}, [8]uint8{}
	}
}
