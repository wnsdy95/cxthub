// chunks.go — Session doc chunk CAS decomposition/reassembly rules (domain model).
//
// Storage adapters (FS/PG) and sync service (push/pull chunk wire) must use the same rules
// to ensure chunk hashes match between client ↔ server for dedup/delta transmission. The CLI
// adapters/chunkcas must follow the same rules (module separation — contract mirror).
//
//   - Canonical bytes must be exactly `{"envelope":<env>,"events":[<e1>,…]}` (key sorting/compaction).
//   - v1 accumulates whole event fragments and joins them with '\n'. It remains readable for compatibility.
//   - v2 chunks the canonical events-array interior byte stream at fixed boundaries. This keeps
//     append-only prefixes stable while allowing a chunk boundary inside an arbitrarily large event.
//   - Integrity (DocHash) is always the hash of the entire canonical — unchanged by chunking.
package domain

import (
	"bytes"
	"encoding/json"
)

// ChunkTarget is the size threshold (uncompressed canonical byte count) that closes a chunk.
const ChunkTarget = 512 << 10

// MaxPortableChunkBytes is the raw body limit for one bounded HTTP batch. v2 chunks are always
// smaller than this; v1 compatibility planning falls back when a whole event would exceed it.
const MaxPortableChunkBytes = 2 << 20

const (
	ChunkFormatV1 = "cxt-doc-chunks-v1"
	ChunkFormatV2 = "cxt-doc-chunks-v2"
	// ChunkFormat is the format emitted by new storage writes.
	ChunkFormat = ChunkFormatV2
)

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

// PlanDocChunks emits the current bounded stream format. It is independent of event size.
func PlanDocChunks(cb []byte) (DocChunkPlan, bool) {
	env, stream, ok := canonicalStream(cb)
	if !ok || len(stream) == 0 {
		return DocChunkPlan{}, false
	}
	// V2 is the exact array interior. Do not decode/copy every event, join the
	// entire transcript, then assemble another whole copy merely to split it.
	return buildDocChunkPlan(ChunkFormatV2, env, chunkByteStream(stream)), true
}

// PlanDocChunksV1 retains the old event-boundary wire plan for peers that do not advertise v2.
func PlanDocChunksV1(cb []byte) (DocChunkPlan, bool) {
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
	return buildDocChunkPlan(ChunkFormatV1, env, raw), true
}

func buildDocChunkPlan(format string, env json.RawMessage, raw [][]byte) DocChunkPlan {
	p := DocChunkPlan{Manifest: DocChunkManifest{Format: format, Envelope: env}, Bodies: make(map[ContentHash][]byte, len(raw))}
	for _, c := range raw {
		h := HashContent(c)
		p.Manifest.Chunks = append(p.Manifest.Chunks, h)
		p.Order = append(p.Order, h)
		p.Bodies[h] = c
	}
	return p
}

// ParseDocChunkManifest parses data if it's a chunk manifest (otherwise ok=false — legacy fallback).
func ParseDocChunkManifest(data []byte) (DocChunkManifest, bool) {
	limit := len(data)
	if limit > 64 {
		limit = 64
	}
	if !bytes.Contains(data[:limit], []byte("cxt-doc-chunks-v")) {
		return DocChunkManifest{}, false
	}
	var man DocChunkManifest
	if err := json.Unmarshal(data, &man); err != nil || !SupportedChunkFormat(man.Format) || len(man.Envelope) == 0 || len(man.Chunks) == 0 {
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
	var cb []byte
	switch normalizeChunkFormat(man.Format) {
	case ChunkFormatV1:
		events := make([][]byte, 0, len(chunks)*8)
		for _, c := range chunks {
			events = append(events, SplitDocChunk(c)...)
		}
		cb = AssembleCanonicalDoc(man.Envelope, events)
	case ChunkFormatV2:
		size := len(man.Envelope) + 25
		for _, chunk := range chunks {
			size += len(chunk)
		}
		var body bytes.Buffer
		body.Grow(size)
		body.WriteString(`{"envelope":`)
		body.Write(man.Envelope)
		body.WriteString(`,"events":[`)
		for _, chunk := range chunks {
			body.Write(chunk)
		}
		body.WriteString(`]}`)
		cb = body.Bytes()
	default:
		return nil, ErrIntegrity
	}
	if HashContent(cb) != want {
		return nil, ErrIntegrity
	}
	return cb, nil
}

// SupportedChunkFormat reports manifest formats this binary can safely assemble.
func SupportedChunkFormat(format string) bool {
	format = normalizeChunkFormat(format)
	return format == ChunkFormatV1 || format == ChunkFormatV2
}

// Empty format is the pre-versioned wire representation and therefore means v1.
func normalizeChunkFormat(format string) string {
	if format == "" {
		return ChunkFormatV1
	}
	return format
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

// canonicalStream checks the complete JSON and its canonical outer framing.
// Returned envelope is owned by the plan; the event stream is only borrowed
// while chunkByteStream makes independently owned bounded bodies.
func canonicalStream(cb []byte) (json.RawMessage, []byte, bool) {
	const prefix = `{"envelope":`
	const middle = `,"events":[`
	if !bytes.HasPrefix(cb, []byte(prefix)) || !bytes.HasSuffix(cb, []byte(`]}`)) {
		return nil, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(cb[len(prefix):]))
	var env json.RawMessage
	if dec.Decode(&env) != nil {
		return nil, nil, false
	}
	offset := len(prefix) + int(dec.InputOffset())
	if !bytes.HasPrefix(cb[offset:], []byte(middle)) {
		return nil, nil, false
	}
	start := offset + len(middle)
	if start > len(cb)-2 || !json.Valid(cb[start-1:len(cb)-1]) {
		return nil, nil, false
	}
	stream := cb[start : len(cb)-2]
	// The old planner removed whitespace between events. Keep the v2 wire
	// boundaries unchanged by accepting only compact canonical framing here.
	quoted, escaped := false, false
	for _, ch := range stream {
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
		} else {
			if ch == '"' {
				quoted = true
			} else if ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t' {
				return nil, nil, false
			}
		}
	}
	return env, stream, true
}
