//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"strconv"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const legacyNormalizationParserHeapBudget int64 = 64 << 20
const legacyNormalizationParserCheckpointQuanta = 64
const legacyNormalizationParserBatchHeap = 2 * domain.LegacyCanonicalEncodingBytes

type legacyNormalizationParserCheckpoint struct {
	Ref   legacyNormalizationBufferRef `json:"ref"`
	Pins  legacyNormalizationBufferRef `json:"pins"`
	Input legacyNormalizationPrefix    `json:"input"`
}

func (parser legacyNormalizationParserCheckpoint) valid(state legacyByteCheckpoint) bool {
	validRef := func(ref legacyNormalizationBufferRef) bool {
		return domain.ValidateContentHash(ref.Hash) == nil && ref.Bytes > 0 && ref.Bytes <= domain.LegacyCanonicalEncodingBytes
	}
	return validRef(parser.Ref) && validRef(parser.Pins) && parser.Input.valid() && parser.Input.Stage == state.stage.incarnation &&
		parser.Input.Generation == 1 && parser.Input.ChunkBytes == domain.LegacyNormalizationPartBytes && parser.Input.Tree.DataBytes > 0
}

func (work *legacyNormalizationCheckpointWork) parserBinding() domain.ContentHash {
	return domain.HashContent([]byte("legacy-normalization-parser-v1\x00" + string(work.job.DocHash) + "\x00" +
		work.state.stage.sourceID + "\x00" + strconv.FormatInt(work.state.stage.sourceVersion, 10) + "\x00" + work.state.stage.incarnation))
}

func (work *legacyNormalizationCheckpointWork) putParserObject(ctx context.Context, hash domain.ContentHash, body []byte) error {
	if work.next.Parser == nil {
		return domain.ErrIntegrity
	}
	if err := work.stageParserObjects(ctx, []legacyByteCheckpointObject{{hash: hash, data: body}}); err != nil {
		return err
	}
	ref := legacyNormalizationBufferRef{Hash: hash, Bytes: len(body)}
	retained := work.parserPinCounts[0]
	for _, pinned := range work.parserPins[0][:retained] {
		if pinned == ref {
			return nil
		}
	}
	if retained < legacyNormalizationPrefixFanout {
		work.parserPins[0][retained] = ref
		work.parserPinCounts[0]++
		return nil
	}
	for level := 1; level < len(work.parserPins); level++ {
		count := work.parserPinCounts[level]
		for _, retained := range work.parserPins[level][:count] {
			if retained == ref {
				return nil
			}
		}
		work.parserPins[level][count] = ref
		work.parserPinCounts[level]++
		if work.parserPinCounts[level] < legacyNormalizationPrefixFanout {
			return nil
		}
		var err error
		ref, err = work.parserPinNode(ctx, work.parserPins[level][:])
		if err != nil {
			return err
		}
		clear(work.parserPins[level][:])
		work.parserPinCounts[level] = 0
	}
	return domain.ErrDocumentResourceLimit
}

func (work *legacyNormalizationCheckpointWork) parserPinNode(ctx context.Context, refs []legacyNormalizationBufferRef) (legacyNormalizationBufferRef, error) {
	node := struct {
		Version uint8                          `json:"v"`
		Entries []legacyNormalizationBufferRef `json:"e"`
	}{Version: 1, Entries: refs}
	pins, err := json.Marshal(node)
	if err != nil {
		return legacyNormalizationBufferRef{}, err
	}
	pinHash := domain.HashContent(pins)
	if err := work.stageParserObjects(ctx, []legacyByteCheckpointObject{{hash: pinHash, data: pins, structure: 1}}); err != nil {
		return legacyNormalizationBufferRef{}, err
	}
	return legacyNormalizationBufferRef{Hash: pinHash, Bytes: len(pins)}, nil
}

func (work *legacyNormalizationCheckpointWork) finishParserPins(ctx context.Context) error {
	if work.next.Parser.Pins.Bytes != 0 {
		return nil
	}
	var roots [8]legacyNormalizationBufferRef
	count := 0
	for level, refs := range work.parserPins {
		if size := work.parserPinCounts[level]; size > 0 {
			ref, err := work.parserPinNode(ctx, refs[:size])
			if err != nil {
				return err
			}
			roots[count], count = ref, count+1
		}
	}
	if count == 0 {
		return domain.ErrIntegrity
	}
	ref, err := work.parserPinNode(ctx, roots[:count])
	if err == nil {
		work.next.Parser.Pins = ref
	}
	return err
}

func (work *legacyNormalizationCheckpointWork) parserIO() domain.LegacyNormalizationParserCheckpointIO {
	return domain.LegacyNormalizationParserCheckpointIO{Put: work.putParserObject,
		Read: func(ctx context.Context, hash domain.ContentHash, size int) ([]byte, error) {
			return work.readParserObject(ctx, hash, size)
		}}
}

func (work *legacyNormalizationCheckpointWork) writeParser(ctx context.Context) (ready bool, failure error) {
	parser := work.next.Parser
	size, partBytes, err := work.capture.ParserInputGeometry(ctx)
	if err != nil {
		return false, err
	}
	if partBytes != domain.LegacyNormalizationPartBytes {
		return false, domain.ErrIntegrity
	}
	if parser.Input.Tree.DataBytes < size {
		parser.Input, err = work.store.stageLegacyNormalizationPrefix(ctx, work.job, work.state, parser.Input, 1,
			partBytes, size, work.capture.ReadParserInput)
		return false, err
	}
	binding, store := work.parserBinding(), work.parserIO()
	ctx, batch, err := work.deferBatch(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		failure = batch.finish(ctx, failure)
		if failure != nil {
			ready = false
			work.capture.Close()
		}
	}()
	for quantum := 0; quantum < domain.DocLegacyByteBackfillBatchBlocks; quantum++ {
		if parser.Ref.Bytes == 0 {
			ref, complete, err := work.capture.ParserStep(ctx, binding, store, legacyNormalizationParserHeapBudget-legacyNormalizationParserBatchHeap)
			if err != nil {
				return false, err
			}
			if complete {
				parser.Ref = legacyNormalizationBufferRef{Hash: ref.Hash, Bytes: ref.Bytes}
			}
			continue
		}
		if work.next.State.Staging() && work.next.Encoder == nil {
			ref, complete, err := work.capture.StageStep(ctx, binding, store, legacyNormalizationParserHeapBudget-legacyNormalizationParserBatchHeap)
			if err != nil {
				return false, err
			}
			if complete {
				work.next.Encoder = &legacyNormalizationBufferRef{Hash: ref.Hash, Bytes: ref.Bytes}
			}
			continue
		}
		if err := work.finishParserPins(ctx); err != nil {
			return false, err
		}
		return parser.valid(work.state), nil
	}
	return false, nil
}

func (work *legacyNormalizationCheckpointWork) readParserInput(ctx context.Context, offset, length int) ([]byte, error) {
	return work.readPrefix(ctx, work.root.Parser.Input, offset, length)
}

func (work *legacyNormalizationCheckpointWork) restoreParserParts(ctx context.Context) (bool, error) {
	prefix := work.root.Parser.Input
	if work.parserParts == nil {
		retained, err := work.retainedBytes()
		if err != nil {
			return false, err
		}
		var planned rootPublicationHeapEstimate
		planned.allocation(int64(prefix.Tree.Leaves), int64(unsafe.Sizeof(domain.ConversationManifestChunk{})))
		if planned.err != nil || retained > legacyNormalizationParserHeapBudget-planned.bytes {
			return false, domain.ErrDocumentResourceLimit
		}
		work.parserParts = make([]domain.ConversationManifestChunk, 0, prefix.Tree.Leaves)
	}
	if len(work.parserParts) == prefix.Tree.Leaves {
		return true, nil
	}
	editor := legacyNormalizationPrefixEditor{chunkBytes: prefix.ChunkBytes,
		read: func(ctx context.Context, hash domain.ContentHash, size int) ([]byte, error) {
			return work.store.readLegacyNormalizationCheckpointObject(ctx, work.job, work.state, hash, size)
		}}
	tree := prefix.Tree
	offset := len(work.parserParts) * prefix.ChunkBytes
	for tree.Height > 0 {
		children, err := editor.children(ctx, tree)
		if err != nil {
			return false, err
		}
		found := false
		for _, child := range children {
			if offset < child.DataBytes {
				tree, found = child, true
				break
			}
			offset -= child.DataBytes
		}
		if !found {
			return false, domain.ErrPreparedDocRangeObject
		}
	}
	if offset != 0 || tree.Leaves != 1 {
		return false, domain.ErrPreparedDocRangeObject
	}
	work.parserParts = append(work.parserParts, domain.ConversationManifestChunk{Hash: tree.Hash, Bytes: int64(tree.DataBytes)})
	return false, nil
}
