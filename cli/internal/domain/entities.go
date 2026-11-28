package domain

import (
	"strings"
	"time"
)

// JSON tags are in snake_case, the same as the backend(cxt-backend) domain and schemas/manifest.schema.json. Although completely separate, they must be compatible over the wire(REST/JSON), so field names are matched (.cxt local storage is in the same format).

// Repo is the root of the session storage space (domain model). One Repo per code repository.
type Repo struct {
	// ID is the normalized remote URL or ContentHash of cwd fallback.
	ID string `json:"id"`
	// RemoteURL is the git remote URL of the code repo (empty if not available).
	RemoteURL string `json:"remote_url"`
	// LocalPath is the shared local context root on this machine (empty on servers).
	// Linked Git worktrees use the primary working tree so one repository cannot
	// split into several .cxt stores.
	LocalPath string `json:"local_path"`
	// DefaultBranch is the default branch name (e.g., "main").
	DefaultBranch string `json:"default_branch"`
	// RepositoryID is the repository bound by the server (for registration response — not stored locally).
	RepositoryID string `json:"repository_id,omitempty"`
	// GitRemoteURL is the git origin (e.g., github.com URL) of the code repo — for web "connected" tab link.
	// Always read from .git, separate from RemoteURL(cxt origin, integrity).
	GitRemoteURL string `json:"git_remote_url,omitempty"`
}

// SessionDoc is a CIR container (regular conversation body, immutable, domain model).
//
// Invariant: Hash addresses CIR under Identity; absent Identity means the
// unchanged legacy ContentHash(canonical_bytes(CIR)) scheme.
type SessionDoc struct {
	// Hash is the ContentHash of this SessionDoc.
	Hash     ContentHash      `json:"hash"`
	Identity DocumentIdentity `json:"identity,omitempty"`
	// CIR is a provider-independent regular conversation representation (domain model).
	CIR CIRDocument `json:"cir"`
}

// MemoryDigest is the distilled memory of a session up to one point (domain model).
type MemoryDigest struct {
	// Summary is the human-readable summary body.
	Summary string `json:"summary"`
	// KeyFacts is a flattened list of core decisions/restrictions/file context.
	KeyFacts []string `json:"key_facts"`
	// OpenTasks is a list of unresolved/next tasks.
	OpenTasks []string `json:"open_tasks"`
	// TasksAuthoritative means OpenTasks is the tool's own structured list,
	// extracted from its latest compaction summary. Only then are the tasks
	// given to a tool as open tasks.
	TasksAuthoritative bool `json:"-"`
}

// IsNativeMemoryProvenanceFact recognizes source labels emitted by older cxt
// distillers. They are transport metadata, not project knowledge, and should
// be excluded only when rendering provider-visible prompts or instruction
// files. Immutable stored digests retain them for compatibility.
func IsNativeMemoryProvenanceFact(fact string) bool {
	fact = strings.ToLower(strings.TrimSpace(fact))
	return strings.HasPrefix(fact, "native memory:") ||
		strings.HasPrefix(fact, "absorbed from") ||
		strings.HasPrefix(fact, "ingested from")
}

const (
	extractiveFallbackHeader           = "Conversation digest (extractive fallback; provider compaction summary unavailable):"
	extractiveFallbackUsersHeader      = "Recent user intent:"
	extractiveFallbackAssistantsHeader = "Recent assistant outcomes:"
	extractiveFallbackDeltaMarker      = "[cxt conversation delta v1]"
)

// AppendExtractiveConversationDelta combines a provider/native baseline with
// the deterministic extractive fallback using a versioned private marker.
// The marker prevents provider-authored prose that merely resembles the
// fallback wire format from being removed as a cxt-owned delta later.
func AppendExtractiveConversationDelta(baseline, delta string) string {
	if delta == "" {
		return baseline
	}
	if baseline == "" {
		return delta
	}
	return baseline + "\n\n" + extractiveFallbackDeltaMarker + "\n" + delta
}

const extractiveConversationItemMaxRunes = 2000

// NormalizeExtractiveConversationItem is the canonical representation stored
// in deterministic fallback digests. Replay projection uses the same function
// so even long raw messages can be removed by exact, not fuzzy, comparison.
func NormalizeExtractiveConversationItem(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= extractiveConversationItemMaxRunes {
		return text
	}
	return string(runes[:extractiveConversationItemMaxRunes]) + "…"
}

// RenderExtractiveFallbackSummary is the canonical text representation of the
// deterministic conversation fallback. Keeping the format in the domain lets
// prompt projection recognize its header exactly.
func RenderExtractiveFallbackSummary(users, assistants []string) string {
	var b strings.Builder
	b.WriteString(extractiveFallbackHeader)
	// Preserve the established storage wire format: the header already carried
	// one newline before writeExtractiveSection added two more.
	b.WriteByte('\n')
	writeExtractiveFallbackSection(&b, extractiveFallbackUsersHeader, users)
	writeExtractiveFallbackSection(&b, extractiveFallbackAssistantsHeader, assistants)
	return strings.TrimSpace(b.String())
}

func writeExtractiveFallbackSection(b *strings.Builder, header string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("\n\n")
	b.WriteString(header)
	b.WriteByte('\n')
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
	}
}

// NativeMemoryScope describes whether provider-owned memory follows a working
// tree or one provider session. The distinction controls prompt projection:
// only WorkingTree memory is guaranteed to be loaded again in a newly
// materialized session.
type NativeMemoryScope string

const (
	NativeMemoryScopeWorkingTree NativeMemoryScope = "working_tree"
	NativeMemoryScopeSession     NativeMemoryScope = "session"
)

// NativeMemory is a native memory value object created by the provider CLI (Section compatibility rules).
// MemorySource.ReadNative returns the ingestion source used as the native-first input to MemoryDistiller.Distill.
type NativeMemory struct {
	// Provider is the creator of this native memory (claude|codex).
	Provider ProviderKind `json:"provider"`
	// Source is the origin identifier. Example: "claude:MEMORY.md", "codex:rollout_summary".
	Source string `json:"source"`
	// Scope states whether the provider auto-loads this value for every new
	// session in the working tree or keeps it bound to the source session.
	Scope NativeMemoryScope `json:"scope,omitempty"`
	// AutoLoadedPrefix is a conservative, complete-line prefix guaranteed to be
	// loaded in a new session. It may be shorter than Text when the provider has
	// a startup memory limit. Empty means no bytes may be removed from prompts.
	AutoLoadedPrefix string `json:"auto_loaded_prefix,omitempty"`
	// Text is the native memory body (narrative text).
	Text string `json:"text"`
	// Structured is an optional structured key-value map (optional, can be nil).
	Structured map[string]string `json:"structured,omitempty"`
}

// SettingsFile / SettingsBundle — server team default settings bundle
// (.claude/.agents/.codex) mirror type.
// cxt settings pull updates the local folder with the received settings.
type SettingsFile struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

type SettingsBundle struct {
	Kind      string         `json:"kind"`
	Files     []SettingsFile `json:"files"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by,omitempty"`
}
