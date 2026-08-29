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

// ValidateOptionalContentHash is used in derived pointers (memory/settings, etc.) that allow empty values.
func ValidateOptionalContentHash(h ContentHash) error {
	if h == "" {
		return nil
	}
	return ValidateContentHash(h)
}

// ContentHashHex returns only the hex portion of a validated content hash.
func ContentHashHex(h ContentHash) (string, error) {
	if err := ValidateContentHash(h); err != nil {
		return "", err
	}
	return h[len(contentHashPrefix):], nil
}

// ProviderKind indicates the type of coding agent provider that created the session.
// Allowed values: "claude", "codex", "unknown".
type ProviderKind = string

const (
	// ProviderClaude is an Anthropic Claude Code CLI provider.
	ProviderClaude ProviderKind = "claude"
	// ProviderCodex is an OpenAI Codex CLI provider.
	ProviderCodex ProviderKind = "codex"
	// ProviderUnknown is an unidentified provider (compatibility rules).
	// Sentinel value for 3rd-party reconciliation with schema/TS enum, used on detection failure.
	ProviderUnknown ProviderKind = "unknown"
)

// HeadRefName is the fixed name of the head ref.
const HeadRefName = "HEAD"

// ValidateRefKind limits the ref kind of wire/ref path input.
func ValidateRefKind(kind RefKind) error {
	switch kind {
	case RefHEAD, RefBranch, RefSession, RefTag:
		return nil
	default:
		return fmt.Errorf("%w: invalid ref kind %q", ErrInvalidRef, kind)
	}
}

// ValidateRefName validates ref names by kind. Branch/session/tag names allow slash layers but reject path escape and git-invalid names.
func ValidateRefName(kind RefKind, name string) error {
	if err := ValidateRefKind(kind); err != nil {
		return err
	}
	if kind == RefHEAD {
		if name != HeadRefName {
			return fmt.Errorf("%w: head ref name must be %q", ErrInvalidRef, HeadRefName)
		}
		return nil
	}
	if kind == RefTag && strings.HasPrefix(name, BranchLifecycleTagPrefix) {
		_, _, err := parseBranchLifecycleTagName(name)
		return err
	}
	return ValidateBranchName(name)
}

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

// ValidateRef is a full validation of local/remote refs. The CLI RepoID is not a path key, so it is allowed to be empty.
func ValidateRef(ref Ref) error {
	if err := ValidateRefName(ref.Kind, ref.Name); err != nil {
		return err
	}
	if ref.Kind == RefHEAD {
		if err := ValidateOptionalContentHash(ref.Target); err != nil {
			return err
		}
		if ref.Symbolic != "" {
			if ref.Target != "" {
				return fmt.Errorf("%w: HEAD cannot be symbolic and detached at once", ErrInvalidRef)
			}
			return ValidateBranchName(strings.TrimPrefix(ref.Symbolic, "refs/heads/"))
		}
		if ref.Target == "" {
			return fmt.Errorf("%w: HEAD target or symbolic branch required", ErrInvalidRef)
		}
		return nil
	}
	if ref.Target == "" {
		return fmt.Errorf("%w: ref target required", ErrInvalidRef)
	}
	return ValidateContentHash(ref.Target)
}

// HookMessagePrefix is the message prefix of snapshots created by agent hook auto-capture (capture path). It is used for sliding checkpoint determination (replacing the tip of a hook snapshot with a new capture) — a vocabulary for identifying hook origin without adding domain fields (hash invariant).
const HookMessagePrefix = "hook: "

// FidelityTier is the fidelity tier of session load/recovery (domain model). Allowed values: "full", "reconstructed", "memory".
type FidelityTier = string

const (
	// FidelityFull is a lossless recovery from the original provider (including original text re-injection with locked reasoning).
	FidelityFull FidelityTier = "full"
	// FidelityReconstructed is cross-provider restoration. Keep text+toolcall transcript, disable/summarize locked reasoning.
	FidelityReconstructed FidelityTier = "reconstructed"
	// FidelityMemory contains only distilled summary (MemoryDigest). No transcript restoration.
	FidelityMemory FidelityTier = "memory"
)

// RefKind indicates the type of variable pointer (domain model). Allowed values: "head", "branch", "session", "tag".
type RefKind = string

const (
	// RefHEAD is a symbolic ref pointing to "the currently checked-out branch". Exactly one per repo, Name="HEAD".
	RefHEAD RefKind = "head"
	// RefBranch is a branch ref pointing to the tip of the session line.
	RefBranch RefKind = "branch"
	// RefSession is a preservation pointer for the branches of diverging sessions within the same git branch.
	RefSession RefKind = "session"
	// RefTag is an immutable label attached to a specific snapshot.
	RefTag RefKind = "tag"
)
