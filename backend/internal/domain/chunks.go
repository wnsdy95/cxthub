// chunks.go — Session doc chunk CAS decomposition/reassembly rules (domain model).
//
// Storage adapters (FS/PG) and sync service (push/pull chunk wire) must use the same rules
// to ensure chunk hashes match between client ↔ server for dedup/delta transmission. The CLI
// adapters/chunkcas must follow the same rules (module separation — contract mirror).
//
//   - Canonical bytes must be exactly `{"envelope":<env>,"events":[<e1>,…]}` (key sorting/compaction).
//   - Accumulating event canonical fragments in order to pass ChunkTarget closes the chunk
//     (prefix-stable — in an append-only session, closed chunks are consistent across captures).
//   - Chunk bytes = pieces joined by '\n'. Chunk hash = HashContent(chunk bytes).
//   - Integrity (DocHash) is always the hash of the entire canonical — unchanged by chunking.
package domain

import (
	"bytes"
	"encoding/json"
)

// ChunkTarget is the size threshold (uncompressed canonical byte count) that closes a chunk.
const ChunkTarget = 512 << 10

// MaxPortableChunkBytes is the upper limit of a single chunk that can be safely transported
// using bounded HTTP transport. If an event is larger than this, PlanDocChunks selects a full doc fallback.
const MaxPortableChunkBytes = 2 << 20

// ChunkFormat is the chunk doc manifest identifier (canonical doc starts with "envelope" key — no overlap).
const ChunkFormat = "cxt-doc-chunks-v1"

// DocChunkManifest is the manifest of a chunk doc (common form for storage and wire).
type DocChunkManifest struct {
	Format   string          `json:"format"`
	Envelope json.RawMessage `json:"envelope"`
	Chunks   []ContentHash   `json:"chunks"`
}

// DocChunkPlan is the chunk decomposition plan for canonical bytes.
type DocChunkPlan struct {
	Manifest DocChunkManifest
	Bodies   map[ContentHash][]byte // chunk hash → uncompressed chunk bytes
	Order    []ContentHash
}

// PlanDocChunks splits canonical bytes and validates reassembly integrity. ok=false falls back to legacy.
func PlanDocChunks(cb []byte) (DocChunkPlan, bool) {
	env, events, err := splitCanonicalDocBytes(cb)
	if err != nil || len(env) == 0 || len(events) == 0 {
		return DocChunkPlan{}, false
	}
	raw := chunkEventFragments(events)
	for _, c := range raw {
		if len(c) > MaxPortableChunkBytes {
			return DocChunkPlan{}, false
		}
	}
	flat := make([][]byte, 0, len(events))
	for _, c := range raw {
		flat = append(flat, SplitDocChunk(c)...)
	}
	if !bytes.Equal(AssembleCanonicalDoc(env, flat), cb) {
		return DocChunkPlan{}, false
	}
	p := DocChunkPlan{Manifest: DocChunkManifest{Format: ChunkFormat, Envelope: env}, Bodies: make(map[ContentHash][]byte, len(raw))}
	for _, c := range raw {
		h := HashContent(c)
		p.Manifest.Chunks = append(p.Manifest.Chunks, h)
		p.Order = append(p.Order, h)
		p.Bodies[h] = c
	}
	return p, true
}

// ParseDocChunkManifest parses data if it's a chunk manifest (otherwise ok=false — legacy fallback).
func ParseDocChunkManifest(data []byte) (DocChunkManifest, bool) {
	limit := len(data)
	if limit > 64 {
		limit = 64
	}
	if !bytes.Contains(data[:limit], []byte(ChunkFormat)) {
		return DocChunkManifest{}, false
	}
	var man DocChunkManifest
	if err := json.Unmarshal(data, &man); err != nil || man.Format != ChunkFormat {
		return DocChunkManifest{}, false
	}
	return man, true
}

// AssembleCanonicalDoc reconstructs canonical bytes from envelope pieces + event pieces.
func AssembleCanonicalDoc(env json.RawMessage, events [][]byte) []byte {
	var b bytes.Buffer
	b.Grow(len(env) + 64)
	b.WriteString(`{"envelope":`)
	b.Write(env)
	b.WriteString(`,"events":[`)
	for i, e := range events {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(e)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// AssembleDocChunks reconstructs canonical bytes from chunk bytes in manifest order and validates integrity (detects chunk corruption and manifest tampering).
func AssembleDocChunks(man DocChunkManifest, chunks [][]byte, want ContentHash) ([]byte, error) {
	events := make([][]byte, 0, len(chunks)*8)
	for _, c := range chunks {
		events = append(events, SplitDocChunk(c)...)
	}
	cb := AssembleCanonicalDoc(man.Envelope, events)
	if HashContent(cb) != want {
		return nil, ErrIntegrity
	}
	return cb, nil
}

// SplitDocChunk splits chunk bytes into event pieces.
func SplitDocChunk(b []byte) [][]byte { return bytes.Split(b, []byte("\n")) }

func splitCanonicalDocBytes(cb []byte) (env json.RawMessage, events []json.RawMessage, err error) {
	var frag struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(cb, &frag); err != nil {
		return nil, nil, err
	}
	return frag.Envelope, frag.Events, nil
}

func chunkEventFragments(events []json.RawMessage) [][]byte {
	var chunks [][]byte
	var cur bytes.Buffer
	for _, e := range events {
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.Write(e)
		if cur.Len() >= ChunkTarget {
			chunks = append(chunks, append([]byte(nil), cur.Bytes()...))
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		chunks = append(chunks, append([]byte(nil), cur.Bytes()...))
	}
	return chunks
}
