package domain

import (
	"bytes"
	"context"
	"fmt"
)

// ConversationManifestForCIR explicitly builds a staged root and owned chunk
// bodies from in-memory CIR. It neither publishes nor assigns any identity.
// Existing canonical normalization applies, including stable event ordering.
// It materializes canonical bytes before checking their size; use bounded
// external loading plus VerifyConversationManifest at untrusted boundaries.
func ConversationManifestForCIR(doc CIRDocument) (ConversationManifest, map[ContentHash][]byte, error) {
	envelope, err := canonicalJSON(doc.Envelope)
	if err != nil || len(envelope) > MaxConversationManifestBytes {
		return ConversationManifest{}, nil, conversationManifestError("CIR envelope encoding or limit")
	}
	if _, err := conversationManifestEnvelope(envelope); err != nil {
		return ConversationManifest{}, nil, err
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		return ConversationManifest{}, nil, fmt.Errorf("%w: CIR canonicalization: %w", ErrConversationManifest, err)
	}
	if len(canonical) > MaxConversationDocumentBytes {
		return ConversationManifest{}, nil, conversationManifestError("CIR document limit")
	}
	prefix := append([]byte(conversationDocPrefix), envelope...)
	prefix = append(prefix, conversationDocMiddle...)
	if !bytes.HasPrefix(canonical, prefix) || !bytes.HasSuffix(canonical, []byte(conversationDocSuffix)) {
		return ConversationManifest{}, nil, conversationManifestError("CIR canonical framing")
	}
	stream := canonical[len(prefix) : len(canonical)-len(conversationDocSuffix)]
	chunks := []ConversationManifestChunk{}
	bodies := make(map[ContentHash][]byte)
	for offset := 0; offset < len(stream); offset += ConversationManifestChunkBytes {
		body := bytes.Clone(stream[offset:min(offset+ConversationManifestChunkBytes, len(stream))])
		hash := HashContent(body)
		chunks = append(chunks, ConversationManifestChunk{Hash: hash, Bytes: int64(len(body))})
		bodies[hash] = body
	}
	manifest, err := NewConversationManifest(envelope, chunks, int64(len(doc.Events)))
	if err != nil {
		return ConversationManifest{}, nil, err
	}
	root, err := ConversationManifestHash(manifest)
	if err != nil {
		return ConversationManifest{}, nil, err
	}
	err = VerifyConversationManifest(context.Background(), root, manifest, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return bodies[hash], nil
	})
	if err != nil {
		return ConversationManifest{}, nil, err
	}
	return manifest, bodies, nil
}
