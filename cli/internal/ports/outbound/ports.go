package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// SessionStore is a content-addressed snapshot/document/ref persistence port (domain model).
//
// Content addressing is key. Same CIR bytes ⇒ same DocHash ⇒ automatic dedup.
// Implementation (adapters/storage) merges blob (fs) and index/ref (sqlite) to mimic a git object DB.
//
// Write order invariant (W1): commit doc → snapshot meta → ref/manifest to prevent dangling.
type SessionStore interface {
	// PutDoc stores the SessionDoc body by content address and returns its ContentHash.
	// If the same hash exists, it's a no-op (idempotent dedup). Returned hash == SessionDoc.Hash.
	PutDoc(ctx context.Context, doc domain.SessionDoc) (domain.ContentHash, error)

	// GetDoc retrieves the SessionDoc body by hash.
	// Returns domain.ErrNotFound if not found.
	GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error)

	// HasDoc quickly determines if a doc exists (body not loaded — pull delta negotiation).
	HasDoc(ctx context.Context, hash domain.ContentHash) (bool, error)

	// PutSnapshot stores an immutable Snapshot meta (parent/DocHash/author, etc.).
	// If it exists, it validates integrity (Snapshot.ID == doc.Hash) and does nothing.
	PutSnapshot(ctx context.Context, snap domain.Snapshot) error

	// GetSnapshot retrieves the Snapshot meta by ID (=ContentHash).
	// Returns domain.ErrNotFound if not found.
	GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error)

	// CompareAndSwapSnapshotMemory advances the local memory attachment only
	// when its current pointer still equals expected. Memory blobs are immutable;
	// this CAS protects the mutable pointer across concurrent CLI processes.
	CompareAndSwapSnapshotMemory(ctx context.Context, id, expected, next domain.ContentHash) error

	// ReconcileGraftState adopts a snapshot graft register from an authoritative
	// server read. It replaces only the graft register, leaving immutable
	// metadata (ID/Parents) unchanged.
	ReconcileGraftState(ctx context.Context, authoritative domain.Snapshot) error

	// ListSnapshots returns a list of snapshots (history) for a specific repo/branch.
	// Returns all repo snapshots if branch is empty.
	ListSnapshots(ctx context.Context, repoID string, branch string) ([]domain.Snapshot, error)

	// PutRef upserts a mutable pointer (HEAD/branch/session/tag).
	// Used for branch HEAD movement and tag creation.
	PutRef(ctx context.Context, ref domain.Ref) error

	// GetRef retrieves a ref by (repoID, kind, name).
	// Example: (repo, RefBranch, "main"). Returns domain.ErrNotFound if not found.
	GetRef(ctx context.Context, repoID string, kind domain.RefKind, name string) (domain.Ref, error)

	// ListRefs lists all refs (HEAD/branches/tags) of a repo.
	ListRefs(ctx context.Context, repoID string) ([]domain.Ref, error)

	// Configuration folder snapshot object (content-addressed) — attached to commits and pushed/pulled.
	PutSettingsObject(ctx context.Context, bundle domain.SettingsBundle) (domain.ContentHash, error)
	GetSettingsObject(ctx context.Context, hash domain.ContentHash) (domain.SettingsBundle, error)

	// Stash stack (git stash equivalent, local-only — latest at the front).
	StashPush(ctx context.Context, repoID string, e domain.StashEntry) error
	// StashPop removes and returns the latest item. Returns ErrNotFound if empty.
	StashPop(ctx context.Context, repoID string) (domain.StashEntry, error)
	StashList(ctx context.Context, repoID string) ([]domain.StashEntry, error)

	// Progress context pointer (session-based upsert — hook auto-capture endpoint, branch ref immutable).
	PutPending(ctx context.Context, p domain.Pending) error
	ListPendings(ctx context.Context, repoID string) ([]domain.Pending, error)
	// DeletePending removes a session's pending (no error if not present — idempotent).
	DeletePending(ctx context.Context, repoID, sessionID string) error

	// DeleteSnapshot/DeleteDoc remove objects idempotently.
	// Their only purpose is garbage collection of hook-capture leaves replaced by a later pending capture or commit;
	// commit history objects are never deleted (caller holds ref, hook prefix guard).
	DeleteSnapshot(ctx context.Context, id domain.ContentHash) error
	DeleteDoc(ctx context.Context, hash domain.ContentHash) error

	// Manifest returns a repo unit catalog (snapshot index + ref list) for push/pull negotiation.
	Manifest(ctx context.Context, repoID string) (domain.Manifest, error)

	// PutMemory stores a MemoryDigest as a content-addressed blob and returns its ContentHash.
	// If the same hash already exists, it's a no-op (idempotent dedup). The Snapshot.MemoryHash points to this.
	// (compatibility rules)
	PutMemory(ctx context.Context, digest domain.MemoryDigest) (domain.ContentHash, error)

	// GetMemory retrieves MemoryDigest by hash.
	// If not found, returns domain.ErrNotFound. (compatibility rules)
	GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error)
}

// RemoteSnapshotStateCursorStore persists optional pull negotiation hints.
// It is deliberately separate from SessionStore: cursors never alter local
// snapshot truth, refs, reachability, or push negotiation.
type RemoteSnapshotStateCursorStore interface {
	LoadRemoteSnapshotStateCursor(ctx context.Context, repoID string) (map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry, error)
	SaveRemoteSnapshotStateCursor(ctx context.Context, repoID string, entries map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry) error
}

// ProviderCodec is a codec for provider raw JSONL ↔ CIR bidirectional transformation (domain model).
//
// Decode: claude/codex JSONL → CIR (provider-independent).
// Encode: CIR → target provider format.
// Tool name mapping (claude Bash/Edit ↔ codex shell/apply_patch …) is performed here.
type ProviderCodec interface {
	// Provider returns the original provider types this codec can decode (claude|codex).
	Provider() domain.ProviderKind

	// Decode converts raw session bytes (JSONL) to CIRDocument.
	// Fills envelope metadata and time-ordered events.
	Decode(ctx context.Context, raw []byte) (domain.CIRDocument, error)

	// Encode converts CIR to target provider session file bytes.
	// If target == cir.Envelope.SourceProvider, it's full fidelity (locked original re-injection),
	// otherwise, it's reconstructed (locked reasoning disabled, redacted_summary fallback).
	Encode(ctx context.Context, cir domain.CIRDocument, target domain.ProviderKind) ([]byte, error)
}

// CaptureSource finds and reads the active session file based on the current working directory (domain model).
//
// Claude: ~/.claude/projects/<cwd-encoded>/<sessionId>.jsonl with the latest mtime.
// Codex : ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl with matching session_meta.cwd and latest mtime.
type CaptureSource interface {
	// Provider returns the provider this capture adapter is responsible for.
	Provider() domain.ProviderKind

	// LocateActiveSession finds the path to the most recent active session file matching the current working directory.
	// Returns domain.ErrNoActiveSession if no matching session is found.
	LocateActiveSession(ctx context.Context, cwd string) (path string, err error)

	// ReadSession reads the raw bytes (JSONL) from the given session file path.
	ReadSession(ctx context.Context, path string) ([]byte, error)

	// SessionFilePath calculates the target session file path for loading.
	// Applies provider directory rules to return a new session file path.
	SessionFilePath(ctx context.Context, cwd string, provider domain.ProviderKind) (string, error)
}

// GitContext is a port for querying the current code repo / branch (domain model).
//
// Claude can augment with gitBranch, but Codex is the unique branch source.
type GitContext interface {
	// CurrentRepo identifies the code repo containing the current working directory.
	// Produces a normalized remote URL or cwd fallback key as Repo.ID.
	CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error)

	// CurrentBranch returns the current git branch name of the cwd.
	// Returns an empty value or fallback if not in a repo.
	CurrentBranch(ctx context.Context, cwd string) (string, error)
}

// MergedPullRequest identifies a Git-host PR whose merge commit entered the
// currently checked-out base branch.
type MergedPullRequest struct {
	Number         int
	BaseBranch     string
	HeadBranch     string
	MergeCommitSHA string
}

// PullRequestMergeResolver maps incoming Git commits back to merged pull
// requests. It is provider-specific discovery only; context promotion remains
// in the SyncRepo use case.
type PullRequestMergeResolver interface {
	ResolveMergedPullRequests(ctx context.Context, gitRemoteURL, baseBranch string, commitSHAs []string) ([]MergedPullRequest, error)
}

// RemoteSync is the sync port to the central server (domain model).
//
// Transfers only missing parts (push/pull) based on content-hash. Mimics git push/pull.
// Push/Pull internally performs the 3-step negotiation/objects/ref of sync protocol
type RemoteSync interface {
	// Push uploads local (snapshot, ref) to the central server. Can call object-only/ref-only steps by passing nil objects or refs. Normal SyncRepo push splits into two steps to ensure object -> queued graft -> ref order, avoiding hash duplication on the server. Sends both raw doc (Snapshot.DocHash) and memory (Snapshot.MemoryHash). (compatibility rules: Snapshots hold and propagate raw+memory). Force moves non-fast-forward refs to the server (git push --force). AppendDiverged appends diverged pushes to the server head (cxt push --append — grafts head onto incoming lineage; no history loss).
	Push(ctx context.Context, repoID string, snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendDiverged bool) error

	// Pull downloads (snapshot metadata, doc body, ref) from the central server.
	// snapshotStates are local state hashes keyed by snapshot ID; a modern peer
	// returns only new or changed metadata, while a legacy manifest safely falls
	// back to all snapshots. docHaves are verified local document hashes.
	Pull(ctx context.Context, repoID string, snapshotStates map[domain.ContentHash]domain.ContentHash, docHaves []domain.ContentHash) (snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, err error)

	// RemoteManifest queries the server-side repo catalog. Used in push/pull negotiation step 1 to calculate what to send/receive.
	RemoteManifest(ctx context.Context, repoID string) (domain.Manifest, error)

	// PromoteSnapshotMessage promotes server snapshot hook labels to commit messages (one-way: hook prefix → non-prefix only — dedup absorbs lost [git sha] links).
	PromoteSnapshotMessage(ctx context.Context, repoID string, id domain.ContentHash, message string) error

	// GraftSnapshotParents propagates local graft events using expectedSeq CAS. 409 stale/cycle is terminal, and retrying an already reflected edge is idempotent.
	GraftSnapshotParents(ctx context.Context, repoID string, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error

	// GetSnapshotRemote reads authoritative snapshot metadata, including the
	// server-owned graft register.
	GetSnapshotRemote(ctx context.Context, repoID string, id domain.ContentHash) (domain.Snapshot, error)

	// Search is the server search (commit messages, conversation bodies) — MCP context_search (read-only).
	Search(ctx context.Context, repoID, query string) ([]SearchHit, bool, error)

	// UpdateRefRemote requests a single ref move to the server. appendDiverged=true appends the diverged target to the server head without loss (graft) — the promotion path for context merged into the default branch. Targets already behind are rejected by the server as non_fast_forward, so the caller treats it as an idempotent no-op.
	UpdateRefRemote(ctx context.Context, repoID string, ref domain.Ref, appendDiverged bool) error

	// RegisterRepo registers repo metadata to the server (idempotently) and returns the server-confirmed record (including workspace_id — binding occurs if the remote URL path matches the workspace).
	// Call before push to ensure the server holds metadata like remote URL (sync protocol).
	RegisterRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error)
	// PushMemory advances the server's causal memory attachment using the
	// digest's PreviousMemoryHash as the expected pointer.
	PushMemory(ctx context.Context, repoID string, digest domain.MemoryDigest) error
	// PullMemory downloads the MemoryDigest from the snapshot. Returns ErrNotFound if not found.
	PullMemory(ctx context.Context, repoID string, snapshotID domain.ContentHash) (domain.MemoryDigest, error)
	// PullMemoryObject downloads one immutable memory object by its own hash so
	// pull can prove fast-forward ancestry before moving a local pointer.
	PullMemoryObject(ctx context.Context, repoID string, hash domain.ContentHash) (domain.MemoryDigest, error)
	// PullSettings downloads the team default settings bundle
	// (claude|agents|codex). Returns ErrNotFound if not found.
	PullSettings(ctx context.Context, repoID, kind string) (domain.SettingsBundle, error)
	// Secret ciphertext envelope (E2E — raw bytes, server opaque). rotate=true performs an explicit replacement — protects updates inserted during CAS (conditional assignment) based on the envelope's fingerprint read during replacement.
	PushSecrets(ctx context.Context, repoID string, raw []byte, rotate bool, expect string) error
	PullSecrets(ctx context.Context, repoID string) ([]byte, error)
	// Propagate content-addressed commit attachment object.
	PushSettingsObject(ctx context.Context, repoID string, hash domain.ContentHash, bundle domain.SettingsBundle) error
	PullSettingsObject(ctx context.Context, repoID string, hash domain.ContentHash) (domain.SettingsBundle, error)

	// Synchronize in-progress context pointer (hook auto capture → web display. Resolve by deletion on commit).
	PushPending(ctx context.Context, repoID string, p domain.Pending) error
	DeletePendingRemote(ctx context.Context, repoID string, sessionID string) error

	// Push wait (unsync) pointer synchronization (local ahead commit → web On Hold display. git push to resolve).
	PushUnsync(ctx context.Context, repoID string, u domain.Unsync) error
	DeleteUnsyncRemote(ctx context.Context, repoID string, branch string) error
}

// PushObjectWants is the server-proven missing subset of a local push
// inventory. Snapshot metadata and document bodies are independent objects: a
// damaged server may have one without the other, so both sets remain explicit.
type PushObjectWants struct {
	Snapshots []domain.ContentHash
	Docs      []domain.ContentHash
}

// PushObjectNegotiator is an optional preflight capability for RemoteSync.
// It lets the application advertise only content hashes before opening large
// cumulative session documents. Chunk hashes are intentionally deferred: once
// a missing document is selected and opened, RemoteSync.Push performs its
// normal document/chunk negotiation and resumes any partially staged upload.
// Remotes without this capability retain the safe legacy behavior of loading
// and passing the full reachable object set to RemoteSync.Push, whose own
// negotiation still prevents redundant transfer.
type PushObjectNegotiator interface {
	NegotiatePushObjects(ctx context.Context, repoID string, snapshotHaves, docHaves []domain.ContentHash) (PushObjectWants, error)
}

// MemorySource is a port that absorbs provider native memory (compatibility rules).
//
// Absorb if available: Claude MEMORY.md, Codex rollout_summary, etc. Input source for native-first strategy.
type MemorySource interface {
	// Provider returns the provider this source is responsible for (claude|codex).
	Provider() domain.ProviderKind

	// ReadNative reads provider native memory for the exact provider session when
	// sessionID is available. An empty sessionID falls back to the newest memory
	// associated with cwd. Returns found=false if not found (no error).
	ReadNative(ctx context.Context, cwd, sessionID string) (native domain.NativeMemory, found bool, err error)
}

// MemoryDistiller is a port that generates MemoryDigest (summary memory) (compatibility rules).
//
// Native-first + fallback: Absorb if native exists (non-nil), otherwise self-distill from CIR.
// For memory-form load (CLAUDE.md/AGENTS.md injection). Uses redacted_summary for reasoning.
// The returned MemoryDigest.SnapshotID is empty and is post-injected by the app layer.
type MemoryDistiller interface {
	// Distill extracts summaries/key facts/unresolved tasks from native (if available) and CIR.
	// If native==nil, self-distill from CIR (fallback). Deterministic.
	Distill(ctx context.Context, cir domain.CIRDocument, native *domain.NativeMemory) (domain.MemoryDigest, error)
}

// MemorySink is a port that injects MemoryDigest into the target provider native memory file (compatibility rules).
//
// Writes to Claude CLAUDE.md/MEMORY.md, Codex AGENTS.md, etc. Does not hijack native directories.
// Records only at sink time during load (compatibility rules).
type MemorySink interface {
	// Provider returns the provider this sink is responsible for (claude|codex).
	Provider() domain.ProviderKind

	// Inject writes the digest to a provider memory file based on the cwd and returns the record path.
	Inject(ctx context.Context, digest domain.MemoryDigest, cwd string) (writtenPath string, err error)
}

// SessionMaterializer is a port that records session bytes (encoded in the target provider format) to a native session file,
// enabling resume (CIR→Provider Byte Encoding is the responsibility of ProviderCodec.Encode, and the materializer is a pure writer that receives the raw result and writes to the file system [Hexagonal Architecture: Adapter→Adapter Dependency Avoidance]). The LoadSession use-case orchestrates codec.Encode → materializer.Materialize.
//
// claude writes to ~/.claude/projects/<cwd-encoded>/<newid>.jsonl and returns resumeCmd="claude --resume <id>", while codex writes to ~/.codex/sessions/.../rollout-*.jsonl and returns "codex resume <id>".
type SessionMaterializer interface {
	// Provider returns the provider this materializer is responsible for (claude|codex).
	Provider() domain.ProviderKind

	// Materialize records the encoded session bytes (raw) to a native session file based on the cwd. It returns the sessionPath (record path) and resumeCmd (native resume command).
	Materialize(ctx context.Context, raw []byte, cwd string) (sessionPath string, resumeCmd string, err error)
}

// SearchHit is a server search hit (an element of the GET /repos/{id}/search response).
type SearchHit struct {
	SnapshotID string `json:"snapshot_id"`
	Branch     string `json:"branch"`
	Kind       string `json:"kind"`
	Role       string `json:"role,omitempty"`
	Snippet    string `json:"snippet"`
	CreatedAt  string `json:"created_at"`
}
