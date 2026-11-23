package domain

import (
	"strings"
	"time"
)

// JSON tags are in snake_case, the same as the backend(cxt-backend) domain and schemas/manifest.schema.json. Although completely separate, they must be compatible over the wire(REST/JSON), so field names are matched (.cxt local storage is in the same format).

// TeamIdentity is a value object that identifies the snapshot author (domain model).
type TeamIdentity struct {
	// Name is the human-readable author name.
	Name string `json:"name"`
	// Email is the author's email, used as a unique identifier within the team.
	Email string `json:"email"`
	// Team is the team label (matches the visibility boundary of the central server).
	Team string `json:"team"`
}

// Repo is the root of the session storage space (domain model). One Repo per code repository.
type Repo struct {
	RequiredDocIdentity DocumentIdentity `json:"required_doc_identity,omitempty"`
	ContextProtocol     int              `json:"context_protocol,omitempty"`
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

// Snapshot is the state of one session at a point in time. The body addressed by ID and its natural
// Parents are immutable; out-of-hash projections and overlays change only under their documented merge/CAS rules.
//
// Invariant S-ID/H1: Snapshot.ID == Snapshot.DocHash. DocIdentity selects the
// document hash scheme; its absence retains the canonical-CIR identity.
type Snapshot struct {
	// ID is the immutable document identity selected by DocIdentity.
	ID ContentHash `json:"id"`
	// RepoID is the ID of the containing repo.
	RepoID string `json:"repo_id"`
	// Branch is the branch name label at creation time.
	Branch string `json:"branch"`
	// Parents is a list of DAG parent snapshot ContentHashes. The root is an empty slice.
	Parents []ContentHash `json:"parents"`
	// DocHash is the ContentHash of the SessionDoc this snapshot points to (== ID).
	DocHash ContentHash `json:"doc_hash"`
	// DocIdentity is omitted for legacy canonical-CIR hashes.
	DocIdentity DocumentIdentity `json:"doc_identity,omitempty"`
	// MemoryHash is the ContentHash of the MemoryDigest attached to this snapshot (optional, can be "").
	MemoryHash ContentHash `json:"memory_hash,omitempty"`
	// ClaudeSettings/AgentsSettings/CodexSettings is the commit-time .claude/.agents/.codex folder snapshot (content-addressed SettingsBundle object) hash. Pushes and pulls with the commit.
	ClaudeSettings ContentHash `json:"claude_settings,omitempty"`
	AgentsSettings ContentHash `json:"agents_settings,omitempty"`
	CodexSettings  ContentHash `json:"codex_settings,omitempty"`
	// Provider is the original capture provider (claude|codex).
	Provider ProviderKind `json:"provider"`
	// Fidelity is the fidelity tier of this snapshot.
	Fidelity FidelityTier `json:"fidelity"`
	// Message is a human-readable snapshot description (commit message meaning).
	Message string `json:"message"`
	// Author is the identifier of the snapshot author.
	Author TeamIdentity `json:"author"`
	// CreatedAt is the snapshot creation timestamp.
	CreatedAt time.Time `json:"created_at"`
	// Grafted indicates that this snapshot has a graft (diverged append) overlay edge attached
	// (server records, pull propagated — ID calculation excluded derivative metadata).
	Grafted bool `json:"grafted,omitempty"`
	// GraftParents are the reachability overlay parents during diverged append (Parents invariant).
	// Local Save applies optimistically with the ordered graft queue and commits to the server via CAS
	// (ID calculation excluded).
	GraftParents []ContentHash `json:"graft_parents,omitempty"`
	// GraftSeq is the version in the (GraftParents, GraftSeq) version register (hash-free metadata).
	// Graft is a replaceable overlay: it should supersede auto-graft (additive alone would create circular recombination on the same branch merge).
	// Replication convergence: high seq replaces the entire set, and seq=0 allows legacy union.
	// seq>0 version conflicts are adjusted to the server projection as the source of truth — Lamport-style max+1 increment.
	GraftSeq uint64 `json:"graft_seq,omitempty"`
	// SessionID is the original agent session identifier (CIREnvelope.SessionOriginID promotion —
	// UI does not need to open doc to draw session boundaries for list/graph metadata).
	SessionID string `json:"session_id,omitempty"`
	// Models is the list of models that appeared in this snapshot session (Envelope.SourceModels promotion —
	// list metadata for drawing overlapping icons for participating AI without doc fetch).
	Models []string `json:"models,omitempty"`
	// CompactionCount is the context compaction count for this snapshot session (Envelope.CompactionCount promotion).
	// Compaction does not change sessionId, so SessionID alone does not show it — the graph draws compression markers (◈) on snapshots with larger CompactionCount than parent (for drawing without doc fetch).
	CompactionCount int `json:"compaction_count,omitempty"`
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

// MemoryDigest is the distilled memory (memory-form load, domain model).
type MemoryDigest struct {
	// SnapshotID is the ContentHash of the target snapshot.
	SnapshotID ContentHash `json:"snapshot_id"`
	// PreviousMemoryHash is the causal parent of this attachment for the same
	// snapshot. Empty means a legacy/initial root. It turns the mutable
	// Snapshot.MemoryHash pointer into a fast-forward-only content-addressed ref.
	PreviousMemoryHash ContentHash `json:"previous_memory_hash,omitempty"`
	// Summary is a human-readable summary body for CLAUDE.md/AGENTS.md injection.
	Summary string `json:"summary"`
	// KeyFacts is a flattened list of core decisions/restrictions/file context.
	KeyFacts []string `json:"key_facts"`
	// OpenTasks is a list of unresolved/next tasks.
	OpenTasks []string `json:"open_tasks"`
	// TasksAuthoritative means OpenTasks is the latest compressed summary structure extraction.
	// (Inheritance signal — storage/wiring excluded). true means merge from fresh is authoritative: does not inherit from ancestor OpenTasks (blocks permanent accumulation of completed tasks — real review P1).
	TasksAuthoritative bool `json:"-"`
	// Provider is the injection target provider format hint.
	Provider ProviderKind `json:"provider"`
	// Fragments retain provenance for branch-wide memory projection. Summary,
	// KeyFacts, and OpenTasks remain the rendered compatibility view. Older
	// digests without fragments are promoted as one immutable legacy fragment.
	Fragments []MemoryFragment `json:"fragments,omitempty"`
	// GraftCoverage records the exact mutable graft register observed when this
	// digest was attached. Natural parents are immutable and need no coverage
	// marker, but a nil value is legacy/unknown and cannot prove that the
	// corrected transitive frontier algorithm covered grafts hidden below them.
	GraftCoverage *MemoryGraftCoverage `json:"graft_coverage,omitempty"`
	ClaimsVersion uint32               `json:"claims_version,omitempty"`
}

// MemoryGraftCoverage describes which transitive lineage a MemoryDigest tried
// to project. ProjectionComplete=true makes the Merkle fingerprint a trusted
// proof; false retains pinned/partial data but can never stop traversal. The
// fingerprint covers natural/graft topology, every reachable graft register,
// and ancestor memory pointers. The root register remains inline for a cheap
// rejection before recomputing it.
type MemoryGraftCoverage struct {
	ProjectionVersion  uint32        `json:"projection_version"`
	ProjectionComplete bool          `json:"projection_complete"`
	LineageFingerprint ContentHash   `json:"lineage_fingerprint,omitempty"`
	GraftSeq           uint64        `json:"graft_seq"`
	GraftParents       []ContentHash `json:"graft_parents,omitempty"`
	PinnedSources      []ContentHash `json:"pinned_sources,omitempty"`
}

// MemoryFragment is one snapshot's independently distilled contribution.
// SourceSnapshot plus fragment content is the stable dedup key across natural
// and graft lineages.
type MemoryFragment struct {
	SourceSnapshot     ContentHash   `json:"source_snapshot"`
	Summary            string        `json:"summary,omitempty"`
	KeyFacts           []string      `json:"key_facts,omitempty"`
	OpenTasks          []string      `json:"open_tasks,omitempty"`
	TasksAuthoritative bool          `json:"tasks_authoritative,omitempty"`
	Claims             []MemoryClaim `json:"claims,omitempty"`
}

func renderMemoryFragments(out *MemoryDigest) {
	var summaries []string
	fallbackInsertAt := -1
	var fallbackUsers, fallbackAssistants []string
	var facts, tasks []string
	for _, fragment := range out.Fragments {
		if summary := strings.TrimSpace(fragment.Summary); summary != "" {
			baseline, users, assistants, hasFallback := splitExtractiveFallbackSummary(summary)
			if baseline != "" && !containsString(summaries, baseline) {
				summaries = append(summaries, baseline)
			}
			if hasFallback {
				// Keep the combined projection at the position of the newest
				// fallback fragment. renderSeedDigest retains the newest tail, so
				// placing it at the first fallback could evict newer unique items.
				fallbackInsertAt = len(summaries)
				fallbackUsers = dedupStrings(fallbackUsers, users)
				fallbackAssistants = dedupStrings(fallbackAssistants, assistants)
			} else if baseline == "" && !containsString(summaries, summary) {
				summaries = append(summaries, summary)
			}
		}
		facts = dedupStrings(facts, fragment.KeyFacts)
		if fragment.TasksAuthoritative {
			tasks = dedupStrings(fragment.OpenTasks)
		} else {
			tasks = dedupStrings(tasks, fragment.OpenTasks)
		}
	}
	if fallbackInsertAt >= 0 {
		summaries = append(summaries, "")
		copy(summaries[fallbackInsertAt+1:], summaries[fallbackInsertAt:])
		summaries[fallbackInsertAt] = RenderExtractiveFallbackSummary(fallbackUsers, fallbackAssistants)
	}
	out.Summary = strings.Join(summaries, "\n\n")
	out.KeyFacts = facts
	out.OpenTasks = tasks
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
// provenance rendering recognize and combine exact items without fuzzy or
// model-dependent comparison.
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

// parseExtractiveFallbackSummary accepts only the exact deterministic format
// emitted by RenderExtractiveFallbackSummary. Any unexpected prose or section
// order is classified as an unstructured/agent-written summary and remains
// byte-for-byte on the legacy rendering path.
func parseExtractiveFallbackSummary(summary string) (users, assistants []string, ok bool) {
	lines := strings.Split(strings.TrimSpace(summary), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != extractiveFallbackHeader {
		return nil, nil, false
	}
	section := 0
	seenUsers, seenAssistants := false, false
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch line {
		case extractiveFallbackUsersHeader:
			if seenUsers || seenAssistants {
				return nil, nil, false
			}
			seenUsers, section = true, 1
		case extractiveFallbackAssistantsHeader:
			if seenAssistants {
				return nil, nil, false
			}
			seenAssistants, section = true, 2
		default:
			if section == 0 || !strings.HasPrefix(line, "- ") {
				return nil, nil, false
			}
			item := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			if item == "" {
				return nil, nil, false
			}
			if section == 1 {
				users = append(users, item)
			} else {
				assistants = append(assistants, item)
			}
		}
	}
	if len(users) == 0 && len(assistants) == 0 {
		return nil, nil, false
	}
	return users, assistants, true
}

// splitExtractiveFallbackSummary recognizes both a standalone deterministic
// fallback and the canonical "provider/native baseline + fallback" shape.
// Strict parsing keeps arbitrary provider-authored prose byte-for-byte.
func splitExtractiveFallbackSummary(summary string) (baseline string, users, assistants []string, ok bool) {
	summary = strings.TrimSpace(summary)
	if users, assistants, ok = parseExtractiveFallbackSummary(summary); ok {
		return "", users, assistants, true
	}
	marker := "\n\n" + extractiveFallbackDeltaMarker + "\n"
	if at := strings.LastIndex(summary, marker); at >= 0 {
		candidate := summary[at+len(marker):]
		if users, assistants, ok = parseExtractiveFallbackSummary(candidate); ok {
			return strings.TrimSpace(summary[:at]), users, assistants, true
		}
	}
	return summary, nil, nil, false
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// dedupStrings merges multiple lists while preserving order (prior list first) and removing duplicates.
func dedupStrings(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
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
