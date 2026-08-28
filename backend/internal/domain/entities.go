package domain

import "time"

// MaxGraftSeq is the upper bound of the graft Lamport version that PostgreSQL BIGINT and FS/CLI JSON replicas can share. Wrapping around at the upper bound could cause removed edges to revive as legacy seq=0, so mutations are fail-closed.
const MaxGraftSeq uint64 = 1<<63 - 1

const MemoryProjectionVersion uint32 = 1

// Repo is the root of the session storage space (data model, sync protocol).
//
// One per detected code repository. Namespace root for all branch/snapshot/ref names.
//
// Invariants:
//   - R1: ID remains stable throughout the repo's lifetime (remote promotion is only via migration).
//   - R2: DefaultBranch is the Name of an existing branch ref (created or designated at first save if none exists).
//
// ID Derivation (R-ID): Hash of normalized remote URL (if present) or cwd absolute path (fallback).
// The server trusts the ID as a multi-tenant isolation key (output is CLI responsibility).
// LocalPath is always an empty string on the server (local-only field, sync protocol).
type Repo struct {
	ID            ContentHash `json:"id"`
	RemoteURL     string      `json:"remote_url"`
	LocalPath     string      `json:"local_path"`
	DefaultBranch string      `json:"default_branch"`
	// WorkspaceID is the containing workspace (visibility boundary). During push it is
	// derived from /<owner_username>/<workspace-slug>/… in RemoteURL. "" means unowned (legacy).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// GitRemoteURL is the code repository's Git origin (for example, GitHub), used by the web "Connected" tab.
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	// About fields correspond to GitHub's repository About panel and are editable on the web.
	Description string   `json:"description,omitempty"`
	Website     string   `json:"website,omitempty"`
	Topics      []string `json:"topics,omitempty"`
	// ProtectDefault rejects --force ref moves on the default branch. History (P1) is already
	// immutable; this independent policy protects only the mutable pointer.
	ProtectDefault bool `json:"protect_default,omitempty"`
}

// SettingsFile is a single file for team default settings bundle (path is relative to bundle root).
type SettingsFile struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

// SettingsBundle is the team default agent settings for the repo
// (.claude/.agents/.codex folder).
// Upload folder (claude|agents|codex) via web; team members receive it via cxt
// and overwrite the matching local directory.
// Kind ∈ {claude, agents, codex}.
type SettingsBundle struct {
	Kind      string         `json:"kind"`
	Files     []SettingsFile `json:"files"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by,omitempty"`
}

// Unsync is a branch-specific mutable pointer for "push pending (local ahead)" state,
// representing the commit side of the On Hold view.
// Key = (auth user, branch) — each team member's unpushed chain is independently displayed.
// The snapshot arrives first through a shadow push; a Git push that advances the ref resolves the pointer by deletion.
type Unsync struct {
	RepoID    ContentHash  `json:"repo_id"`
	User      string       `json:"user"` // Authenticated user username (server injected — pointer key)
	Branch    string       `json:"branch"`
	Target    ContentHash  `json:"target"`
	Author    TeamIdentity `json:"author"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Pending is a session-specific "in-progress context" mutable pointer (hook capture landing point mirror).
// It points to the latest hook capture snapshot without moving branch refs, which the web renders as
// a "continuing conversation tail" (same session as the tip commit) or "Uncommitted" (detached session).
// When a commit snapshot absorbs the session, the CLI resolves it with DELETE.
type Pending struct {
	RepoID    ContentHash  `json:"repo_id"`
	SessionID string       `json:"session_id"`
	Branch    string       `json:"branch"`
	Provider  ProviderKind `json:"provider"`
	Target    ContentHash  `json:"target"`
	Author    TeamIdentity `json:"author"`
	UpdatedAt time.Time    `json:"updated_at"`
	// Dismissed means the user hid this durable capture from the uncommitted list.
	// It does not delete data: snapshot, document, and session history remain intact. The flag is sticky;
	// PutPending preserves it so SyncPendings cannot make the entry visible again. Only the UI list excludes it.
	Dismissed bool `json:"dismissed,omitempty"`
}

// PendingDeleteResult is the storage-level outcome of an expected-target
// compare-and-delete. Absent is a successful idempotent resolution, while Kept
// proves that a concurrent newer capture exists and must not be removed.
type PendingDeleteResult uint8

const (
	PendingDeleteKept PendingDeleteResult = iota
	PendingDeleteDeleted
	PendingDeleteAbsent
)

func (r PendingDeleteResult) Resolved() bool {
	return r == PendingDeleteDeleted || r == PendingDeleteAbsent
}

// Snapshot is the session state at one point in time (a commit). The body identified by ID and its natural
// Parents are immutable and content-addressed. Out-of-hash projections and overlays such as Branches,
// MemoryHash, GraftParents, and GraftSeq are updated under their own merge/CAS rules (data model).
//
// Core Invariant (S-ID/H1):
//
//	ID == DocHash == ContentHash(canonical_bytes(SessionDoc.CIR))
//
// ID is therefore the hash of the CIR body's canonical bytes. Commit metadata such as Message, Author,
// CreatedAt, and Parents does not participate in the ID (same body → same ID → automatic deduplication).
// The server recomputes and verifies this invariant when it receives a push (sync protocol).
//
// Invariants:
//   - S1: ID, DocHash, and natural Parents never change after storage. Out-of-hash projections are updated
//     only through each field's merge/CAS rules.
//   - S2: DocHash must point to a SessionDoc that exists in the store (write order W1).
//   - S3: All hashes in Parents must exist within the same RepoID (or root is an empty array).
//   - S4: Parents links form an acyclic DAG.
//   - S5: Branch is only a birth label; refs define ownership.
//
// MemoryHash is a pointer to the MemoryDigest attached to the raw doc (compatibility rules, optional, "" possible).
// Snapshots can hold both raw (DocHash) and memory (MemoryHash) together.
type Snapshot struct {
	ID     ContentHash `json:"id"`
	RepoID ContentHash `json:"repo_id"`
	Branch string      `json:"branch"`
	// Branches is Git branch membership projected from branch reflogs. Branch is the legacy birth-label field;
	// content deduplication allows multiple branches to share one snapshot.
	Branches   []string      `json:"branches,omitempty"`
	Parents    []ContentHash `json:"parents"`
	DocHash    ContentHash   `json:"doc_hash"`
	MemoryHash ContentHash   `json:"memory_hash,omitempty"`
	// Content-addressed hash of the .claude/.agents/.codex folder snapshots at the commit point.
	ClaudeSettings ContentHash  `json:"claude_settings,omitempty"`
	AgentsSettings ContentHash  `json:"agents_settings,omitempty"`
	CodexSettings  ContentHash  `json:"codex_settings,omitempty"`
	Provider       ProviderKind `json:"provider"`
	Fidelity       FidelityTier `json:"fidelity"`
	Message        string       `json:"message"`
	Author         TeamIdentity `json:"author"`
	CreatedAt      time.Time    `json:"created_at"`
	// Grafted indicates that this snapshot has a graft (diverged-append) overlay edge. The server records it
	// for the UI's join marker; it does not participate in S-ID.
	Grafted bool `json:"grafted,omitempty"`
	// GraftParents are reachability-overlay parents. Natural Parents never change; reachability is
	// Parents ∪ GraftParents. This field is outside S-ID. The server projection is authoritative, and the CLI
	// converges after optimistically applying an expected-seq event.
	GraftParents []ContentHash `json:"graft_parents,omitempty"`
	// GraftSeq versions the (GraftParents, GraftSeq) register outside S-ID. A graft is a replaceable overlay,
	// not an append-only set: join reordering must be able to supersede an earlier auto-graft, because adding
	// the reverse edge would create a cycle. For replica convergence, the higher seq replaces the complete set;
	// only two legacy seq=0 sets may be unioned. At equal seq>0, the server projection wins and reconciliation
	// advances the register with a Lamport-style max+1.
	GraftSeq uint64 `json:"graft_seq,omitempty"`
	// SessionID is the source agent session identifier promoted from CIREnvelope.SessionOriginID. It lets the
	// UI render same-branch session boundaries without opening document blobs. It is outside S-ID.
	SessionID string `json:"session_id,omitempty"`
	// Models, promoted from Envelope.SourceModels, lists every model seen in the session so the UI can render
	// participant icons without fetching the document. It is outside S-ID.
	Models []string `json:"models,omitempty"`
	// CompactionCount, promoted from Envelope.CompactionCount, is the number of context compactions in the session.
	// Compaction does not change sessionId, so the UI draws ◈ on snapshots whose count exceeds their parent's.
	// It is outside S-ID and avoids a document fetch.
	CompactionCount int `json:"compaction_count,omitempty"`
}

// ReachabilityParents is the list of parent nodes in the reachability walk (Parents ∪ GraftParents, duplicates removed).
// A single rule for ancestor/reachability determination — the engine's walk, Fsck audit, and GC guard must all use this.
// This central rule prevents the earlier drift where hand-written walkers omitted overlay edges from dangling audits.
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

// Branch is a read-only view of a ref (kind=branch) in a domain-friendly manner
// (data model, sync protocol).
//
// The source of truth is Ref(branch).Target (the save/sync unit is a ref), and Branch.Head must always
// match Ref(branch, Name).Target (invariant B1, duplicates are OQ-2).
type Branch struct {
	Name   string      `json:"name"`
	RepoID ContentHash `json:"repo_id"`
	Head   ContentHash `json:"head"`
}

// Ref is a mutable name pointing to a snapshot (data model, schemas/manifest.schema.json $defs/ref).
//
// Kind unifies head/branch/session/tag representations.
//   - head:   Name="HEAD" (fixed), Symbolic=current branch name (e.g., "main"), Target is usually empty.
//     detached HEAD is Symbolic="" + Target=<snap>.
//   - branch: Name=branch name, Target=latest snapshot of that branch.
//   - session: Name=the session branch name preserved by join, Target=tip (independent of HEAD/Git branches).
//   - tag:    Name=tag name, Target=immutable snapshot (REF2).
//
// Invariants:
//   - REF1: branch/session/tag/detached HEAD's Target is an existing Snapshot.ID. Symbolic HEAD is an existing branch.
//   - REF2: tag is immutable once created (force re-tagging is delete+create).
//   - REF3: there is exactly one head ref per repo, Name="HEAD".
//   - REF4: branch tip advancement is only allowed via fast-forward or explicit (fork/conflict handling). Arbitrary rewind is forbidden.
type Ref struct {
	Kind     RefKind     `json:"kind"`
	Name     string      `json:"name"`
	RepoID   ContentHash `json:"repo_id"`
	Target   ContentHash `json:"target"`
	Symbolic string      `json:"symbolic,omitempty"`
}

// RefLogEntry is an append-only log entry for a single ref movement (git reflog equivalent).
// It provides a safety net to recover tips that were obscured by ref movements, and it does not record symbolic HEAD movements.
type RefLogEntry struct {
	Kind      RefKind     `json:"kind"`
	Name      string      `json:"name"`
	Old       ContentHash `json:"old"`
	New       ContentHash `json:"new"`
	CreatedAt time.Time   `json:"created_at"`
}

// Manifest is the repository-level metadata index (data model, schemas/manifest.schema.json).
//
// It serves two roles: (a) push/pull negotiation by computing missing snapshots and
// (b) storage of the optimistic-lock version.
//
// Invariants:
//   - M1: All targets in refs must be included in SnapshotIndex (dangling ref forbidden).
//   - M2: Version must monotonically increase with each update ( CAS).
//   - C1: Manifest write must fail without version-CAS (lost-update prevention).
type Manifest struct {
	RepoID            ContentHash                 `json:"repo_id"`
	Refs              []Ref                       `json:"refs"`
	SnapshotIndex     []ContentHash               `json:"snapshot_index"`
	MemoryAttachments map[ContentHash]ContentHash `json:"memory_attachments"`
	SnapshotStates    map[ContentHash]ContentHash `json:"snapshot_states,omitempty"`
	Version           int                         `json:"version"`
	UpdatedAt         time.Time                   `json:"updated_at"`
}

// MemoryDigest is the distilled memory from a snapshot (derivative, data model table, sync protocol).
//
// Used in memory-form load (FidelityMemory). Attached to SnapshotID, and the target snapshot must exist before the digest upload (422 if not, sync protocol).
//
// The backend stores only CIR-neutral digests and does not know provider-native formats.
type MemoryDigest struct {
	SnapshotID         ContentHash          `json:"snapshot_id"`
	PreviousMemoryHash ContentHash          `json:"previous_memory_hash,omitempty"`
	Summary            string               `json:"summary"`
	KeyFacts           []string             `json:"key_facts"`
	OpenTasks          []string             `json:"open_tasks"`
	Provider           ProviderKind         `json:"provider"`
	Fragments          []MemoryFragment     `json:"fragments,omitempty"`
	GraftCoverage      *MemoryGraftCoverage `json:"graft_coverage,omitempty"`
}

// MemoryGraftCoverage is the root graft register and transitive lineage state
// observed when a client projected and attached a digest. ProjectionComplete
// distinguishes a trusted fingerprint from retained partial data. Nil means
// legacy/unknown.
type MemoryGraftCoverage struct {
	ProjectionVersion  uint32        `json:"projection_version"`
	ProjectionComplete bool          `json:"projection_complete"`
	LineageFingerprint ContentHash   `json:"lineage_fingerprint,omitempty"`
	GraftSeq           uint64        `json:"graft_seq"`
	GraftParents       []ContentHash `json:"graft_parents,omitempty"`
	PinnedSources      []ContentHash `json:"pinned_sources,omitempty"`
}

// MemoryFragment is one snapshot-scoped memory contribution. The backend
// treats it as CIR-neutral content and preserves it for deterministic client
// projection across natural and graft parents.
type MemoryFragment struct {
	SourceSnapshot     ContentHash `json:"source_snapshot"`
	Summary            string      `json:"summary,omitempty"`
	KeyFacts           []string    `json:"key_facts,omitempty"`
	OpenTasks          []string    `json:"open_tasks,omitempty"`
	TasksAuthoritative bool        `json:"tasks_authoritative,omitempty"`
}
