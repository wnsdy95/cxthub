// Package inbound defines server use-case (driving) ports.
//
// These ports are server-side business entry points called by the REST delivery layer (adapters/delivery/http). Derived from sync protocol port mapping and compatibility rules use-case from the server's perspective (validator). Unlike the client's inbound (SaveSession/LoadSession etc. client workflows), the server focuses on object storage, validation, ref CAS, and negotiation (sync protocol).
package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// CommitSnapshot validates and atomically stores a content-addressed snapshot
// and document batch after recalculating every object hash.
type CommitSnapshot interface {
	Commit(ctx context.Context, in CommitInput) (CommitOutput, error)
}

// PushReceive performs push negotiation (sync protocol step A). Responds with the missing parts (want) after subtracting what the server already has from the client's haves.
type PushReceive interface {
	Negotiate(ctx context.Context, in PushNegotiateInput) (PushNegotiateOutput, error)
}

// StoreChunks is the upload phase of bounded chunk transport. First, it stores content-addressed chunks idempotently. Then, it validates and confirms the existing Commit with the manifest. Partial failures are retryable in the next negotiate, excluding already stored chunks.
type StoreChunks interface {
	StoreChunks(ctx context.Context, in StoreChunksInput) (StoreChunksOutput, error)
}

// PromoteSnapshot promotes the message of a hook label snapshot to a commit message (one-way).
// Content-hash deduplication allows hook capture leaves to commit, recovering lost [git sha] links —
// Inventory-only push requires a dedicated metadata update path since it does not resend existing objects.
type PromoteSnapshot interface {
	PromoteSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error
}

// Authenticate verifies team token + user identity (sync protocol).
// It checks token-to-team mapping, identity consistency, and repository visibility.
type Authenticate interface {
	Authenticate(ctx context.Context, in AuthInput) (AuthOutput, error)
}

// --- DTO (name/type only; derive from sync protocol wire form) ---

// ChunkedDoc represents a doc as a manifest (envelope+chunk hash) in a wire format (chunk delta transmission).
type ChunkedDoc = domain.DocumentRepresentation

// ChunkObject is the chunk body (Data = uncompressed chunk bytes — JSON base64).
type ChunkObject struct {
	Hash domain.ContentHash `json:"hash"`
	Data []byte             `json:"data"`
}

const (
	// Limit raw batch to 2MiB to comfortably fit within 4MiB, even with JSON base64 overhead.
	MaxChunkWireRawBytes = domain.MaxPortableChunkBytes
	MaxChunkWireObjects  = 32
	MaxChunkWireJSONBody = 4 << 20
)

type StoreChunksInput struct {
	RepoID domain.ContentHash
	Chunks []ChunkObject
}

type StoreChunksOutput struct {
	Stored  int `json:"stored"`
	Deduped int `json:"deduped"`
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
	DocIdentitiesSupported          []domain.DocumentIdentity `json:"doc_identities_supported"`
	RootPublicationEnabled          bool                      `json:"root_publication_enabled"`
	PreparedMemoryArchivesSupported bool                      `json:"prepared_memory_archives_supported,omitempty"`
	AsyncDocsSupported              bool                      `json:"async_docs_supported,omitempty"`
	SnapshotWants                   []domain.ContentHash      `json:"snapshot_wants"`
	DocWants                        []domain.ContentHash      `json:"doc_wants"`
	// ChunksSupported true = chunk wire support (GCS ignores — operates as-is).
	ChunksSupported bool `json:"chunks_supported,omitempty"`
	// BoundedChunksSupported true if chunk bodies are sent as bounded batches to /push/chunks·/pull/chunks. For old servers, the existing push/objects compatibility paths are used.
	BoundedChunksSupported bool     `json:"bounded_chunks_supported,omitempty"`
	ChunkFormatsSupported  []string `json:"chunk_formats_supported,omitempty"`
	// CIRVersionsSupported advertises document semantics the server can hash and store.
	// Absence from an old server means CIR v1 only.
	CIRVersionsSupported []string             `json:"cir_versions_supported,omitempty"`
	ChunkWants           []domain.ContentHash `json:"chunk_wants,omitempty"`
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
