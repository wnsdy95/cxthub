package domain

import (
	"bytes"
	"context"
	"slices"
)

func BuildStreamedLegacyDocByteRangeGeneration(ctx context.Context, doc VerifiedLegacyStream, original DocChunkManifest) (DocRangeGeneration, []DocRangeObject, error) {
	generation, objects, err := BuildStreamedLegacyDocRangeGeneration(ctx, doc)
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	normalized := doc.Manifest()
	if original.Format != ChunkFormatV2 || !bytes.Equal(original.Envelope, normalized.Envelope) ||
		!slices.Equal(original.Chunks, normalized.Chunks) || int64(doc.size) != generation.StreamBytes {
		return legacyRangeGenerationResult(ctx, generation, objects)
	}
	chunks := make([]ConversationManifestChunk, len(normalized.Chunks))
	for ordinal, hash := range normalized.Chunks {
		if err := ctx.Err(); err != nil {
			return DocRangeGeneration{}, nil, err
		}
		chunks[ordinal] = ConversationManifestChunk{Hash: hash, Bytes: min(int64(ChunkTarget), generation.StreamBytes-int64(ordinal)*ChunkTarget)}
	}
	generation.Version = 4
	generation.ChunkPages, err = buildDocRangeChunkDirectory(ctx, chunks, &objects)
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	if _, err := generation.Encode(); err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return legacyRangeGenerationResult(ctx, generation, objects)
}

func BuildLegacyDocRangeGeneration(ctx context.Context, doc VerifiedSessionDoc) (DocRangeGeneration, []DocRangeObject, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, nil, err
	}
	if !doc.Valid() || doc.DocumentRef().Identity != DocumentIdentityLegacy {
		return DocRangeGeneration{}, nil, ErrUnsupportedDocumentIdentity
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return buildLegacyDocRangeGeneration(ctx, doc.DocumentRef(), plan, func(offset, length int) ([]byte, error) {
		return doc.EventStreamRange(ctx, offset, length)
	})
}

func BuildStreamedLegacyDocRangeGeneration(ctx context.Context, doc VerifiedLegacyStream) (DocRangeGeneration, []DocRangeObject, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, nil, err
	}
	if !doc.Valid() || doc.Reference().DocumentRef().Identity != DocumentIdentityLegacy {
		return DocRangeGeneration{}, nil, ErrUnsupportedDocumentIdentity
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return buildLegacyDocRangeGeneration(ctx, doc.Reference().DocumentRef(), plan, func(offset, length int) ([]byte, error) {
		return doc.source.Read(ctx, offset, length)
	})
}

func buildLegacyDocRangeGeneration(ctx context.Context, reference DocumentRef, plan DocReadPlan, read func(int, int) ([]byte, error)) (DocRangeGeneration, []DocRangeObject, error) {
	objects := make([]DocRangeObject, 0)
	generation, err := emitLegacyDocRangeGeneration(ctx, reference, plan, read, func(object DocRangeObject) error {
		objects = append(objects, object)
		return nil
	})
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return legacyRangeGenerationResult(ctx, generation, objects)
}

func emitLegacyDocRangeGeneration(ctx context.Context, reference DocumentRef, plan DocReadPlan, read func(int, int) ([]byte, error), emit func(DocRangeObject) error) (DocRangeGeneration, error) {
	generation, err := prepareLegacyDocRangeGeneration(ctx, reference, plan, read, emit)
	if err != nil {
		return DocRangeGeneration{}, err
	}
	if _, err := generation.Encode(); err != nil {
		return DocRangeGeneration{}, err
	}
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, err
	}
	return generation, nil
}

func prepareLegacyDocRangeGeneration(ctx context.Context, reference DocumentRef, plan DocReadPlan, read func(int, int) ([]byte, error), emit func(DocRangeObject) error) (DocRangeGeneration, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, err
	}
	index := plan.index
	if index.Hash != reference.Hash || reference.Validate() != nil || reference.Identity != DocumentIdentityLegacy ||
		len(plan.bodies) != len(index.Events) || (read == nil && len(index.Events) != 0) {
		return DocRangeGeneration{}, ErrIntegrity
	}
	envelope, err := canonicalJSON(plan.Envelope())
	if err != nil {
		return DocRangeGeneration{}, err
	}
	identity := DocumentIdentityLegacy
	generation := DocRangeGeneration{Version: 3, SourceIdentity: &identity, Algorithm: 1, Hash: reference.Hash,
		Envelope: envelope, EventCount: int64(len(index.Events))}
	pages := legacyRangePages{}
	expectedOffset := 0
	for start := 0; start < len(index.Events); start += DocRangePageEvents {
		for offset, location := range index.Events[start:min(start+DocRangePageEvents, len(index.Events))] {
			if err := ctx.Err(); err != nil {
				return DocRangeGeneration{}, err
			}
			if location.Index != start+offset || location.Offset != expectedOffset || location.Length <= 0 {
				return DocRangeGeneration{}, ErrIntegrity
			}
			raw, err := read(location.Offset, location.Length)
			if err != nil {
				return DocRangeGeneration{}, err
			}
			entry, err := docRangeEntryWithSequence(location.Index, location.Offset, raw, location.Seq, plan.sequencesKnown)
			if failure := ctx.Err(); failure != nil {
				return DocRangeGeneration{}, failure
			}
			if err != nil || entry.Hash != location.Hash || entry.Length != location.Length {
				return DocRangeGeneration{}, ErrIntegrity
			}
			if err := pages.add(ctx, entry, emit); err != nil {
				return DocRangeGeneration{}, err
			}
			expectedOffset = location.Offset + location.Length + 1
		}
	}
	if err := pages.flush(ctx, emit); err != nil {
		return DocRangeGeneration{}, err
	}
	generation.StreamBytes = pages.bytes
	generation.EventPages, err = emitDocRangeDirectory(ctx, pages.hashes, emit)
	if err != nil {
		return DocRangeGeneration{}, err
	}
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, err
	}
	return generation, nil
}

func legacyRangeGenerationResult(ctx context.Context, generation DocRangeGeneration, objects []DocRangeObject) (DocRangeGeneration, []DocRangeObject, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return generation, objects, nil
}
