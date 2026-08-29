// Package domain contains the cxt server's domain types.
//
// Under the complete-separation rule (the module boundary), the backend shares no Go code
// with the CLI. Wire/JSON shapes derive from schemas/cir.schema.json, schemas/manifest.schema.json,
// the data model, and the OpenAPI contract.
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
// (the data model, schemas/manifest.schema.json $defs/contentHash).
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

// ContentHashHex returns only the hex part of the validated content hash.
func ContentHashHex(h ContentHash) (string, error) {
	if err := ValidateContentHash(h); err != nil {
		return "", err
	}
	return string(h)[len(contentHashPrefix):], nil
}

// ProviderKind is the type of the original/target provider (schemas/cir.schema.json $defs/providerKind).
//
// The backend does not interpret provider raw formats but stores and projects the provider type as is in the CIR envelope and Snapshot metadata.
type ProviderKind string

const (
	// ProviderClaude identifies a Claude Code session source.
	ProviderClaude ProviderKind = "claude"
	// ProviderCodex identifies a Codex CLI session source.
	ProviderCodex ProviderKind = "codex"
	// ProviderUnknown represents an unknown provider (the third enum value required by compatibility rules).
	ProviderUnknown ProviderKind = "unknown"
)

// FidelityTier is the restoration-fidelity tier (schemas/cir.schema.json $defs/fidelityTier).
//
//	full          = lossless original, including same-provider reinjection
//	reconstructed = cross-provider reconstruction with reasoning disabled or summarized
//	memory        = distilled summary only; transcript is not restored
//
// The backend does not derive this tier (loading belongs to the CLI); it stores the frozen value in Snapshot metadata.
type FidelityTier string

const (
	// FidelityFull is a lossless original.
	FidelityFull FidelityTier = "full"
	// FidelityReconstructed is a cross-provider reconstruction.
	FidelityReconstructed FidelityTier = "reconstructed"
	// FidelityMemory contains only a distilled summary.
	FidelityMemory FidelityTier = "memory"
)

// RefKind identifies a ref type (schemas/manifest.schema.json $defs/refKind).
//
//	head    = current-checkout symbolic ref (one per repo, name fixed at "HEAD" — invariant REF3)
//	branch = context-tip pointer for a real Git branch
//	session = preservation pointer for a session fork within the same Git branch (internal join use)
//	tag     = immutable label (REF2: target never changes after creation)
type RefKind string

const (
	// RefHead is symbolic HEAD.
	RefHead RefKind = "head"
	// RefBranch is a branch tip.
	RefBranch RefKind = "branch"
	// RefSession is a session-fork tip within one Git branch.
	RefSession RefKind = "session"
	// RefTag is an immutable tag.
	RefTag RefKind = "tag"
)

// HeadRefName is the fixed head-ref name (invariant REF3, data model).
const HeadRefName = "HEAD"

// SessionRefPrefix builds the Git-branch scope for an internal ref that preserves the session fork
// left by a partial join. Encoding the branch's byte length as its own path component prevents a prefix
// comparison from treating "feature" and "feature/foo" as the same scope.
//
// Example ref name: fork/v1/4/main/<short-tip>
func SessionRefPrefix(branch string) string {
	return fmt.Sprintf("fork/v1/%d/%s/", len(branch), branch)
}

// ValidateRefKind restricts the ref kind accepted at wire and ref-path boundaries.
func ValidateRefKind(kind RefKind) error {
	switch kind {
	case RefHead, RefBranch, RefSession, RefTag:
		return nil
	default:
		return fmt.Errorf("%w: invalid ref kind %q", ErrValidation, kind)
	}
}

// ValidateRefName validates a ref name by kind. Branch, session, and tag names may contain slash
// hierarchies, but path traversal and Git-invalid names are rejected.
func ValidateRefName(kind RefKind, name string) error {
	if err := ValidateRefKind(kind); err != nil {
		return err
	}
	if kind == RefHead {
		if name != HeadRefName {
			return fmt.Errorf("%w: head ref name must be %q", ErrValidation, HeadRefName)
		}
		return nil
	}
	if kind == RefTag && strings.HasPrefix(name, BranchLifecycleTagPrefix) {
		_, _, err := parseBranchLifecycleTagName(name)
		return err
	}
	return ValidateBranchName(name)
}

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

// ValidateRef performs complete ref validation at backend store and service boundaries.
func ValidateRef(ref Ref) error {
	if err := ValidateContentHash(ref.RepoID); err != nil {
		return err
	}
	if err := ValidateRefName(ref.Kind, ref.Name); err != nil {
		return err
	}
	if ref.Kind == RefHead {
		if err := ValidateOptionalContentHash(ref.Target); err != nil {
			return err
		}
		if ref.Symbolic != "" {
			if ref.Target != "" {
				return fmt.Errorf("%w: HEAD cannot be symbolic and detached at once", ErrIntegrity)
			}
			return ValidateBranchName(strings.TrimPrefix(ref.Symbolic, "refs/heads/"))
		}
		if ref.Target == "" {
			return fmt.Errorf("%w: HEAD target or symbolic branch required", ErrIntegrity)
		}
		return nil
	}
	if ref.Target == "" {
		return fmt.Errorf("%w: ref target required", ErrIntegrity)
	}
	return ValidateContentHash(ref.Target)
}

// StashBranchLabel is the Branch label for stash snapshots (the same value used by the CLI). Stashes are
// normally local-only, but content-hash deduplication can make a stash object share an ID with commit history;
// ref-based reachability can then include it in a push. The label is used for statistics exclusion and promotion decisions.
const StashBranchLabel = "(stash)"

// HookMessagePrefix is the Message prefix for hook-capture snapshots (the same value used by the CLI).
const HookMessagePrefix = "hook: "

// TeamIdentity identifies a snapshot author (data model table, sync protocol).
//
// Unlike Git's separate author and committer, cxt records a single Author (OQ-7).
// It is for attribution and auditing, not access control (sync protocol).
type TeamIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Team  string `json:"team"`
}
