// Package chunkcas is the single source of truth for session doc chunk CAS decomposition/reassembly rules.
//
// Adapters/storage and adapters/backendclient must use the same rules to ensure that chunk hashes match locally ↔ server for deduplication/delta transmission to work. Rules:
//
//   - Canonical bytes must be exactly `{"envelope":<env>,"events":[<e1>,…]}` (key sorted and compact).
//   - Accumulate canonical event fragments in order to pass ChunkTarget, which closes the chunk (prefix-stable — append-only sessions have the same captured chunks).
//   - Chunk bytes = concatenating fragments with '\n'. Chunk hash = HashContent(chunk bytes).
//   - DocHash is always the hash of the entire canonical — unchanged regardless of chunking.
//
// Plan returns only the plan for reassembly==original validation (ok=false — fallback).
package chunkcas

import (
	"bytes"
	"encoding/json"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ChunkTarget is the chunk closure size threshold (uncompressed canonical byte count).
const ChunkTarget = 512 << 10

// MaxPortableChunkBytes is the upper limit for a single chunk that can be safely transported using bounded HTTP transport. If an event is larger than this, PlanDoc selects a full doc fallback.
const MaxPortableChunkBytes = 2 << 20

// Format is the chunk doc manifest identifier (canonical doc starts with "envelope" key — no overlap).
const Format = "cxt-doc-chunks-v1"

// Manifest is the chunk doc manifest (common form for storage and wire).
type Manifest struct {
	Format   string               `json:"format"`
	Envelope json.RawMessage      `json:"envelope"`
	Chunks   []domain.ContentHash `json:"chunks"`
}

// Plan is the chunk decomposition plan for canonical bytes.
type Plan struct {
	Manifest Manifest
	// Bodies are chunk hashes → uncompressed chunk bytes.
	Bodies map[domain.ContentHash][]byte
	// Order is the chunk hash in manifest order.
	Order []domain.ContentHash
}

// PlanDoc distills canonical bytes and validates reassembly integrity. ok=false falls back to legacy.
func PlanDoc(cb []byte) (Plan, bool) {
	env, events, err := split(cb)
	if err != nil || len(env) == 0 || len(events) == 0 {
		return Plan{}, false
	}
	raw := chunkEvents(events)
	for _, c := range raw {
		if len(c) > MaxPortableChunkBytes {
			return Plan{}, false
		}
	}
	flat := make([][]byte, 0, len(events))
	for _, c := range raw {
		flat = append(flat, SplitChunk(c)...)
	}
	if !bytes.Equal(Assemble(env, flat), cb) {
		return Plan{}, false
	}
	p := Plan{Manifest: Manifest{Format: Format, Envelope: env}, Bodies: make(map[domain.ContentHash][]byte, len(raw))}
	for _, c := range raw {
		h := domain.HashContent(c)
		p.Manifest.Chunks = append(p.Manifest.Chunks, h)
		p.Order = append(p.Order, h)
		p.Bodies[h] = c
	}
	return p, true
}

// ParseManifest parses data if it's a chunk manifest (otherwise ok=false — legacy fallback).
func ParseManifest(data []byte) (Manifest, bool) {
	limit := len(data)
	if limit > 64 {
		limit = 64
	}
	if !bytes.Contains(data[:limit], []byte(Format)) {
		return Manifest{}, false
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil || man.Format != Format {
		return Manifest{}, false
	}
	return man, true
}

// Assemble recovers canonical bytes from envelope and event fragments (byte-accurate).
func Assemble(env json.RawMessage, events [][]byte) []byte {
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

// AssembleChunks recovers canonical bytes from chunk bytes in manifest order and compares integrity hash (dirt detection).
func AssembleChunks(man Manifest, chunks [][]byte, want domain.ContentHash) ([]byte, error) {
	events := make([][]byte, 0, len(chunks)*8)
	for _, c := range chunks {
		events = append(events, SplitChunk(c)...)
	}
	cb := Assemble(man.Envelope, events)
	if domain.HashContent(cb) != want {
		return nil, domain.ErrHashMismatch
	}
	return cb, nil
}

// SplitChunk converts chunk bytes back into event fragments.
func SplitChunk(b []byte) [][]byte { return bytes.Split(b, []byte("\n")) }

func split(cb []byte) (env json.RawMessage, events []json.RawMessage, err error) {
	var frag struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(cb, &frag); err != nil {
		return nil, nil, err
	}
	return frag.Envelope, frag.Events, nil
}

func chunkEvents(events []json.RawMessage) [][]byte {
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
