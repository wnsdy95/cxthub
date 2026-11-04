package domain

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"unsafe"
)

type legacyRangePages struct {
	entries  []DocRangeEntry
	hashes   []ContentHash
	hashHeap docRangeHeaderHeapEstimate
	count    int
	rendered int
	bytes    int64
}

func (pages *legacyRangePages) add(ctx context.Context, entry DocRangeEntry, emit func(DocRangeObject) error) error {
	entry.RenderedBefore = pages.rendered
	if !entry.EmptyReasoning {
		pages.rendered++
	}
	pages.count++
	pages.bytes = int64(entry.Offset + entry.Length)
	pages.entries = append(pages.entries, entry)
	if len(pages.entries) == DocRangePageEvents {
		return pages.flush(ctx, emit)
	}
	return ctx.Err()
}

func (pages *legacyRangePages) flush(ctx context.Context, emit func(DocRangeObject) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(pages.entries) == 0 {
		return nil
	}
	raw, err := json.Marshal(pages.entries)
	if err != nil || len(raw) > MaxDocRangePageBytes {
		return ErrIntegrity
	}
	hash := HashContent(raw)
	pages.hashHeap.allocation(int64(len(hash)), 1)
	if pages.hashHeap.err != nil {
		return pages.hashHeap.err
	}
	pages.hashes = append(pages.hashes, hash)
	if err := emit(DocRangeObject{Hash: hash, Bytes: raw}); err != nil {
		return err
	}
	clear(pages.entries)
	pages.entries = pages.entries[:0]
	return ctx.Err()
}

type LegacyRangeMetadataPreparation struct {
	plan              LegacyByteBackfill
	generation        DocRangeGeneration
	pages             legacyRangePages
	metadata          VerifiedLegacyDerivedRangeMetadata
	scanner           legacyStreamScanner
	next              int
	events            *docRangeDirectoryWork
	chunks            *docRangeChunkDirectoryWork
	done              bool
	failure           error
	envelopePosition  int
	envelopeHashState []byte
}

func NewLegacyRangeMetadataPreparation(ctx context.Context, proof VerifiedSessionDoc, plan LegacyByteBackfill) (*LegacyRangeMetadataPreparation, error) {
	seed, err := NewLegacyRangeMetadataSeed(ctx, proof, plan)
	if err != nil {
		return nil, err
	}
	return seed.NewPreparation(ctx, plan)
}

func (preparation *LegacyRangeMetadataPreparation) valid() bool {
	return preparation != nil && preparation.failure == nil && preparation.plan.Valid() && preparation.plan.source != nil &&
		preparation.generation.Hash == preparation.plan.spec.Reference.Hash && preparation.metadata.objects != nil
}

func (preparation *LegacyRangeMetadataPreparation) Reference() DocumentRef {
	if !preparation.valid() {
		return DocumentRef{}
	}
	return preparation.plan.spec.Reference
}

func (preparation *LegacyRangeMetadataPreparation) PlanKey() ContentHash {
	if !preparation.valid() {
		return ""
	}
	return preparation.plan.key
}

func (preparation *LegacyRangeMetadataPreparation) ObjectCount() int {
	if !preparation.valid() {
		return 0
	}
	return len(preparation.metadata.objects)
}

func (preparation *LegacyRangeMetadataPreparation) Metadata() (VerifiedLegacyDerivedRangeMetadata, error) {
	if !preparation.valid() {
		return VerifiedLegacyDerivedRangeMetadata{}, ErrIntegrity
	}
	if !preparation.done {
		return VerifiedLegacyDerivedRangeMetadata{}, ErrDocRangeIndexPending
	}
	return preparation.metadata, nil
}

func (preparation *LegacyRangeMetadataPreparation) Step(ctx context.Context, save func(context.Context, DocRangeObject) error) (complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !preparation.valid() {
		return false, ErrIntegrity
	}
	if preparation.done {
		return true, nil
	}
	defer func() {
		preparation.scanner.visit = nil
		if failure != nil {
			preparation.failure = failure
		}
	}()
	emit := func(object DocRangeObject) error { return preparation.metadata.add(ctx, object, save) }
	if preparation.generation.EnvelopeRef != nil && preparation.generation.Envelope != nil {
		return false, preparation.prepareEnvelope(ctx)
	}
	if preparation.events != nil || preparation.chunks != nil {
		return preparation.advanceDirectories(ctx, emit)
	}
	preparation.scanner.visit = func(span canonicalSpan, body []byte) (int, error) {
		entry, err := preparation.plan.rangeEntry(ctx, preparation.pages.count, span.offset, body)
		if err != nil {
			return 0, err
		}
		if err := preparation.pages.add(ctx, entry, emit); err != nil {
			return 0, err
		}
		if cap(preparation.scanner.event) > canonicalCheckBytes {
			preparation.scanner.event = nil
		}
		return entry.Seq, nil
	}
	if preparation.next < preparation.plan.spec.Blocks {
		block, err := preparation.plan.Block(ctx, preparation.next)
		if err != nil {
			return false, err
		}
		if err := preparation.scanner.add(ctx, block.Bytes()); err != nil {
			return false, err
		}
		preparation.next++
	}
	if preparation.next != preparation.plan.spec.Blocks {
		return false, nil
	}
	if err := preparation.scanner.finish(ctx); err != nil {
		return false, err
	}
	if int64(preparation.scanner.offset) != preparation.plan.spec.StreamBytes || preparation.pages.bytes != preparation.plan.spec.StreamBytes {
		return false, ErrIntegrity
	}
	if err := preparation.pages.flush(ctx, emit); err != nil {
		return false, err
	}
	preparation.generation.EventCount = int64(preparation.pages.count)
	preparation.generation.StreamBytes = preparation.pages.bytes
	var err error
	preparation.events, err = startDocRangeDirectoryWork(preparation.pages.hashes, preparation.pages.hashHeap)
	if err != nil {
		return false, err
	}
	preparation.chunks, err = newDocRangeChunkDirectoryWork(preparation.plan.descriptors)
	if err != nil {
		return false, err
	}
	return false, nil
}

func (preparation *LegacyRangeMetadataPreparation) advanceDirectories(ctx context.Context, emit func(DocRangeObject) error) (bool, error) {
	if preparation.events != nil {
		complete, err := preparation.events.step(ctx, emit)
		if err != nil {
			return false, err
		}
		if complete {
			preparation.generation.EventPages = preparation.events.root
			preparation.events, preparation.pages.hashes = nil, nil
			preparation.pages.hashHeap = docRangeHeaderHeapEstimate{}
		}
		return false, nil
	}
	complete, err := preparation.chunks.step(ctx, emit)
	if err != nil || !complete {
		return false, err
	}
	preparation.generation.ChunkPages = preparation.chunks.root
	preparation.chunks = nil
	preparation.metadata, err = preparation.metadata.seal(ctx, preparation.generation, preparation.plan.key, preparation.plan.spec.Reference)
	if err != nil {
		return false, err
	}
	preparation.done = true
	return true, nil
}

func (preparation *LegacyRangeMetadataPreparation) prepareEnvelope(ctx context.Context) error {
	digest := sha256.New()
	if len(preparation.envelopeHashState) != 0 {
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(preparation.envelopeHashState); err != nil {
			return ErrIntegrity
		}
	}
	envelope := preparation.generation.Envelope
	to := min(preparation.envelopePosition+LegacyCanonicalEncodingBytes, len(envelope))
	if preparation.envelopePosition < 0 || preparation.envelopePosition >= len(envelope) {
		return ErrIntegrity
	}
	digest.Write(envelope[preparation.envelopePosition:to])
	if err := ctx.Err(); err != nil {
		return err
	}
	if to == len(envelope) {
		preparation.generation.EnvelopeRef.Hash = ContentHash(contentHashPrefix + hex.EncodeToString(digest.Sum(nil)))
		preparation.generation.Envelope = nil
		preparation.envelopeHashState, preparation.envelopePosition = nil, 0
		return nil
	}
	state, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
	if err == nil {
		preparation.envelopeHashState, preparation.envelopePosition = state, to
	}
	return err
}

func (preparation *LegacyRangeMetadataPreparation) RetainedBytes() (int64, error) {
	if !preparation.valid() {
		return 0, ErrIntegrity
	}
	planBytes, err := preparation.plan.RetainedBytes()
	if err != nil {
		return 0, err
	}
	estimate := docRangeHeaderHeapEstimate{bytes: planBytes}
	estimate.allocation(1, int64(unsafe.Sizeof(*preparation)))
	estimate.allocation(1, int64(unsafe.Sizeof(DocumentIdentityLegacy)))
	estimate.allocation(int64(cap(preparation.generation.Envelope)), 1)
	estimate.allocation(int64(cap(preparation.envelopeHashState)), 1)
	if preparation.generation.EnvelopeRef != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(DocRangeEnvelopeReference{})))
		estimate.allocation(int64(len(preparation.generation.EnvelopeRef.Hash)), 1)
	}
	estimate.allocation(int64(cap(preparation.scanner.event)), 1)
	estimate.allocation(int64(cap(preparation.pages.entries)), int64(unsafe.Sizeof(DocRangeEntry{})))
	estimate.allocation(int64(cap(preparation.pages.hashes)), int64(unsafe.Sizeof(ContentHash(""))))
	estimate.allocation(int64(cap(preparation.metadata.objects)), int64(unsafe.Sizeof(legacyDerivedRangeObjectMetadata{})))
	estimate.allocation(int64(cap(preparation.metadata.descriptor)), 1)
	preparation.events.retained(&estimate)
	preparation.chunks.retained(&estimate)
	for _, entry := range preparation.pages.entries[:cap(preparation.pages.entries)] {
		for _, value := range []string{string(entry.Hash), entry.Role, string(entry.Kind), entry.ToolName} {
			estimate.allocation(int64(len(value)), 1)
		}
	}
	estimate.include(preparation.pages.hashHeap)
	estimate.include(preparation.metadata.objectHeap)
	for _, value := range []string{string(preparation.generation.Hash), string(preparation.generation.EventPages.Hash),
		string(preparation.generation.ChunkPages.Hash), string(preparation.metadata.descriptorHash),
		string(preparation.metadata.reference.Hash), string(preparation.metadata.reference.Identity), string(preparation.metadata.planKey)} {
		estimate.allocation(int64(len(value)), 1)
	}
	return estimate.bytes, estimate.err
}
