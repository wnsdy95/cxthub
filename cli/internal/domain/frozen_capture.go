package domain

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const (
	// FrozenCaptureVersion also pins replay normalization semantics. Bump this
	// version when codec/masking/regexp semantics change: the policy fingerprint
	// binds options and secrets, not executable normalization behavior.
	FrozenCaptureVersion          = 1
	FrozenCaptureChunkBytes int64 = 1 << 20
	FrozenCaptureMaxBytes   int64 = 2 << 30
	// FrozenCaptureMemoryMaxBytes bounds the JSON-encoded private native memory.
	FrozenCaptureMemoryMaxBytes = 8 << 20
)

var ErrFrozenCaptureInput = errors.New("invalid frozen capture input")

// FrozenCaptureInput describes private native bytes, NOT a public CAS object or
// a projection proof. The caller durably journals this descriptor after Freeze.
// Every projection must re-read and verify every chunk and the complete hash.
// Raw chunks remain private evidence; there is deliberately no automatic GC.
type FrozenCaptureInput struct {
	Version           int                  `json:"version"`
	Provider          ProviderKind         `json:"provider"`
	SourcePath        string               `json:"source_path"`
	Size              int64                `json:"size"`
	Hash              ContentHash          `json:"hash"`
	Chunks            []FrozenCaptureChunk `json:"chunks"`
	CapturedAt        time.Time            `json:"captured_at"`
	PolicyFingerprint string               `json:"policy_fingerprint"`
	DocIdentity       DocumentIdentity     `json:"doc_identity,omitempty"`
	SessionID         string               `json:"session_id,omitempty"`
	// NativeMemory is raw private receipt evidence, never a public CAS object.
	// Nil records explicit absence; replay must not discover provider memory.
	NativeMemory *NativeMemory `json:"native_memory"`
}

type FrozenCaptureChunk struct {
	Hash ContentHash `json:"hash"`
	Size int64       `json:"size"`
}

func (in FrozenCaptureInput) Validate() error {
	if in.Version != FrozenCaptureVersion || (in.Provider != ProviderClaude && in.Provider != ProviderCodex) ||
		!filepath.IsAbs(in.SourcePath) || filepath.Clean(in.SourcePath) != in.SourcePath || strings.ContainsRune(in.SourcePath, 0) ||
		in.Size <= 0 || in.Size > FrozenCaptureMaxBytes || in.CapturedAt.IsZero() ||
		ValidateContentHash(in.Hash) != nil || in.DocIdentity.Validate() != nil ||
		len(in.PolicyFingerprint) != 64 || strings.Trim(in.PolicyFingerprint, "0123456789abcdef") != "" {
		return ErrFrozenCaptureInput
	}
	if int64(len(in.Chunks)) != (in.Size+FrozenCaptureChunkBytes-1)/FrozenCaptureChunkBytes {
		return ErrFrozenCaptureInput
	}
	remaining := in.Size
	for _, chunk := range in.Chunks {
		want := min(remaining, FrozenCaptureChunkBytes)
		if chunk.Size != want || ValidateContentHash(chunk.Hash) != nil {
			return ErrFrozenCaptureInput
		}
		remaining -= chunk.Size
	}
	if in.NativeMemory != nil {
		if in.NativeMemory.Provider != in.Provider {
			return ErrFrozenCaptureInput
		}
		// Reject oversized raw values before JSON encoding can allocate for them.
		remaining := FrozenCaptureMemoryMaxBytes
		consume := func(value string) bool {
			if len(value) > remaining {
				return false
			}
			remaining -= len(value)
			return true
		}
		m := in.NativeMemory
		for _, value := range []string{m.Provider, m.Source, string(m.Scope), m.Text, m.AutoLoadedPrefix} {
			if !consume(value) {
				return ErrFrozenCaptureInput
			}
		}
		for key, value := range m.Structured {
			if !consume(key) || !consume(value) {
				return ErrFrozenCaptureInput
			}
		}
		encoded, err := json.Marshal(m)
		if err != nil || len(encoded) > FrozenCaptureMemoryMaxBytes {
			return ErrFrozenCaptureInput
		}
	}
	return nil
}
