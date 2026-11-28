// Package inbound defines server use-case (driving) ports.
//
// These ports are server-side business entry points called by the REST delivery layer (adapters/delivery/http). Derived from sync protocol port mapping and compatibility rules use-case from the server's perspective (validator). The server focuses on object storage, validation and negotiation (sync protocol).
package inbound

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

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
	RepoID domain.ContentHash
	// Captures is the number of capture records (the wire field "snapshots") in
	// the request. The server no longer stores captures and refuses a batch
	// that carries any.
	Captures     int
	Docs         []domain.SessionDoc
	ChunkedDocs  []ChunkedDoc
	ChunkObjects []ChunkObject
}

// CommitOutput represents a store/duplicate aggregation.
type CommitOutput struct {
	StoredDocs  int
	DedupedDocs int
}

// PushNegotiateInput (sync protocol step A request).
type PushNegotiateInput struct {
	RepoID   domain.ContentHash
	DocHaves []domain.ContentHash
	// ChunkHaves is the chunk plan hash of docs the client will send — only missing parts are answered (delta upload).
	ChunkHaves []domain.ContentHash
}

// PushNegotiateOutput: missing parts (want) the server actually needs. (wire: snake_case)
type PushNegotiateOutput struct {
	DocIdentitiesSupported          []domain.DocumentIdentity `json:"doc_identities_supported"`
	RootPublicationEnabled          bool                      `json:"root_publication_enabled"`
	PreparedMemoryArchivesSupported bool                      `json:"prepared_memory_archives_supported,omitempty"`
	AsyncDocsSupported              bool                      `json:"async_docs_supported,omitempty"`
	DocWants                        []domain.ContentHash      `json:"doc_wants"`
	// ChunksSupported true = chunk wire support (GCS ignores — operates as-is).
	ChunksSupported bool `json:"chunks_supported,omitempty"`
	// BoundedChunksSupported true if chunk bodies are sent as bounded batches to /push/chunks. For old servers, the existing push/objects compatibility paths are used.
	BoundedChunksSupported bool     `json:"bounded_chunks_supported,omitempty"`
	ChunkFormatsSupported  []string `json:"chunk_formats_supported,omitempty"`
	// CIRVersionsSupported advertises document semantics the server can hash and store.
	// Absence from an old server means CIR v1 only.
	CIRVersionsSupported []string             `json:"cir_versions_supported,omitempty"`
	ChunkWants           []domain.ContentHash `json:"chunk_wants,omitempty"`
}
