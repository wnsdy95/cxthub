package domain

import (
	"strings"
	"time"
)

// MaxGraftSeq is the upper bound of the graft Lamport version that can be represented by the server PostgreSQL BIGINT and the local JSON replica. Wrapping to seq=0 is not allowed.
const MaxGraftSeq uint64 = 1<<63 - 1

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
	// ID is the normalized remote URL or ContentHash of cwd fallback.
	ID string `json:"id"`
	// RemoteURL is the git remote URL of the code repo (empty if not available).
	RemoteURL string `json:"remote_url"`
	// LocalPath is the absolute cwd path on this machine (empty on servers).
	LocalPath string `json:"local_path"`
	// DefaultBranch is the default branch name (e.g., "main").
	DefaultBranch string `json:"default_branch"`
	// WorkspaceID is the workspace bound by the server (for registration response — not stored locally).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// GitRemoteURL is the git origin (e.g., github.com URL) of the code repo — for web "connected" tab link.
	// Always read from .git, separate from RemoteURL(cxt origin, integrity).
	GitRemoteURL string `json:"git_remote_url,omitempty"`
}

// Branch represents the session line (code git branch name, domain model).
type Branch struct {
	// Name is the branch name (e.g., "main", "feat/auth").
	Name string `json:"name"`
	// RepoID is the ID of the containing Repo.
	RepoID string `json:"repo_id"`
	// Head is the latest snapshot ContentHash of this branch (source of truth is Ref(branch).Target).
	Head ContentHash `json:"head"`
}

// Snapshot is the state of one session at a point in time. The body addressed by ID and its natural
// Parents are immutable; out-of-hash projections and overlays change only under their documented merge/CAS rules.
//
// Invariant S-ID/H1: Snapshot.ID == Snapshot.DocHash == ContentHash(canonical_bytes(SessionDoc.CIR)).
type Snapshot struct {
	// ID is the ContentHash of the CIR normalized bytes and is immutable.
	ID ContentHash `json:"id"`
	// RepoID is the ID of the containing repo.
	RepoID string `json:"repo_id"`
	// Branch is the branch name label at creation time.
	Branch string `json:"branch"`
	// Parents is a list of DAG parent snapshot ContentHashes. The root is an empty slice.
	Parents []ContentHash `json:"parents"`
	// DocHash is the ContentHash of the SessionDoc this snapshot points to (== ID).
	DocHash ContentHash `json:"doc_hash"`
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

// ReachabilityParents is the parent list for reachability walk (Parents ∪ GraftParents, duplicates removed).
// Same single rule as server (backend/internal/domain) — all ancestor walks must use this to ensure graft overlay edge reachability consistency across layers.
func (s Snapshot) ReachabilityParents() []ContentHash {
	if len(s.GraftParents) == 0 {
		return s.Parents
	}
	out := append([]ContentHash{}, s.Parents...)
	seen := make(map[ContentHash]bool, len(s.Parents)+len(s.GraftParents))
	for _, p := range s.Parents {
		seen[p] = true
	}
	for _, g := range s.GraftParents {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// Ref is a mutable pointer (HEAD/branch/session/tag unified representation, domain model).
type Ref struct {
	// Kind is the ref type (head|branch|session|tag).
	Kind RefKind `json:"kind"`
	// Name is the ref name (HEAD is fixed as "HEAD"; others are slash-hierarchy names).
	Name string `json:"name"`
	// RepoID is the ID of the containing repo.
	RepoID string `json:"repo_id"`
	// Target is the ContentHash of the Snapshot this ref points to (empty if symbolic HEAD).
	Target ContentHash `json:"target"`
	// Symbolic is the branch name when HEAD indirectly points to a branch.
	Symbolic string `json:"symbolic,omitempty"`
}

// SessionDoc is a CIR container (regular conversation body, immutable, domain model).
//
// Invariant: Hash == ContentHash(canonical_bytes(CIR)).
type SessionDoc struct {
	// Hash is the ContentHash of this SessionDoc.
	Hash ContentHash `json:"hash"`
	// CIR is a provider-independent regular conversation representation (domain model).
	CIR CIRDocument `json:"cir"`
}

// MemoryDigest is the distilled memory (memory-form load, domain model).
type MemoryDigest struct {
	// SnapshotID is the ContentHash of the target snapshot.
	SnapshotID ContentHash `json:"snapshot_id"`
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
}

// MergeDigests inherits memory (prior) into new distillation (fresh) — deterministic.
//
// Memory follows the same logic as raw ancestry: if the snapshot ancestry continues (natural inheritance·append graft irrelevant), memory also continues. Continuous commits in the same session result in deterministic distillation recreating the same items, so dedup absorbs, and new sessions/appends preserve prior items (ancestor precedence).
func MergeDigests(prior, fresh MemoryDigest) MemoryDigest {
	out := fresh
	if prior.Summary != "" && prior.Summary != fresh.Summary && !strings.Contains(fresh.Summary, prior.Summary) {
		if fresh.Summary == "" {
			out.Summary = prior.Summary
		} else {
			out.Summary = prior.Summary + "\n\n" + fresh.Summary
		}
	}
	out.KeyFacts = dedupStrings(prior.KeyFacts, fresh.KeyFacts)
	if fresh.TasksAuthoritative {
		// The latest summary has authority over the unresolved list — ancestor lists can include completed items, thus being discarded.
		// (The summary itself is a compaction snapshot, so it can be stale until the next compaction — data freshness limit acceptance.)
		out.OpenTasks = dedupStrings(fresh.OpenTasks)
	} else {
		out.OpenTasks = dedupStrings(prior.OpenTasks, fresh.OpenTasks)
	}
	return out
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

// NativeMemory is a native memory value object created by the provider CLI (Section compatibility rules).
// MemorySource.ReadNative returns the ingestion source used as the native-first input to MemoryDistiller.Distill.
type NativeMemory struct {
	// Provider is the creator of this native memory (claude|codex).
	Provider ProviderKind `json:"provider"`
	// Source is the origin identifier. Example: "claude:MEMORY.md", "codex:rollout_summary".
	Source string `json:"source"`
	// Text is the native memory body (narrative text).
	Text string `json:"text"`
	// Structured is an optional structured key-value map (optional, can be nil).
	Structured map[string]string `json:"structured,omitempty"`
}

// Manifest is a repo unit metadata index (snapshot/ref list catalog, domain model).
type Manifest struct {
	// RepoID is the ID of the containing repo.
	RepoID string `json:"repo_id"`
	// Refs is a list of all mutable pointers (HEAD/branch/session/tag).
	Refs []Ref `json:"refs"`
	// SnapshotIndex is a list of held snapshot IDs (push/pull negotiation's have set).
	SnapshotIndex []ContentHash `json:"snapshot_index"`
	// Version is an optimistic lock monotonically increasing version.
	Version int `json:"version"`
	// UpdatedAt is the last time this manifest was updated.
	UpdatedAt time.Time `json:"updated_at"`
}

// StashEntry is an item in the stash stack (git stash equivalent).
// Temporarily stores the active session outside the branch history — snapshot objects are stored content-addressed but
// do not point to any branch ref (excluding push targets), and stack order is managed by .cxt/stash.json.
type StashEntry struct {
	// Snapshot is the ContentHash of the stored session snapshot.
	Snapshot ContentHash `json:"snapshot"`
	// Branch is the branch at which the stash was created (git's "WIP on <branch>").
	Branch string `json:"branch"`
	// Message is a human-readable description.
	Message string `json:"message"`
	// Provider is the session provider (claude|codex).
	Provider ProviderKind `json:"provider"`
	// CreatedAt is the creation timestamp.
	CreatedAt time.Time `json:"created_at"`
}

// StashBranchLabel is the branch label of the stash snapshot — it distinguishes from regular branch logs and
// is excluded from push targets (git stash is local-only).
const StashBranchLabel = "(stash)"

// Unsync is a branch-specific mutable pointer for commits that are local-ahead and awaiting push.
// It points to the tip of a commit chain not yet reachable from the shared server branch ref.
// Snapshot and document objects arrive first through an objects-only push; this pointer exposes
// that chain in the On Hold region until a Git push advances the ref and removes the pointer.
// Server key = (auth user, branch) — each team member's unpushed chain appears independently.
type Unsync struct {
	// RepoID is the ID of the associated repo.
	RepoID string `json:"repo_id"`
	// Branch is the target branch name (half of the pointer key).
	Branch string `json:"branch"`
	// Target is the ContentHash of my local branch tip snapshot.
	Target ContentHash `json:"target"`
	// Author is the identifier of the author of this unpushed chain.
	Author TeamIdentity `json:"author"`
	// UpdatedAt is the last update timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

// Pending is a session-specific "in-progress context" mutable pointer (capture path Hook Auto-Capture Anchor).
// Hook capture snapshots update this pointer without moving the branch ref — the branch DAG is maintained with commit snapshots only, and in-progress conversations are rendered as "continuing conversation tails" (tip commits and similar sessions) or "Uncommitted" (orphan sessions) in the UI. The next commit snapshot encapsulates the entire session up to that point, so pending is resolved by deletion upon commit.
type Pending struct {
	// RepoID is the ID of the containing repo.
	RepoID string `json:"repo_id"`
	// SessionID is the original agent session identifier (pointer key).
	SessionID string `json:"session_id"`
	// Branch is the branch at the capture point (for tip matching and UI grouping).
	Branch string `json:"branch"`
	// Provider is the session provider (claude|codex).
	Provider ProviderKind `json:"provider"`
	// Target is the ContentHash of the latest hook capture snapshot (overwrite update).
	Target ContentHash `json:"target"`
	// Author is the identifier of the author of this session.
	Author TeamIdentity `json:"author"`
	// UpdatedAt is the last update timestamp.
	UpdatedAt time.Time `json:"updated_at"`
	// Dismissed is a session display in the uncommitted list that the user has hidden (server authority — no data deletion).
	// Local settings are usually not set; the server preserves them as sticky, so they do not revive on re-push.
	Dismissed bool `json:"dismissed,omitempty"`
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
