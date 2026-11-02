package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unsafe"
)

const DocLegacyByteBackfillBatchBlocks = 8
const MaxDocLegacyByteBackfillSpecBytes = 1024

type DocLegacyByteBackfillSpec struct {
	Version     int         `json:"version"`
	Reference   DocumentRef `json:"reference"`
	StreamBytes int64       `json:"stream_bytes"`
	BlockBytes  int         `json:"block_bytes"`
	Blocks      int         `json:"blocks"`
}

func (spec DocLegacyByteBackfillSpec) Validate() error {
	if spec.Version != 1 || spec.Reference.Validate() != nil || spec.Reference.Identity != DocumentIdentityLegacy ||
		spec.BlockBytes != ConversationManifestChunkBytes || spec.StreamBytes < 0 ||
		spec.StreamBytes > int64(MaxConversationDocumentBytes-canonicalFrameBytes-2) ||
		spec.Blocks != int((spec.StreamBytes+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes) {
		return ErrIntegrity
	}
	return nil
}

func (spec DocLegacyByteBackfillSpec) Encode() ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	raw, err := canonicalJSON(spec)
	if err != nil || len(raw) > MaxDocLegacyByteBackfillSpecBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocLegacyByteBackfillSpec(expected ContentHash, raw []byte) (DocLegacyByteBackfillSpec, error) {
	var spec DocLegacyByteBackfillSpec
	if len(raw) == 0 || len(raw) > MaxDocLegacyByteBackfillSpecBytes || ValidateContentHash(expected) != nil ||
		HashContent(raw) != expected || json.Unmarshal(raw, &spec) != nil {
		return DocLegacyByteBackfillSpec{}, ErrIntegrity
	}
	canonical, err := spec.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocLegacyByteBackfillSpec{}, ErrIntegrity
	}
	return spec, nil
}

type LegacyByteBackfill struct {
	spec                   DocLegacyByteBackfillSpec
	key                    ContentHash
	stream                 canonicalSegments
	source                 VerifiedDocStream
	descriptors            []ConversationManifestChunk
	canonicalRetainedBytes int64
	sealed                 bool
}

func NewLegacyByteBackfill(ctx context.Context, proof VerifiedSessionDoc) (LegacyByteBackfill, error) {
	if err := ctx.Err(); err != nil {
		return LegacyByteBackfill{}, err
	}
	if !proof.Valid() {
		return LegacyByteBackfill{}, ErrIntegrity
	}
	if proof.DocumentRef().Identity != DocumentIdentityLegacy {
		return LegacyByteBackfill{}, ErrUnsupportedDocumentIdentity
	}
	envelope, stream, err := proof.segmentedContext(ctx)
	if err != nil {
		return LegacyByteBackfill{}, err
	}
	if stream.size > MaxConversationDocumentBytes-canonicalFrameBytes-len(envelope) {
		return LegacyByteBackfill{}, ErrDocumentResourceLimit
	}
	spec := DocLegacyByteBackfillSpec{Version: 1, Reference: proof.DocumentRef(), StreamBytes: int64(stream.size),
		BlockBytes: ConversationManifestChunkBytes, Blocks: (stream.size + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes}
	spec.Reference.Hash = ContentHash(strings.Clone(string(spec.Reference.Hash)))
	raw, err := spec.Encode()
	if err != nil {
		return LegacyByteBackfill{}, err
	}
	if err := ctx.Err(); err != nil {
		return LegacyByteBackfill{}, err
	}
	canonicalRetainedBytes := int64(stream.size)
	if len(stream.parts) > 0 && proof.chunks == nil {
		canonicalRetainedBytes = int64(len(proof.canonical))
	}
	return LegacyByteBackfill{spec: spec, key: HashContent(raw), stream: stream, canonicalRetainedBytes: canonicalRetainedBytes, sealed: true}, nil
}

func (plan LegacyByteBackfill) Valid() bool {
	if !plan.sealed || plan.spec.Validate() != nil || ValidateContentHash(plan.key) != nil {
		return false
	}
	if plan.source == nil {
		return plan.descriptors == nil && int64(plan.stream.size) == plan.spec.StreamBytes && plan.canonicalRetainedBytes >= plan.spec.StreamBytes
	}
	return plan.stream.parts == nil && plan.stream.offsets == nil && plan.stream.size == 0 &&
		plan.canonicalRetainedBytes == 0 && len(plan.descriptors) == plan.spec.Blocks
}

func (plan LegacyByteBackfill) Spool(ctx context.Context, source VerifiedDocStream) (LegacyByteBackfill, error) {
	preparation, err := NewLegacyByteSpoolPreparation(ctx, plan, source)
	if err != nil {
		return LegacyByteBackfill{}, err
	}
	for {
		complete, err := preparation.Step(ctx)
		if err != nil {
			return LegacyByteBackfill{}, err
		}
		if complete {
			return preparation.Plan()
		}
	}
}

func (plan LegacyByteBackfill) RetainedBytes() (int64, error) {
	if !plan.Valid() {
		return 0, ErrIntegrity
	}
	retained := int64(unsafe.Sizeof(plan)) + int64(len(plan.spec.Reference.Hash)+len(plan.spec.Reference.Identity)+len(plan.key)) + plan.canonicalRetainedBytes
	retained += int64(cap(plan.stream.parts))*int64(unsafe.Sizeof("")) + int64(cap(plan.stream.offsets))*int64(unsafe.Sizeof(int(0)))
	retained += int64(cap(plan.descriptors)) * int64(unsafe.Sizeof(ConversationManifestChunk{}))
	for _, descriptor := range plan.descriptors {
		retained += int64(len(descriptor.Hash))
	}
	return retained, nil
}

func legacyByteBackfillRead(ctx context.Context, source VerifiedDocStream, offset int, descriptor ConversationManifestChunk) (body []byte, failure error) {
	defer func() {
		if errors.Is(failure, ErrIntegrity) && !errors.Is(failure, ErrPreparedDocRangeObject) {
			failure = errors.Join(ErrPreparedDocRangeObject, failure)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if descriptor.Bytes <= 0 || descriptor.Bytes > ConversationManifestChunkBytes || ValidateContentHash(descriptor.Hash) != nil {
		return nil, ErrIntegrity
	}
	current, err := source.Read(ctx, offset, int(descriptor.Bytes))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(current) > ConversationManifestChunkBytes {
		return nil, ErrIntegrity
	}
	body = bytes.Clone(current)
	if int64(len(body)) != descriptor.Bytes {
		return nil, ErrIntegrity
	}
	hash, err := HashContentContext(ctx, body)
	if err != nil {
		return nil, err
	}
	if hash != descriptor.Hash {
		return nil, ErrIntegrity
	}
	return body, nil
}

func (plan LegacyByteBackfill) Spec() (DocLegacyByteBackfillSpec, error) {
	if !plan.Valid() {
		return DocLegacyByteBackfillSpec{}, ErrIntegrity
	}
	return plan.spec, nil
}

func (plan LegacyByteBackfill) Key() (ContentHash, error) {
	if !plan.Valid() {
		return "", ErrIntegrity
	}
	return plan.key, nil
}

type VerifiedLegacyByteBlock struct {
	reference  DocumentRef
	plan       ContentHash
	ordinal    int
	descriptor ConversationManifestChunk
	body       string
	sealed     bool
}

func (block VerifiedLegacyByteBlock) Valid() bool {
	return block.sealed && block.reference.Validate() == nil && block.reference.Identity == DocumentIdentityLegacy &&
		ValidateContentHash(block.plan) == nil && block.ordinal >= 0 && ValidateContentHash(block.descriptor.Hash) == nil &&
		len(block.body) > 0 && len(block.body) <= ConversationManifestChunkBytes && int64(len(block.body)) == block.descriptor.Bytes
}

func (block VerifiedLegacyByteBlock) Reference() DocumentRef {
	if !block.Valid() {
		return DocumentRef{}
	}
	return block.reference
}

func (block VerifiedLegacyByteBlock) PlanKey() ContentHash {
	if !block.Valid() {
		return ""
	}
	return block.plan
}

func (block VerifiedLegacyByteBlock) Ordinal() int {
	if !block.Valid() {
		return -1
	}
	return block.ordinal
}

func (block VerifiedLegacyByteBlock) Descriptor() ConversationManifestChunk {
	if !block.Valid() {
		return ConversationManifestChunk{}
	}
	return block.descriptor
}

func (block VerifiedLegacyByteBlock) Bytes() []byte {
	if !block.Valid() {
		return nil
	}
	return []byte(block.body)
}

func (plan LegacyByteBackfill) Block(ctx context.Context, ordinal int) (VerifiedLegacyByteBlock, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedLegacyByteBlock{}, err
	}
	if !plan.Valid() {
		return VerifiedLegacyByteBlock{}, ErrIntegrity
	}
	if ordinal < 0 || ordinal >= plan.spec.Blocks {
		return VerifiedLegacyByteBlock{}, ErrValidation
	}
	offset := ordinal * ConversationManifestChunkBytes
	length := min(int64(ConversationManifestChunkBytes), plan.spec.StreamBytes-int64(offset))
	if plan.source != nil {
		descriptor := plan.descriptors[ordinal]
		if descriptor.Bytes != length {
			return VerifiedLegacyByteBlock{}, ErrIntegrity
		}
		body, err := legacyByteBackfillRead(ctx, plan.source, offset, descriptor)
		if err != nil {
			return VerifiedLegacyByteBlock{}, err
		}
		canonical := string(body)
		if err := ctx.Err(); err != nil {
			return VerifiedLegacyByteBlock{}, err
		}
		return VerifiedLegacyByteBlock{reference: plan.spec.Reference, plan: plan.key, ordinal: ordinal,
			descriptor: descriptor, body: canonical, sealed: true}, nil
	}
	body := make([]byte, 0, length)
	var failure error
	plan.stream.ranges(canonicalSpan{offset: offset, length: int(length)}, func(part string) {
		for len(part) > 0 && failure == nil {
			if failure = ctx.Err(); failure != nil {
				return
			}
			count := min(len(part), canonicalCheckBytes)
			body = append(body, part[:count]...)
			part = part[count:]
		}
	})
	if failure != nil {
		return VerifiedLegacyByteBlock{}, failure
	}
	if int64(len(body)) != length {
		return VerifiedLegacyByteBlock{}, ErrIntegrity
	}
	hash, err := HashContentContext(ctx, body)
	if err != nil {
		return VerifiedLegacyByteBlock{}, err
	}
	return VerifiedLegacyByteBlock{reference: plan.spec.Reference, plan: plan.key, ordinal: ordinal,
		descriptor: ConversationManifestChunk{Hash: hash, Bytes: int64(length)}, body: string(body), sealed: true}, nil
}

type DocLegacyByteBackfillProgress struct {
	Plan        ContentHash
	Next        int
	CopiedBytes int64
	Complete    bool
}

func (plan LegacyByteBackfill) CopyBatch(ctx context.Context, start, maxBlocks int, save func(context.Context, VerifiedLegacyByteBlock) error) (DocLegacyByteBackfillProgress, error) {
	if err := ctx.Err(); err != nil {
		return DocLegacyByteBackfillProgress{}, err
	}
	if !plan.Valid() {
		return DocLegacyByteBackfillProgress{}, ErrIntegrity
	}
	if start < 0 || start > plan.spec.Blocks || maxBlocks < 1 || maxBlocks > DocLegacyByteBackfillBatchBlocks || start != plan.spec.Blocks && save == nil {
		return DocLegacyByteBackfillProgress{}, ErrValidation
	}
	progress := DocLegacyByteBackfillProgress{Plan: plan.key, Next: start, Complete: start == plan.spec.Blocks}
	for progress.Next < min(plan.spec.Blocks, start+maxBlocks) {
		block, err := plan.Block(ctx, progress.Next)
		if err != nil {
			return progress, err
		}
		if err := save(ctx, block); err != nil {
			return progress, err
		}
		progress.Next++
		progress.CopiedBytes += block.descriptor.Bytes
		progress.Complete = progress.Next == plan.spec.Blocks
	}
	return progress, ctx.Err()
}
