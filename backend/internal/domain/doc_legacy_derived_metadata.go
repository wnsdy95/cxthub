package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"
)

var ErrPreparedDocRangeObject = fmt.Errorf("%w: prepared range object is corrupt", ErrIntegrity)

type legacyDerivedRangeObjectMetadata struct {
	hash  ContentHash
	bytes int64
}

type VerifiedLegacyDerivedRangeMetadata struct {
	descriptor     []byte
	descriptorHash ContentHash
	reference      DocumentRef
	planKey        ContentHash
	objects        []legacyDerivedRangeObjectMetadata
	objectHeap     docRangeHeaderHeapEstimate
	sealed         bool
}

func PrepareLegacyDerivedRangeMetadata(ctx context.Context, proof VerifiedSessionDoc, save func(context.Context, DocRangeObject) error) (VerifiedLegacyDerivedRangeMetadata, error) {
	metadata := VerifiedLegacyDerivedRangeMetadata{
		objects: make([]legacyDerivedRangeObjectMetadata, 0),
	}
	generation, key, err := emitLegacyDerivedDocByteRangeGeneration(ctx, proof, func(object DocRangeObject) error {
		return metadata.add(ctx, object, save)
	})
	if err != nil {
		return VerifiedLegacyDerivedRangeMetadata{}, err
	}
	return metadata.seal(ctx, generation, key, proof.DocumentRef())
}

func (metadata *VerifiedLegacyDerivedRangeMetadata) add(ctx context.Context, object DocRangeObject, save func(context.Context, DocRangeObject) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if save == nil || metadata.sealed {
		return ErrValidation
	}
	body := bytes.Clone(object.Bytes)
	if len(body) == 0 || len(body) > MaxDocRangePageBytes || HashContent(body) != object.Hash {
		return ErrIntegrity
	}
	expected := legacyDerivedRangeObjectMetadata{hash: ContentHash(strings.Clone(string(object.Hash))), bytes: int64(len(body))}
	metadata.objectHeap.allocation(int64(len(expected.hash)), 1)
	if metadata.objectHeap.err != nil {
		return metadata.objectHeap.err
	}
	metadata.objects = append(metadata.objects, expected)
	if err := save(ctx, DocRangeObject{Hash: expected.hash, Bytes: body}); err != nil {
		return err
	}
	return ctx.Err()
}

func (metadata VerifiedLegacyDerivedRangeMetadata) seal(ctx context.Context, generation DocRangeGeneration, key ContentHash, reference DocumentRef) (VerifiedLegacyDerivedRangeMetadata, error) {
	encoded, err := generation.Encode()
	if err != nil {
		return VerifiedLegacyDerivedRangeMetadata{}, err
	}
	metadata.descriptor = bytes.Clone(encoded)
	metadata.descriptorHash = HashContent(encoded)
	metadata.reference = DocumentRef{
		Hash:     ContentHash(strings.Clone(string(reference.Hash))),
		Identity: DocumentIdentity(strings.Clone(string(reference.Identity))),
	}
	metadata.planKey = ContentHash(strings.Clone(string(key)))
	if err := ctx.Err(); err != nil {
		return VerifiedLegacyDerivedRangeMetadata{}, err
	}
	metadata.sealed = true
	return metadata, nil
}

func (metadata VerifiedLegacyDerivedRangeMetadata) Valid() bool {
	return metadata.sealed && metadata.reference.Validate() == nil && metadata.reference.Identity == DocumentIdentityLegacy &&
		ValidateContentHash(metadata.planKey) == nil && ValidateContentHash(metadata.descriptorHash) == nil &&
		len(metadata.descriptor) > 0 && len(metadata.descriptor) <= MaxDocRangeGenerationBytes &&
		HashContent(metadata.descriptor) == metadata.descriptorHash && metadata.objects != nil
}

func (metadata VerifiedLegacyDerivedRangeMetadata) Reference() DocumentRef {
	if !metadata.Valid() {
		return DocumentRef{}
	}
	return metadata.reference
}

func (metadata VerifiedLegacyDerivedRangeMetadata) PlanKey() ContentHash {
	if !metadata.Valid() {
		return ""
	}
	return metadata.planKey
}

func (metadata VerifiedLegacyDerivedRangeMetadata) Descriptor() (DocRangeGeneration, error) {
	if !metadata.Valid() {
		return DocRangeGeneration{}, ErrIntegrity
	}
	return DecodeDocRangeGeneration(metadata.reference.Hash, metadata.descriptorHash, bytes.Clone(metadata.descriptor))
}

func (metadata VerifiedLegacyDerivedRangeMetadata) ObjectCount() int {
	if !metadata.Valid() {
		return 0
	}
	return len(metadata.objects)
}

func (metadata VerifiedLegacyDerivedRangeMetadata) Object(ctx context.Context, ordinal int, load func(context.Context, int) ([]byte, error)) (DocRangeObject, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeObject{}, err
	}
	if !metadata.Valid() {
		return DocRangeObject{}, ErrIntegrity
	}
	if ordinal < 0 || ordinal >= len(metadata.objects) || load == nil {
		return DocRangeObject{}, ErrValidation
	}
	raw, err := load(ctx, ordinal)
	if err != nil {
		if errors.Is(err, ErrIntegrity) {
			return DocRangeObject{}, errors.Join(ErrPreparedDocRangeObject, err)
		}
		return DocRangeObject{}, err
	}
	body := bytes.Clone(raw)
	if err := ctx.Err(); err != nil {
		return DocRangeObject{}, err
	}
	expected := metadata.objects[ordinal]
	if int64(len(body)) != expected.bytes || HashContent(body) != expected.hash {
		return DocRangeObject{}, ErrPreparedDocRangeObject
	}
	if err := ctx.Err(); err != nil {
		return DocRangeObject{}, err
	}
	return DocRangeObject{Hash: expected.hash, Bytes: body}, nil
}

func (metadata VerifiedLegacyDerivedRangeMetadata) RetainedBytes() (int64, error) {
	if !metadata.Valid() {
		return 0, ErrIntegrity
	}
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(metadata)))
	estimate.allocation(int64(cap(metadata.descriptor)), 1)
	estimate.allocation(int64(cap(metadata.objects)), int64(unsafe.Sizeof(legacyDerivedRangeObjectMetadata{})))
	for _, value := range []string{string(metadata.descriptorHash), string(metadata.reference.Hash), string(metadata.reference.Identity), string(metadata.planKey)} {
		estimate.allocation(int64(len(value)), 1)
	}
	estimate.include(metadata.objectHeap)
	if estimate.err != nil {
		return 0, estimate.err
	}
	return estimate.bytes, nil
}
