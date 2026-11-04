package domain

import (
	"context"
	"slices"
)

type MemoryReadChunkVerifier struct {
	refs []MemoryReadChunkRef
}

func NewMemoryReadChunkVerifier(ctx context.Context, repo, identifier ContentHash, header MemoryReadHeader) (*MemoryReadChunkVerifier, error) {
	rawHeader, err := MemoryReadHeaderBytes(ctx, header)
	if err != nil {
		return nil, err
	}
	if HashContent(rawHeader) != identifier || header.Source.Graph.RepoID != repo {
		return nil, ErrIntegrity
	}
	refs := slices.Clone(header.Chunks)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &MemoryReadChunkVerifier{refs: refs}, nil
}

func (verifier *MemoryReadChunkVerifier) Decode(ctx context.Context, ordinal int, raw []byte) ([]EffectiveMemoryItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifier == nil || ordinal < 0 || ordinal >= len(verifier.refs) {
		return nil, ErrIntegrity
	}
	return memoryReadDecodeChunk(ctx, verifier.refs[ordinal], raw)
}
