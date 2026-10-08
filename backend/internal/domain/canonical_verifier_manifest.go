package domain

import (
	"bytes"
	"context"
)

// This private entry state is created only after strict root metadata and the
// expected domain-separated root have been validated. Its slices are owned.
type verifiedManifestIdentity struct {
	manifest  ConversationManifest
	canonical string
}

// VerifyConversationManifestDoc verifies every current chunk occurrence and
// returns an immutable, scheme-bearing document over those same owned bytes.
// It shares the legacy verifier's exact event semantics and bounded semantic
// cache, but never compares a manifest root to a fabricated whole-CIR digest.
// Success grants no repository ownership, retention or permission to publish.
//
// The loader must honor ctx and may reuse its buffer on its next call, but must
// not concurrently mutate a returned body. Fixed root partitions, declared
// lengths and top-level count are mandatory, including a zero-chunk empty root.
func (v *CanonicalDocVerifier) VerifyConversationManifestDoc(ctx context.Context, want ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error)) (VerifiedSessionDoc, error) {
	var zero VerifiedSessionDoc
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if len(manifest.Envelope) > MaxConversationManifestBytes || len(manifest.Chunks) > MaxConversationManifestChunks {
		return zero, ErrConversationManifest
	}
	manifest.Envelope = bytes.Clone(manifest.Envelope)
	if manifest.Chunks != nil {
		manifest.Chunks = append([]ConversationManifestChunk{}, manifest.Chunks...)
	}
	raw, err := CanonicalConversationManifest(manifest)
	if err != nil {
		return zero, err
	}
	if ValidateContentHash(want) != nil || HashContent(append([]byte(conversationManifestDomain), raw...)) != want {
		return zero, ErrIntegrity
	}
	// Only this private core view omits root lengths/counts. The authenticated
	// root state travels with it; it is never exported as a legacy descriptor.
	stream := DocChunkManifest{Format: ChunkFormatV2, Envelope: manifest.Envelope, Chunks: make([]ContentHash, len(manifest.Chunks))}
	for i, chunk := range manifest.Chunks {
		stream.Chunks[i] = chunk.Hash
	}
	root := &verifiedManifestIdentity{manifest: manifest, canonical: string(raw)}
	return v.verifyChunkDocument(ctx, want, stream, load, MaxConversationDocumentBytes, root)
}
