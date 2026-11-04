package domain

import (
	"context"
	"errors"
	"io"
	"strings"
	"unsafe"
)

type LegacyByteSpoolPreparation struct {
	input       LegacyByteBackfill
	source      VerifiedDocStream
	descriptors []ConversationManifestChunk
	written     int
	verified    int
	result      LegacyByteBackfill
	failure     error
}

func NewLegacyByteSpoolPreparation(ctx context.Context, plan LegacyByteBackfill, source VerifiedDocStream) (*LegacyByteSpoolPreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !plan.Valid() || source == nil {
		return nil, ErrIntegrity
	}
	return &LegacyByteSpoolPreparation{input: plan, source: source, descriptors: make([]ConversationManifestChunk, plan.spec.Blocks)}, nil
}

func (preparation *LegacyByteSpoolPreparation) valid() bool {
	return preparation != nil && preparation.failure == nil && preparation.source != nil && (preparation.input.Valid() || preparation.result.Valid())
}

func (preparation *LegacyByteSpoolPreparation) Reference() DocumentRef {
	if !preparation.valid() {
		return DocumentRef{}
	}
	if preparation.result.Valid() {
		return preparation.result.spec.Reference
	}
	return preparation.input.spec.Reference
}

func (preparation *LegacyByteSpoolPreparation) PlanKey() ContentHash {
	if !preparation.valid() {
		return ""
	}
	if preparation.result.Valid() {
		return preparation.result.key
	}
	return preparation.input.key
}

func (preparation *LegacyByteSpoolPreparation) SourceBytes() int64 {
	if !preparation.valid() {
		return 0
	}
	if preparation.result.Valid() {
		return preparation.result.spec.StreamBytes
	}
	return min(int64(preparation.written)*ConversationManifestChunkBytes, preparation.input.spec.StreamBytes)
}

func (preparation *LegacyByteSpoolPreparation) Plan() (LegacyByteBackfill, error) {
	if !preparation.valid() {
		return LegacyByteBackfill{}, ErrIntegrity
	}
	if !preparation.result.Valid() {
		return LegacyByteBackfill{}, ErrDocRangeIndexPending
	}
	return preparation.result, nil
}

func (preparation *LegacyByteSpoolPreparation) Step(ctx context.Context) (complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !preparation.valid() {
		return false, ErrIntegrity
	}
	if preparation.result.Valid() {
		return true, nil
	}
	defer func() {
		if failure != nil {
			preparation.failure = failure
		}
	}()
	spec := preparation.input.spec
	if preparation.written < spec.Blocks {
		block, err := preparation.input.Block(ctx, preparation.written)
		if err != nil {
			return false, err
		}
		descriptor := block.descriptor
		descriptor.Hash = ContentHash(strings.Clone(string(descriptor.Hash)))
		if err := preparation.source.Append(ctx, block.Bytes()); err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		preparation.descriptors[preparation.written] = descriptor
		preparation.written++
		return false, nil
	}
	if preparation.verified < spec.Blocks {
		ordinal := preparation.verified
		if _, err := legacyByteBackfillRead(ctx, preparation.source, ordinal*ConversationManifestChunkBytes, preparation.descriptors[ordinal]); err != nil {
			return false, err
		}
		preparation.verified++
		if preparation.verified != spec.Blocks {
			return false, nil
		}
	}
	end, err := preparation.source.Read(ctx, int(spec.StreamBytes), 0)
	if failure := ctx.Err(); failure != nil {
		return false, failure
	}
	if err != nil {
		return false, err
	}
	if len(end) != 0 {
		return false, ErrIntegrity
	}
	extra, err := preparation.source.Read(ctx, int(spec.StreamBytes), 1)
	if failure := ctx.Err(); failure != nil {
		return false, failure
	}
	if len(extra) != 0 || err == nil {
		return false, ErrIntegrity
	}
	if !errors.Is(err, ErrIntegrity) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	spec.Reference.Hash = ContentHash(strings.Clone(string(spec.Reference.Hash)))
	preparation.result = LegacyByteBackfill{spec: spec, key: ContentHash(strings.Clone(string(preparation.input.key))),
		source: preparation.source, descriptors: preparation.descriptors, sealed: true}
	preparation.input, preparation.descriptors = LegacyByteBackfill{}, nil
	return true, nil
}

func (preparation *LegacyByteSpoolPreparation) RetainedBytes() (int64, error) {
	if !preparation.valid() {
		return 0, ErrIntegrity
	}
	plan := preparation.input
	if preparation.result.Valid() {
		plan = preparation.result
	}
	retained, err := plan.RetainedBytes()
	if err != nil {
		return 0, err
	}
	estimate := docRangeHeaderHeapEstimate{bytes: retained}
	estimate.allocation(1, int64(unsafe.Sizeof(*preparation)))
	estimate.allocation(int64(cap(preparation.descriptors)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	for _, descriptor := range preparation.descriptors[:cap(preparation.descriptors)] {
		estimate.allocation(int64(len(descriptor.Hash)), 1)
	}
	return estimate.bytes, estimate.err
}
