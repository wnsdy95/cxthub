// Package chunkcas is the single source of truth for session doc chunk CAS decomposition/reassembly rules.
//
// Adapters/storage and adapters/backendclient must use the same rules to ensure that chunk hashes match locally ↔ server for deduplication/delta transmission to work. Rules:
//
//   - Canonical bytes must be exactly `{"envelope":<env>,"events":[<e1>,…]}` (key sorted and compact).
//   - v1 keeps whole canonical events joined with '\n' for old peer compatibility.
//   - v2 chunks the canonical events-array interior at fixed byte offsets, so one event can be arbitrarily large.
//   - Both formats keep closed append-only prefixes stable. Chunk hash = HashContent(chunk bytes).
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

// MaxPortableChunkBytes is the raw body limit for one bounded HTTP batch. v2 chunks are always
// below it; only PlanDocV1 falls back when one whole event exceeds it.
const MaxPortableChunkBytes = 2 << 20

const (
	FormatV1 = "cxt-doc-chunks-v1"
	FormatV2 = "cxt-doc-chunks-v2"
	// Format is emitted by new local storage writes.
	Format = FormatV2
)

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

// PlanDoc emits the current byte-stream format, whose chunks remain bounded even when one event is huge.
func PlanDoc(cb []byte) (Plan, bool) {
	env, events, err := split(cb)
	if err != nil || len(env) == 0 || len(events) == 0 {
		return Plan{}, false
	}
	stream := joinEventStream(events)
	raw := chunkByteStream(stream)
	if !bytes.Equal(assembleStream(env, bytes.Join(raw, nil)), cb) {
		return Plan{}, false
	}
	return buildPlan(FormatV2, env, raw), true
}

// PlanDocV1 retains the old whole-event format for compatibility with peers without v2 capability.
func PlanDocV1(cb []byte) (Plan, bool) {
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
	return buildPlan(FormatV1, env, raw), true
}

func buildPlan(format string, env json.RawMessage, raw [][]byte) Plan {
	p := Plan{Manifest: Manifest{Format: format, Envelope: env}, Bodies: make(map[domain.ContentHash][]byte, len(raw))}
	for _, c := range raw {
		h := domain.HashContent(c)
		p.Manifest.Chunks = append(p.Manifest.Chunks, h)
		p.Order = append(p.Order, h)
		p.Bodies[h] = c
	}
	return p
}

// ParseManifest parses data if it's a chunk manifest (otherwise ok=false — legacy fallback).
func ParseManifest(data []byte) (Manifest, bool) {
	limit := len(data)
	if limit > 64 {
		limit = 64
	}
	if !bytes.Contains(data[:limit], []byte("cxt-doc-chunks-v")) {
		return Manifest{}, false
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil || !SupportedFormat(man.Format) || len(man.Envelope) == 0 || len(man.Chunks) == 0 {
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
	var cb []byte
	switch normalizeFormat(man.Format) {
	case FormatV1:
		events := make([][]byte, 0, len(chunks)*8)
		for _, c := range chunks {
			events = append(events, SplitChunk(c)...)
		}
		cb = Assemble(man.Envelope, events)
	case FormatV2:
		cb = assembleStream(man.Envelope, bytes.Join(chunks, nil))
	default:
		return nil, domain.ErrHashMismatch
	}
	if domain.HashContent(cb) != want {
		return nil, domain.ErrHashMismatch
	}
	return cb, nil
}

func SupportedFormat(format string) bool {
	format = normalizeFormat(format)
	return format == FormatV1 || format == FormatV2
}

func normalizeFormat(format string) string {
	if format == "" {
		return FormatV1
	}
	return format
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

func chunkByteStream(stream []byte) [][]byte {
	chunks := make([][]byte, 0, (len(stream)+ChunkTarget-1)/ChunkTarget)
	for len(stream) > 0 {
		n := ChunkTarget
		if len(stream) < n {
			n = len(stream)
		}
		chunks = append(chunks, append([]byte(nil), stream[:n]...))
		stream = stream[n:]
	}
	return chunks
}

func joinEventStream(events []json.RawMessage) []byte {
	var b bytes.Buffer
	for i, event := range events {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(event)
	}
	return b.Bytes()
}

func assembleStream(env json.RawMessage, stream []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(env) + len(stream) + 32)
	b.WriteString(`{"envelope":`)
	b.Write(env)
	b.WriteString(`,"events":[`)
	b.Write(stream)
	b.WriteString(`]}`)
	return b.Bytes()
}
