package domain

import "time"

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
	RequiredDocIdentity DocumentIdentity `json:"required_doc_identity,omitempty"`
	ContextProtocol     int              `json:"context_protocol,omitempty"`
	ID                  ContentHash      `json:"id"`
	RemoteURL           string           `json:"remote_url"`
	LocalPath           string           `json:"local_path"`
	DefaultBranch       string           `json:"default_branch"`
	// RepositoryID is the containing repository (visibility boundary). During push it is
	// derived from /<owner_username>/<repository-slug>/… in RemoteURL. "" means unowned (legacy).
	RepositoryID string `json:"repository_id,omitempty"`
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
	Branches    []string         `json:"branches,omitempty"`
	Parents     []ContentHash    `json:"parents"`
	DocHash     ContentHash      `json:"doc_hash"`
	DocIdentity DocumentIdentity `json:"doc_identity,omitempty"`
	MemoryHash  ContentHash      `json:"memory_hash,omitempty"`
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
