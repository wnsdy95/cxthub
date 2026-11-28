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

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ChunkTarget is the chunk closure size threshold (uncompressed canonical byte count).
const ChunkTarget = 512 << 10

// MaxPortableChunkBytes is the raw body limit for one bounded HTTP batch. v2 chunks are always
// below it.
const MaxPortableChunkBytes = 2 << 20

const (
	FormatV1 = "cxt-doc-chunks-v1"
	FormatV2 = "cxt-doc-chunks-v2"
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
	env, stream, ok := canonicalStream(cb)
	if !ok || len(stream) == 0 {
		return Plan{}, false
	}
	// V2 is the exact array interior. Do not decode/copy every event, join the
	// entire transcript, then assemble another whole copy merely to split it.
	return buildPlan(FormatV2, env, chunkByteStream(stream)), true
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
