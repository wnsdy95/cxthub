package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// This additive primitive is not a negotiated document identity or storage
// format. Legacy CIR hashes and chunk readers remain unchanged. Mirrored in the
// independent CLI module; see docs/CONVERSATION_IDENTITY.md for the staged contract.
const (
	ConversationManifestVersion     = 1
	ConversationManifestIdentity    = "cxt-manifest-sha256-v1"
	ConversationManifestChunkFormat = "cxt-doc-chunks-v2"
	ConversationManifestChunkBytes  = 512 << 10
	MaxConversationManifestBytes    = 256 << 10
	MaxConversationManifestChunks   = 2048
	MaxConversationDocumentBytes    = 512 << 20

	conversationManifestDomain = "cxt-conversation-root-v1\x00"
	conversationDocPrefix      = `{"envelope":`
	conversationDocMiddle      = `,"events":[`
	conversationDocSuffix      = `]}`
	conversationFrameBytes     = len(conversationDocPrefix) + len(conversationDocMiddle) + len(conversationDocSuffix)
)

var ErrConversationManifest = errors.New("invalid conversation manifest")

type ConversationManifestChunk struct {
	Hash  ContentHash `json:"hash"`
	Bytes int64       `json:"bytes"`
}

type ConversationManifest struct {
	Version     int                         `json:"version"`
	Identity    string                      `json:"identity"`
	ChunkFormat string                      `json:"chunk_format"`
	Envelope    json.RawMessage             `json:"envelope"`
	Chunks      []ConversationManifestChunk `json:"chunks"`
	StreamBytes int64                       `json:"stream_bytes"`
	EventCount  int64                       `json:"event_count"`
}

// NewConversationManifest owns its metadata. The derived sum and caller's
// event count remain claims until VerifyConversationManifest reads the bodies.
func NewConversationManifest(envelope json.RawMessage, chunks []ConversationManifestChunk, eventCount int64) (ConversationManifest, error) {
	if len(envelope) > MaxConversationManifestBytes || len(chunks) > MaxConversationManifestChunks {
		return ConversationManifest{}, conversationManifestError("metadata limit")
	}
	m := ConversationManifest{
		Version: ConversationManifestVersion, Identity: ConversationManifestIdentity,
		ChunkFormat: ConversationManifestChunkFormat, Envelope: bytes.Clone(envelope),
		Chunks: make([]ConversationManifestChunk, len(chunks)), EventCount: eventCount,
	}
	copy(m.Chunks, chunks)
	for _, chunk := range m.Chunks {
		if chunk.Bytes <= 0 || chunk.Bytes > MaxConversationDocumentBytes-m.StreamBytes {
			return ConversationManifest{}, conversationManifestError("chunk length or stream limit")
		}
		m.StreamBytes += chunk.Bytes
	}
	if err := m.Validate(); err != nil {
		return ConversationManifest{}, err
	}
	return m, nil
}

// Validate checks bounded metadata only, never bytes, CIR event semantics,
// storage ownership, dependency retention or permission to publish a root.
func (m ConversationManifest) Validate() error {
	_, err := CanonicalConversationManifest(m)
	return err
}

// CanonicalConversationManifest emits the sole accepted v1 wire encoding.
// Envelope must already have exact typed CIR canonical encoding. All object
// keys are sorted; chunk occurrences and every complete value are preserved.
func CanonicalConversationManifest(m ConversationManifest) ([]byte, error) {
	if m.Version != ConversationManifestVersion || m.Identity != ConversationManifestIdentity ||
		m.ChunkFormat != ConversationManifestChunkFormat || m.Chunks == nil ||
		len(m.Chunks) > MaxConversationManifestChunks || len(m.Envelope) == 0 ||
		len(m.Envelope) > MaxConversationManifestBytes || !utf8.Valid(m.Envelope) {
		return nil, conversationManifestError("version, identity, format or metadata shape")
	}
	if _, err := conversationManifestEnvelope(m.Envelope); err != nil {
		return nil, conversationManifestError("noncanonical CIR envelope")
	}
	available := int64(MaxConversationDocumentBytes - conversationFrameBytes - len(m.Envelope))
	if m.StreamBytes < 0 || m.StreamBytes > available || m.EventCount < 0 || m.EventCount > m.StreamBytes {
		return nil, conversationManifestError("document size or event count")
	}
	var total int64
	for i, chunk := range m.Chunks {
		if ValidateContentHash(chunk.Hash) != nil || chunk.Bytes <= 0 || chunk.Bytes > ConversationManifestChunkBytes ||
			(i < len(m.Chunks)-1 && chunk.Bytes != ConversationManifestChunkBytes) || chunk.Bytes > available-total {
			return nil, conversationManifestError("chunk hash, partition or size")
		}
		total += chunk.Bytes
	}
	if total != m.StreamBytes || (total == 0) != (m.EventCount == 0) {
		return nil, conversationManifestError("stream length or empty count")
	}
	raw, err := canonicalJSON(m)
	if err != nil || len(raw) > MaxConversationManifestBytes {
		return nil, conversationManifestError("encoded manifest limit")
	}
	return raw, nil
}

// ConversationManifestHash hashes metadata under an explicit node separator.
// It does not verify any chunk or authorize substituting this root for a legacy
// canonical CIR hash, even when both use the ContentHash string type.
func ConversationManifestHash(m ConversationManifest) (ContentHash, error) {
	raw, err := CanonicalConversationManifest(m)
	if err != nil {
		return "", err
	}
	return HashContent(append([]byte(conversationManifestDomain), raw...)), nil
}

// DecodeConversationManifest accepts canonical-only wire bytes. Exact typed
// re-encoding rejects duplicates, unknown/case-aliased/missing/null fields,
// alternate escapes, reordered keys, whitespace and trailing JSON. It is not a
// shape-sniffing replacement for any legacy decoder.
func DecodeConversationManifest(raw []byte) (ConversationManifest, error) {
	if len(raw) == 0 || len(raw) > MaxConversationManifestBytes || !utf8.Valid(raw) {
		return ConversationManifest{}, conversationManifestError("wire limit or UTF-8")
	}
	var m ConversationManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ConversationManifest{}, conversationManifestError("wire JSON")
	}
	canonical, err := CanonicalConversationManifest(m)
	if err != nil {
		return ConversationManifest{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return ConversationManifest{}, conversationManifestError("noncanonical manifest")
	}
	return m, nil
}

func conversationManifestError(reason string) error {
	return fmt.Errorf("%w: %s", ErrConversationManifest, reason)
}
