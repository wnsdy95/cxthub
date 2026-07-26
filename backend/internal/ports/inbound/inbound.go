// Package inbound defines server use-case (driving) ports.
//
// These ports are server-side business entry points called by the REST delivery layer (adapters/delivery/http). Derived from sync protocol port mapping and compatibility rules use-case from the server's perspective (validator). Unlike the client's inbound (SaveSession/LoadSession etc. client workflows), the server focuses on object storage, validation, ref CAS, and negotiation (sync protocol).
package inbound

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CommitSnapshot validates and atomically stores a content-addressed snapshot
// and document batch after recalculating every object hash.
type CommitSnapshot interface {
	Commit(ctx context.Context, in CommitInput) (CommitOutput, error)
}

// ForkSnapshot creates a new branch ref by branching off a tip from FromSnapshot ("branch replication + ancestor tracking") (data model, sync protocol fork, auto fork). Adds a new branch ref with tip FromSnapshot (original unchanged, F1). No data duplication.
type ForkSnapshot interface {
	Fork(ctx context.Context, in ForkInput) (ForkOutput, error)
}

// DiffSnapshots returns the event delta between two snapshots (sync protocol diff, compatibility rules). LCA-based comparison is the app's responsibility (data model).
type DiffSnapshots interface {
	Diff(ctx context.Context, in DiffInput) (DiffOutput, error)
}

// UpdateRef moves a ref using compare-and-swap. ExpectedTarget provides
// optimistic concurrency control for fast-forward and divergent updates.
type UpdateRef interface {
	UpdateRef(ctx context.Context, in UpdateRefInput) (UpdateRefOutput, error)
}

// ListSnapshots returns a list of snapshot metadata per branch (sync protocol, ListSnapshots mirror).
type ListSnapshots interface {
	List(ctx context.Context, in ListSnapshotsInput) ([]domain.Snapshot, error)
}

// PushReceive performs push negotiation (sync protocol step A). Responds with the missing parts (want) after subtracting what the server already has from the client's haves.
type PushReceive interface {
	Negotiate(ctx context.Context, in PushNegotiateInput) (PushNegotiateOutput, error)
}

// StoreChunks is the upload phase of bounded chunk transport. First, it stores content-addressed chunks idempotently. Then, it validates and confirms the existing Commit with the manifest. Partial failures are retryable in the next negotiate, excluding already stored chunks.
type StoreChunks interface {
	StoreChunks(ctx context.Context, in StoreChunksInput) (StoreChunksOutput, error)
}

// PullChunks bounds the total raw response size so it stays below proxy and Cloud Run body limits.
type PullChunks interface {
	PullChunks(ctx context.Context, in PullChunksInput) (PullChunksOutput, error)
}

// PromoteSnapshot promotes the message of a hook label snapshot to a commit message (one-way).
// Content-hash deduplication allows hook capture leaves to commit, recovering lost [git sha] links —
// Inventory-only push requires a dedicated metadata update path since it does not resend existing objects.
type PromoteSnapshot interface {
	PromoteSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error
}

// JoinSnapshot repositions session forks (sibling branches) of the same git branch behind the branch head (web graph drag&drop).
// It performs a graft overlay and ref movement under the parent immutability principle and does not allow cross-branch merges.
type JoinSnapshot interface {
	Join(ctx context.Context, in JoinInput) (JoinOutput, error)
}

// JoinInput repositions Snapshot(X) behind TargetBranch head(H). Only git branch membership projected via reflog is allowed.
// Segments are unique first-parent child paths starting from SessionID, not from X. The server calculates the graph directly.
// If IncludeDescendants is true, head advances to the server-calculated tip (entire branch), otherwise, it advances only to X,
// and remaining descendants are preserved as internal session refs branching from X.
type JoinInput struct {
	RepoID             domain.ContentHash
	TargetBranch       string
	Snapshot           domain.ContentHash
	IncludeDescendants bool
}

// JoinOutput (wire: snake_case).
type JoinOutput struct {
	Branch string             `json:"branch"`
	Head   domain.ContentHash `json:"head"`
	// ForkBranch names the session ref left behind in the NO (partial merge) case (omitted if none).
	ForkBranch string `json:"fork_branch,omitempty"`
}

// PullSend handles pull/objects requests to batch-send missing objects (sync protocol Step B).
// Pull is read-only and does not change the server (sync protocol Step B).
type PullSend interface {
	Send(ctx context.Context, in PullSendInput) (PullSendOutput, error)
}

// Authenticate verifies team token + user identity (sync protocol).
// It checks token-to-team mapping, identity consistency, and repository visibility.
type Authenticate interface {
	Authenticate(ctx context.Context, in AuthInput) (AuthOutput, error)
}

// --- DTO (name/type only; derive from sync protocol wire form) ---

// ChunkedDoc represents a doc as a manifest (envelope+chunk hash) in a wire format (chunk delta transmission).
type ChunkedDoc struct {
	Hash     domain.ContentHash   `json:"hash"`
	Envelope json.RawMessage      `json:"envelope"`
	Chunks   []domain.ContentHash `json:"chunks"`
}

// ChunkObject is the chunk body (Data = uncompressed chunk bytes — JSON base64).
type ChunkObject struct {
	Hash domain.ContentHash `json:"hash"`
	Data []byte             `json:"data"`
}

const (
	// Limit raw batch to 2MiB to comfortably fit within 4MiB, even with JSON base64 overhead. Explicitly reject chunks larger than this for a single event.
	MaxChunkWireRawBytes = domain.MaxPortableChunkBytes
	MaxChunkWireObjects  = 32
	MaxChunkWireJSONBody = 4 << 20
	MaxChunkWantJSONBody = 64 << 10
)

type StoreChunksInput struct {
	RepoID domain.ContentHash
	Chunks []ChunkObject
}

type StoreChunksOutput struct {
	Stored  int `json:"stored"`
	Deduped int `json:"deduped"`
}

type PullChunksInput struct {
	RepoID domain.ContentHash
	Wants  []domain.ContentHash
}

type PullChunksOutput struct {
	ChunkObjects []ChunkObject `json:"chunk_objects"`
}

// CommitInput represents the push/objects body (sync protocol Step B).
// ChunkedDocs represent the chunk wire format of a doc — the body is composed of ChunkObjects (server failure chunks) and reassembled, hashed, and stored in the same path as Docs.
type CommitInput struct {
	RepoID       domain.ContentHash
	Snapshots    []domain.Snapshot
	Docs         []domain.SessionDoc
	ChunkedDocs  []ChunkedDoc
	ChunkObjects []ChunkObject
}

// CommitOutput represents a store/duplicate aggregation.
type CommitOutput struct {
	StoredSnapshots  int
	StoredDocs       int
	DedupedSnapshots int
	DedupedDocs      int
}

// ForkInput (data model ForkSession mirror, sync protocol).
type ForkInput struct {
	RepoID       domain.ContentHash
	FromSnapshot domain.ContentHash
	NewBranch    string
	Author       domain.TeamIdentity
}

// ForkOutput: the created branch and its head. (wire: snake_case)
type ForkOutput struct {
	Branch string             `json:"branch"`
	Head   domain.ContentHash `json:"head"`
}

// DiffInput: comparison of two snapshots (left/right) (sync protocol).
type DiffInput struct {
	RepoID domain.ContentHash
	Left   domain.ContentHash
	Right  domain.ContentHash
}

// DiffEntry: Single event change between two snapshots (similar to frontend DiffEntry, wire: op/seq/summary).
type DiffEntry struct {
	Op      string `json:"op"`      // "add" | "remove"
	Seq     int    `json:"seq"`     // CIR seq of the target event
	Summary string `json:"summary"` // Human-readable change summary
}

// DiffOutput: List of event unit changes (1:1 with frontend {changes:[...]}).
type DiffOutput struct {
	Changes []DiffEntry `json:"changes"`
}

// UpdateRefInput: CAS ref move (sync protocol).
type UpdateRefInput struct {
	RepoID         domain.ContentHash
	Ref            domain.Ref
	ExpectedTarget domain.ContentHash
	// Force allows non-fast-forward moves (equivalent to git push --force). If false (default), non-fast-forward moves without the previous commit as an ancestor are rejected with ErrNonFastForward.
	Force bool
	// Append appends a diverged push with no common ancestor (orphaned) to the current head: attaches the current head as a parent to the root of the incoming lineage (graft) followed by a fast-forward. Parents are not included in the content-hash, so snapshot IDs are immutable, and the entire history is preserved (unlike Force, which results in no loss).
	Append bool
}

// RefUpdateResult is the result of ref move determination (sync protocol).
type RefUpdateResult string

const (
	RefFastForward    RefUpdateResult = "fast_forward"
	RefUpToDate       RefUpdateResult = "up_to_date"
	RefNonFastForward RefUpdateResult = "non_fast_forward"
	RefDivergedForked RefUpdateResult = "diverged_forked"
	// RefForced indicates a forced move using --force.
	RefForced RefUpdateResult = "forced"
	// RefAppended indicates an irrelevant diverged push that was grafted onto the current head.
	RefAppended RefUpdateResult = "appended"
)

// UpdateRefOutput: verdict + (fork info when diverged) (sync protocol).
type UpdateRefOutput struct {
	Result          RefUpdateResult
	Ref             domain.Ref
	RequestedTarget domain.ContentHash
	ServerTarget    domain.ContentHash
	ForkedRef       *domain.Ref
	MergeBase       domain.ContentHash
}

// ListSnapshotsInput: branch filter (empty for all).
type ListSnapshotsInput struct {
	RepoID domain.ContentHash
	Branch string
}

// PushNegotiateInput (sync protocol step A request).
type PushNegotiateInput struct {
	RepoID        domain.ContentHash
	SnapshotHaves []domain.ContentHash
	DocHaves      []domain.ContentHash
	// ChunkHaves is the chunk plan hash of docs the client will send — only missing parts are answered (delta upload).
	ChunkHaves []domain.ContentHash
}

// PushNegotiateOutput: missing parts (want) the server actually needs. (wire: snake_case)
type PushNegotiateOutput struct {
	SnapshotWants []domain.ContentHash `json:"snapshot_wants"`
	DocWants      []domain.ContentHash `json:"doc_wants"`
	// ChunksSupported true = chunk wire support (GCS ignores — operates as-is).
	ChunksSupported bool `json:"chunks_supported,omitempty"`
	// BoundedChunksSupported true if chunk bodies are sent as bounded batches to /push/chunks·/pull/chunks. For old servers, the existing push/objects compatibility paths are used.
	BoundedChunksSupported bool                 `json:"bounded_chunks_supported,omitempty"`
	ChunkWants             []domain.ContentHash `json:"chunk_wants,omitempty"`
}

// PullSendInput (sync protocol Step B Request).
type PullSendInput struct {
	RepoID        domain.ContentHash
	SnapshotWants []domain.ContentHash
	DocWants      []domain.ContentHash
	// Chunk Wire (Delta Download): Manifest/Chunk Body Request.
	DocManifestWants []domain.ContentHash
	ChunkWants       []domain.ContentHash
}

// PullSendOutput: Download Target Object (push/objects Request Body Equivalent). (wire: snake_case)
type PullSendOutput struct {
	Snapshots []domain.Snapshot   `json:"snapshots"`
	Docs      []domain.SessionDoc `json:"docs"`
	// DocManifests are the chunk manifests requested for the doc — impossible docs are returned as Docs in a single response.
	DocManifests []ChunkedDoc  `json:"doc_manifests,omitempty"`
	ChunkObjects []ChunkObject `json:"chunk_objects,omitempty"`
	// The manifest response announces the pull/chunks capability. Old clients ignore this.
	BoundedChunksSupported bool `json:"bounded_chunks_supported,omitempty"`
}

// AuthInput: Bearer team token + X-Cxt-Identity header parsing result (sync protocol).
type AuthInput struct {
	TeamToken string
	Identity  domain.TeamIdentity
	RepoID    domain.ContentHash // Visibility check target (empty value means repo-agnostic call)
}

// AuthOutput: Empirically verified team context.
type AuthOutput struct {
	Team     string
	Identity domain.TeamIdentity
}

// SearchInput: Searches snapshot metadata (commit message/author) and conversation body (CIR message/reasoning) as substring (web UI "Where did they say that?" requirement — case-insensitive).
type SearchInput struct {
	RepoID domain.ContentHash
	Query  string
	Limit  int // 0 means server default (50)
}

// SearchHit: Search result item. Kind: "commit" (message/author match) | "event" (conversation body match).
// (wire: snake_case — created_at is RFC3339)
type SearchHit struct {
	SnapshotID domain.ContentHash `json:"snapshot_id"`
	Branch     string             `json:"branch"`
	Kind       string             `json:"kind"`
	Role       string             `json:"role,omitempty"` // Speaker (user/assistant ...) in event match
	Seq        int                `json:"seq,omitempty"`  // CIR seq in event match
	Snippet    string             `json:"snippet"`
	CreatedAt  string             `json:"created_at"`
}

// SearchOutput: Hit list + whether limit is reached (client should narrow search term if reached).
type SearchOutput struct {
	Hits      []SearchHit `json:"hits"`
	Truncated bool        `json:"truncated"`
}

// DanglingParent: Points to parent snapshot that does not exist (real corruption — git fsck missing parent response).
type DanglingParent struct {
	Snapshot domain.ContentHash `json:"snapshot"`
	Missing  domain.ContentHash `json:"missing"`
}

// FsckReport: Reference reachability audit results (read-only, git fsck response). Do not fix anything.
// Parentless snapshots are classified as normal roots and are not errors.
type FsckReport struct {
	Total           int                  `json:"total"`            // Total number of snapshots
	Reachable       int                  `json:"reachable"`        // Number of snapshots reachable from any ref
	Roots           []domain.ContentHash `json:"roots"`            // Snapshots without parents (normal roots)
	Unreachable     []domain.ContentHash `json:"unreachable"`      // Snapshots that cannot be reached (dangling/lost)
	DanglingParents []DanglingParent     `json:"dangling_parents"` // Non-existent parent references (corrupted)
}
