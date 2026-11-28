// Package domain contains the cxt server's domain types.
//
// Under the complete-separation rule (the module boundary), the backend shares no Go code
// with the CLI. Wire/JSON shapes derive from schemas/cir.schema.json, the data model, and the
// OpenAPI contract (schemas/openapi.yaml).
//
// The backend does not understand provider-native Claude/Codex formats. It accepts only provider-neutral CIR.
//
// This file defines value objects and enums.
package domain

import (
	"fmt"
	"strings"
)

// ContentHash is a value object representing a content address hash.
//
// Format: "sha256:<hex>" (lowercase hex 64 characters). Pattern ^sha256:[0-9a-f]{64}$
// (the data model, schemas/openapi.yaml components/schemas/ContentHash).
//
// Determinism requires identical logical content to produce identical hex. Canonical serialization
// (canonical_bytes) before hashing provides that property (data model, RFC 8785 JCS-oriented).
type ContentHash string

const contentHashPrefix = "sha256:"

// ValidateContentHash strictly validates an externally supplied content-address key. Store-path use fails closed
// unless the value matches the documented format.
func ValidateContentHash(h ContentHash) error {
	s := string(h)
	if len(s) != len(contentHashPrefix)+64 || !strings.HasPrefix(s, contentHashPrefix) {
		return fmt.Errorf("%w: invalid content hash %q", ErrIntegrity, s)
	}
	for _, r := range s[len(contentHashPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: invalid content hash %q", ErrIntegrity, s)
		}
	}
	return nil
}

// ValidateOptionalContentHash is used in derivative pointers (memory/settings, etc.) that allow empty values.
func ValidateOptionalContentHash(h ContentHash) error {
	if h == "" {
		return nil
	}
	return ValidateContentHash(h)
}

// ProviderKind is the type of the original/target provider (schemas/cir.schema.json $defs/providerKind).
//
// The backend does not interpret provider raw formats but keeps the provider type as is in the CIR envelope.
type ProviderKind string

const (
	// ProviderClaude identifies a Claude Code session source.
	ProviderClaude ProviderKind = "claude"
	// ProviderCodex identifies a Codex CLI session source.
	ProviderCodex ProviderKind = "codex"
)

// FidelityTier is the restoration-fidelity tier (schemas/cir.schema.json $defs/fidelityTier).
//
//	full          = lossless original, including same-provider reinjection
//	reconstructed = cross-provider reconstruction with reasoning disabled or summarized
//	memory        = distilled summary only; transcript is not restored
//
// The backend does not derive this tier (loading belongs to the CLI); it keeps the value the document carries.
type FidelityTier string

const (
	// FidelityFull is a lossless original.
	FidelityFull FidelityTier = "full"
)

// ValidateBranchName permits only safe ref paths for branch, session, and tag names.
func ValidateBranchName(name string) error {
	if name == "" || len(name) > 1024 {
		return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
	}
	if strings.Contains(name, "\\") || strings.Contains(name, "..") || strings.Contains(name, "@{") || name == "@" || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
		}
	}
	for _, r := range name {
		switch {
		case r <= ' ', r == 0x7f:
			return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
		case strings.ContainsRune("~^:?*[", r):
			return fmt.Errorf("%w: invalid ref name %q", ErrValidation, name)
		}
	}
	return nil
}
