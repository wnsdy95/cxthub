package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocRangeDirectoryFanout = 128
const DocRangeChunkPageSize = 128
const MaxDocRangeDirectoryBytes = 16 << 10
const MaxDocRangeGenerationBytes = MaxConversationManifestBytes + 2048

type DocRangeDirectoryRoot struct {
	Hash  ContentHash `json:"hash"`
	Count int         `json:"count"`
}

type DocRangeGeneration struct {
	Version        int                        `json:"version"`
	SourceIdentity *DocumentIdentity          `json:"source_identity,omitempty"`
	Algorithm      int                        `json:"algorithm"`
	Hash           ContentHash                `json:"hash"`
	Envelope       json.RawMessage            `json:"envelope,omitempty"`
	EnvelopeRef    *DocRangeEnvelopeReference `json:"envelope_ref,omitempty"`
	EventCount     int64                      `json:"event_count"`
	StreamBytes    int64                      `json:"stream_bytes"`
	EventPages     DocRangeDirectoryRoot      `json:"event_pages"`
	ChunkPages     DocRangeDirectoryRoot      `json:"chunk_pages"`
}

type DocRangeEnvelopeReference struct {
	Hash  ContentHash `json:"hash"`
	Bytes int         `json:"bytes"`
}

type VerifiedDocRangeGeneration struct {
	index      VerifiedDocRangeIndex
	generation DocRangeGeneration
	hash       ContentHash
}

func (verified VerifiedDocRangeGeneration) Descriptor() DocRangeGeneration {
	generation := verified.generation
	generation.Envelope = bytes.Clone(generation.Envelope)
	if generation.EnvelopeRef != nil {
		reference := *generation.EnvelopeRef
		generation.EnvelopeRef = &reference
	}
	if generation.SourceIdentity != nil {
		identity := *generation.SourceIdentity
		generation.SourceIdentity = &identity
	}
	return generation
}

func BuildVerifiedDocRangeGeneration(ctx context.Context, index VerifiedDocRangeIndex) (VerifiedDocRangeGeneration, []DocRangeObject, error) {
	generation, objects, err := BuildDocRangeGeneration(ctx, index)
	if err != nil {
		return VerifiedDocRangeGeneration{}, nil, err
	}
	raw, err := generation.Encode()
	if err != nil {
		return VerifiedDocRangeGeneration{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return VerifiedDocRangeGeneration{}, nil, err
	}
	return VerifiedDocRangeGeneration{index: index, generation: generation, hash: HashContent(raw)}, objects, nil
}

type DocRangeObject struct {
	Hash  ContentHash
	Bytes []byte
}

type docRangeDirectory struct {
	Level  int           `json:"level"`
	Start  int           `json:"start"`
	Hashes []ContentHash `json:"hashes"`
}

func (root DocRangeDirectoryRoot) validate(count int) error {
	if root.Count != count || count < 0 || count > MaxDocRangePages {
		return ErrIntegrity
	}
	if count == 0 {
		if root.Hash != "" {
			return ErrIntegrity
		}
		return nil
	}
	if ValidateContentHash(root.Hash) != nil {
		return ErrIntegrity
	}
	return nil
}

func (generation DocRangeGeneration) ChunkCount() int {
	if generation.Version == 3 {
		return 0
	}
	return int((generation.StreamBytes + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes)
}

func (generation DocRangeGeneration) Identity() DocumentIdentity {
	if generation.SourceIdentity != nil {
		return *generation.SourceIdentity
	}
	return DocumentIdentityRootV1
}

func (generation DocRangeGeneration) Validate(hash ContentHash) error {
	validIdentity := generation.Version == 2 && generation.SourceIdentity == nil ||
		(generation.Version == 3 || generation.Version == 4 || generation.Version == 5 || generation.Version == 6) && generation.SourceIdentity != nil && *generation.SourceIdentity == DocumentIdentityLegacy
	envelopeBytes := len(generation.Envelope)
	if generation.Version == 6 {
		if generation.EnvelopeRef == nil || len(generation.Envelope) != 0 || generation.EnvelopeRef.Bytes <= MaxConversationManifestBytes ||
			generation.EnvelopeRef.Bytes > MaxConversationDocumentBytes-conversationFrameBytes || ValidateContentHash(generation.EnvelopeRef.Hash) != nil {
			return ErrIntegrity
		}
		envelopeBytes = generation.EnvelopeRef.Bytes
	} else if generation.EnvelopeRef != nil || envelopeBytes == 0 || envelopeBytes > MaxConversationManifestBytes {
		return ErrIntegrity
	}
	if !validIdentity || generation.Algorithm != 1 || generation.Hash != hash || ValidateContentHash(hash) != nil ||
		generation.StreamBytes < 0 || generation.StreamBytes > int64(MaxConversationDocumentBytes-conversationFrameBytes-envelopeBytes) ||
		generation.EventCount < 0 || generation.EventCount > generation.StreamBytes || (generation.EventCount == 0) != (generation.StreamBytes == 0) {
		return ErrIntegrity
	}
	if generation.Version != 6 && generation.Identity() == DocumentIdentityLegacy {
		if _, err := decodeCanonicalCIREnvelope(generation.Envelope); err != nil {
			return ErrIntegrity
		}
	} else if generation.Version != 6 {
		if _, err := conversationManifestEnvelope(generation.Envelope); err != nil {
			return ErrIntegrity
		}
	}
	if generation.EventPages.validate(int((generation.EventCount+DocRangePageEvents-1)/DocRangePageEvents)) != nil ||
		generation.ChunkPages.validate((generation.ChunkCount()+DocRangeChunkPageSize-1)/DocRangeChunkPageSize) != nil {
		return ErrIntegrity
	}
	return nil
}

func (generation DocRangeGeneration) Encode() ([]byte, error) {
	if err := generation.Validate(generation.Hash); err != nil {
		return nil, err
	}
	raw, err := canonicalJSON(generation)
	if err != nil || len(raw) > MaxDocRangeGenerationBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func buildDocRangeDirectory(ctx context.Context, hashes []ContentHash, objects *[]DocRangeObject) (DocRangeDirectoryRoot, error) {
	return emitDocRangeDirectory(ctx, hashes, func(object DocRangeObject) error {
		*objects = append(*objects, object)
		return nil
	})
}

func emitDocRangeDirectory(ctx context.Context, hashes []ContentHash, emit func(DocRangeObject) error) (DocRangeDirectoryRoot, error) {
	work, err := newDocRangeDirectoryWork(hashes)
	if err != nil {
		return DocRangeDirectoryRoot{}, err
	}
	for {
		complete, err := work.step(ctx, emit)
		if err != nil {
			return DocRangeDirectoryRoot{}, err
		}
		if complete {
			return work.root, nil
		}
	}
}

func BuildDocRangeGeneration(ctx context.Context, verified VerifiedDocRangeIndex) (DocRangeGeneration, []DocRangeObject, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeGeneration{}, nil, err
	}
	index := verified.index
	if verified.read == nil || index.Validate(index.Hash) != nil {
		return DocRangeGeneration{}, nil, ErrIntegrity
	}
	objects := make([]DocRangeObject, 0)
	events, err := buildDocRangeDirectory(ctx, index.Pages, &objects)
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	chunks, err := buildDocRangeChunkDirectory(ctx, index.Manifest.Chunks, &objects)
	if err != nil {
		return DocRangeGeneration{}, nil, err
	}
	generation := DocRangeGeneration{Version: 2, Algorithm: 1, Hash: index.Hash, Envelope: bytes.Clone(index.Manifest.Envelope), EventCount: index.Manifest.EventCount, StreamBytes: index.Manifest.StreamBytes, EventPages: events, ChunkPages: chunks}
	if _, err := generation.Encode(); err != nil {
		return DocRangeGeneration{}, nil, err
	}
	return generation, objects, ctx.Err()
}

func buildDocRangeChunkDirectory(ctx context.Context, chunks []ConversationManifestChunk, objects *[]DocRangeObject) (DocRangeDirectoryRoot, error) {
	return emitDocRangeChunkDirectory(ctx, chunks, func(object DocRangeObject) error {
		*objects = append(*objects, object)
		return nil
	})
}

func emitDocRangeChunkDirectory(ctx context.Context, chunks []ConversationManifestChunk, emit func(DocRangeObject) error) (DocRangeDirectoryRoot, error) {
	work, err := newDocRangeChunkDirectoryWork(chunks)
	if err != nil {
		return DocRangeDirectoryRoot{}, err
	}
	for {
		complete, err := work.step(ctx, emit)
		if err != nil {
			return DocRangeDirectoryRoot{}, err
		}
		if complete {
			return work.root, nil
		}
	}
}
