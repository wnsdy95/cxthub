package domain

import (
	"fmt"
	"strings"
)

// ContentHash is the basic unit for content address resolution.
// Format: "sha256:<lowercase-hex-64chars>". Example: "sha256:9f1c...".
// Same ContentHash ⇒ Same content (dedup key, invariant).
type ContentHash = string

const contentHashPrefix = "sha256:"

// ValidateContentHash strictly validates an external/remote content-address key.
func ValidateContentHash(h ContentHash) error {
	if len(h) != len(contentHashPrefix)+64 || !strings.HasPrefix(h, contentHashPrefix) {
		return fmt.Errorf("%w: invalid content hash %q", ErrHashMismatch, h)
	}
	for _, r := range h[len(contentHashPrefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: invalid content hash %q", ErrHashMismatch, h)
		}
	}
	return nil
}

// ProviderKind indicates the type of coding agent provider that created the session.
// Allowed values: "claude", "codex", "unknown".
type ProviderKind = string

const (
	// ProviderClaude is an Anthropic Claude Code CLI provider.
	ProviderClaude ProviderKind = "claude"
	// ProviderCodex is an OpenAI Codex CLI provider.
	ProviderCodex ProviderKind = "codex"
)

// ValidateBranchName allows only safe ref paths for branch/session/tag names.
func ValidateBranchName(name string) error {
	if name == "" || len(name) > 1024 {
		return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
	}
	if strings.Contains(name, "\\") || strings.Contains(name, "..") || strings.Contains(name, "@{") || name == "@" || strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
		}
	}
	for _, r := range name {
		switch {
		case r <= ' ', r == 0x7f:
			return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
		case strings.ContainsRune("~^:?*[", r):
			return fmt.Errorf("%w: invalid ref name %q", ErrInvalidRef, name)
		}
	}
	return nil
}

// FidelityTier is the fidelity tier of session load/recovery (domain model). Allowed values: "full", "reconstructed", "memory".
type FidelityTier = string

const (
	// FidelityFull is a lossless recovery from the original provider (including original text re-injection with locked reasoning).
	FidelityFull FidelityTier = "full"
)
