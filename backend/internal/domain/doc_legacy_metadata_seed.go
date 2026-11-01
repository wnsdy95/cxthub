package domain

import (
	"bytes"
	"context"
	"strings"
	"unsafe"
)

type LegacyRangeMetadataSeed struct {
	reference DocumentRef
	key       ContentHash
	envelope  []byte
	bytes     int64
	sealed    bool
}

func NewLegacyRangeMetadataSeed(ctx context.Context, proof VerifiedSessionDoc, plan LegacyByteBackfill) (LegacyRangeMetadataSeed, error) {
	if err := ctx.Err(); err != nil {
		return LegacyRangeMetadataSeed{}, err
	}
	if !proof.Valid() {
		return LegacyRangeMetadataSeed{}, ErrIntegrity
	}
	if proof.DocumentRef().Identity != DocumentIdentityLegacy {
		return LegacyRangeMetadataSeed{}, ErrUnsupportedDocumentIdentity
	}
	if !plan.Valid() || plan.spec.Reference != proof.DocumentRef() {
		return LegacyRangeMetadataSeed{}, ErrIntegrity
	}
	envelope, stream, err := proof.segmentedContext(ctx)
	if err != nil {
		return LegacyRangeMetadataSeed{}, err
	}
	if int64(stream.size) != plan.spec.StreamBytes {
		return LegacyRangeMetadataSeed{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return LegacyRangeMetadataSeed{}, err
	}
	encoded := make([]byte, len(envelope))
	for from := 0; from < len(envelope); {
		if err := ctx.Err(); err != nil {
			return LegacyRangeMetadataSeed{}, err
		}
		to := min(from+canonicalCheckBytes, len(envelope))
		copy(encoded[from:to], envelope[from:to])
		from = to
	}
	if err := ctx.Err(); err != nil {
		return LegacyRangeMetadataSeed{}, err
	}
	return LegacyRangeMetadataSeed{reference: DocumentRef{Hash: ContentHash(strings.Clone(string(plan.spec.Reference.Hash))), Identity: DocumentIdentityLegacy},
		key: ContentHash(strings.Clone(string(plan.key))), envelope: encoded, bytes: plan.spec.StreamBytes, sealed: true}, nil
}

func (seed LegacyRangeMetadataSeed) valid() bool {
	return seed.sealed && seed.reference.Validate() == nil && seed.reference.Identity == DocumentIdentityLegacy &&
		ValidateContentHash(seed.key) == nil && len(seed.envelope) != 0 && seed.bytes >= 0
}

func (seed LegacyRangeMetadataSeed) Reference() DocumentRef {
	if !seed.valid() {
		return DocumentRef{}
	}
	return seed.reference
}

func (seed LegacyRangeMetadataSeed) PlanKey() ContentHash {
	if !seed.valid() {
		return ""
	}
	return seed.key
}

func (seed LegacyRangeMetadataSeed) EnvelopeBytes() int {
	if !seed.valid() {
		return 0
	}
	return len(seed.envelope)
}

func (seed LegacyRangeMetadataSeed) ReadEnvelope(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !seed.valid() || offset < 0 || length < 0 || offset > len(seed.envelope) || length > len(seed.envelope)-offset {
		return nil, ErrIntegrity
	}
	if length > LegacyCanonicalEncodingBytes {
		return nil, ErrDocumentResourceLimit
	}
	return bytes.Clone(seed.envelope[offset : offset+length]), ctx.Err()
}

func (seed LegacyRangeMetadataSeed) NewPreparation(ctx context.Context, plan LegacyByteBackfill) (*LegacyRangeMetadataPreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !seed.valid() || !plan.Valid() || plan.source == nil || plan.spec.Reference != seed.reference || plan.key != seed.key || plan.spec.StreamBytes != seed.bytes {
		return nil, ErrIntegrity
	}
	for ordinal, descriptor := range plan.descriptors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ValidateContentHash(descriptor.Hash) != nil || descriptor.Bytes != min(int64(ConversationManifestChunkBytes), plan.spec.StreamBytes-int64(ordinal)*ConversationManifestChunkBytes) {
			return nil, ErrIntegrity
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity := DocumentIdentityLegacy
	generation := DocRangeGeneration{Version: 5, SourceIdentity: &identity, Algorithm: 1, Hash: seed.reference.Hash, Envelope: seed.envelope}
	if len(seed.envelope) > MaxConversationManifestBytes {
		generation.Version = 6
		generation.EnvelopeRef = &DocRangeEnvelopeReference{Bytes: len(seed.envelope)}
	}
	return &LegacyRangeMetadataPreparation{plan: plan,
		generation: generation,
		metadata:   VerifiedLegacyDerivedRangeMetadata{objects: make([]legacyDerivedRangeObjectMetadata, 0)},
		scanner:    legacyStreamScanner{limit: int(seed.bytes)}}, nil
}

func (seed LegacyRangeMetadataSeed) RetainedBytes() (int64, error) {
	if !seed.valid() {
		return 0, ErrIntegrity
	}
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(seed)))
	estimate.allocation(int64(cap(seed.envelope)), 1)
	for _, value := range []string{string(seed.reference.Hash), string(seed.reference.Identity), string(seed.key)} {
		estimate.allocation(int64(len(value)), 1)
	}
	return estimate.bytes, estimate.err
}
