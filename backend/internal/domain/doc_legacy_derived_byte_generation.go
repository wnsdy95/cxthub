package domain

import "context"

func BuildLegacyDerivedDocByteRangeGeneration(ctx context.Context, proof VerifiedSessionDoc) (DocRangeGeneration, []DocRangeObject, error) {
	objects := make([]DocRangeObject, 0)
	generation, _, err := emitLegacyDerivedDocByteRangeGeneration(ctx, proof, func(object DocRangeObject) error {
		objects = append(objects, object)
		return nil
	})
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return legacyRangeGenerationResult(ctx, generation, objects)
}

func emitLegacyDerivedDocByteRangeGeneration(ctx context.Context, proof VerifiedSessionDoc, emit func(DocRangeObject) error) (DocRangeGeneration, ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, "", err
	}
	if !proof.Valid() || proof.DocumentRef().Identity != DocumentIdentityLegacy {
		return DocRangeGeneration{}, "", ErrUnsupportedDocumentIdentity
	}
	readPlan, err := proof.PlanReadIndexContext(ctx)
	if err != nil {
		return DocRangeGeneration{}, "", err
	}
	plan, err := NewLegacyByteBackfill(ctx, proof)
	if err != nil {
		return DocRangeGeneration{}, "", err
	}
	generation, err := prepareLegacyDocRangeGeneration(ctx, proof.DocumentRef(), readPlan, func(offset, length int) ([]byte, error) {
		return proof.EventStreamRange(ctx, offset, length)
	}, emit)
	if err != nil {
		return DocRangeGeneration{}, "", err
	}
	spec, err := plan.Spec()
	if err != nil {
		return DocRangeGeneration{}, "", err
	}
	if spec.Reference != proof.DocumentRef() || generation.Hash != spec.Reference.Hash || generation.StreamBytes != spec.StreamBytes {
		return DocRangeGeneration{}, "", ErrIntegrity
	}
	chunks := make([]ConversationManifestChunk, 0, spec.Blocks)
	for ordinal := 0; ordinal < spec.Blocks; ordinal++ {
		block, err := plan.Block(ctx, ordinal)
		if err != nil {
			return DocRangeGeneration{}, "", err
		}
		chunks = append(chunks, block.Descriptor())
	}
	generation.Version = 5
	if len(generation.Envelope) > MaxConversationManifestBytes {
		hash, err := HashContentContext(ctx, generation.Envelope)
		if err != nil {
			return DocRangeGeneration{}, "", err
		}
		generation.Version = 6
		generation.EnvelopeRef = &DocRangeEnvelopeReference{Hash: hash, Bytes: len(generation.Envelope)}
		generation.Envelope = nil
	}
	generation.ChunkPages, err = emitDocRangeChunkDirectory(ctx, chunks, emit)
	if err != nil {
		return DocRangeGeneration{}, "", err
	}
	if _, err := generation.Encode(); err != nil {
		return DocRangeGeneration{}, "", err
	}
	key, err := plan.Key()
	if err != nil {
		return DocRangeGeneration{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, "", err
	}
	return generation, key, nil
}
