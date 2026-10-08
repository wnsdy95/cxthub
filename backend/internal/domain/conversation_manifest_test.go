package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func conversationTestEnvelope(t testing.TB, precise bool) []byte {
	t.Helper()
	v := map[string]any{"captured_at": "2026-10-07T00:00:00Z", "cir_version": "2", "cwd": "/tmp/\uD55C\uAE00", "fidelity": "full", "git_branch": "main", "session_origin_id": "synthetic", "source_model": "model", "source_provider": "codex"}
	if precise {
		v["output_tokens"] = json.Number("9007199254740993")
	}
	raw, err := canonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func conversationTestPlan(t testing.TB, env, stream []byte, count int64) (ConversationManifest, map[ContentHash][]byte) {
	t.Helper()
	chunks := []ConversationManifestChunk{}
	bodies := map[ContentHash][]byte{}
	for pos := 0; pos < len(stream); pos += ConversationManifestChunkBytes {
		body := bytes.Clone(stream[pos:min(pos+ConversationManifestChunkBytes, len(stream))])
		h := HashContent(body)
		chunks = append(chunks, ConversationManifestChunk{Hash: h, Bytes: int64(len(body))})
		bodies[h] = body
	}
	m, err := NewConversationManifest(env, chunks, count)
	if err != nil {
		t.Fatal(err)
	}
	return m, bodies
}

func conversationTestVerify(m ConversationManifest, bodies map[ContentHash][]byte) error {
	h, err := ConversationManifestHash(m)
	if err != nil {
		return err
	}
	return VerifyConversationManifest(context.Background(), h, m, func(_ context.Context, h ContentHash) ([]byte, error) { return bodies[h], nil })
}

const conversationVectorStream = `{"blocks":[{"text":"\u003cb\u003e\u0026` + "\uD55C\uAE00\U0001F642" + `\u2028\u2029","type":"text"}],"kind":"message","role":"user","seq":0},{"call_id":"x","input":{"n":9007199254740992},"kind":"tool_call","seq":1,"tool_name":"test"}`

// Independent vectors were produced with Python hashlib and sorted compact
// JSON, integer-preserving numeric parsing and explicit Go HTML/U+2028 escaping.
// These same fixtures execute in both independent modules.
func TestConversationManifestVectorsAndLegacyIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, stream, root, legacy string
		count                      int64
		size                       int
	}{
		{"empty", "", "sha256:ed840f598f733b77a265c201a5b47878c405243c373574972cf109f5a412b774", "sha256:aa26eee6bbb489caa2983098ec976b8e382105f7cc4c327d8706d4b4b696e643", 0, 336},
		{"HTML-Unicode-precision", conversationVectorStream, "sha256:0876308079ddda6c47a5560a9b19362ebe38cb14fc2677775ed2f3664a526640", "sha256:97749c221312030d2a65820a29577e15fd7ece80ddc75e813d5d948fd87c7e84", 2, 465},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := conversationTestEnvelope(t, tc.count != 0)
			m, bodies := conversationTestPlan(t, env, []byte(tc.stream), tc.count)
			raw, err := CanonicalConversationManifest(m)
			if err != nil || len(raw) != tc.size {
				t.Fatalf("canonical size=%d err=%v", len(raw), err)
			}
			h, err := ConversationManifestHash(m)
			if err != nil || string(h) != tc.root || h == HashContent(raw) {
				t.Fatalf("root=%s err=%v", h, err)
			}
			decoded, err := DecodeConversationManifest(raw)
			if err != nil || decoded.Chunks == nil || conversationTestVerify(decoded, bodies) != nil {
				t.Fatal("wire/full verification", err)
			}
			body := append([]byte(conversationDocPrefix), env...)
			body = append(body, conversationDocMiddle...)
			body = append(body, tc.stream...)
			body = append(body, conversationDocSuffix...)
			if string(HashContent(body)) != tc.legacy || HashContent(body) == h {
				t.Fatal("legacy hash changed or relabeled")
			}
			var doc CIRDocument
			if json.Unmarshal(body, &doc) != nil {
				t.Fatal("CIR fixture")
			}
			canonical, err := CanonicalBytes(doc)
			if err != nil || !bytes.Equal(canonical, body) || ValidateSessionDocHash(SessionDoc{Hash: HashContent(body), CIR: doc}) != nil {
				t.Fatal("legacy canonical/hash compatibility", err)
			}
			if ValidateSessionDocHash(SessionDoc{Hash: h, CIR: doc}) == nil {
				t.Fatal("manifest root accepted as legacy CIR identity")
			}
			built, builtBodies, err := ConversationManifestForCIR(doc)
			if err != nil {
				t.Fatal("CIR builder", err)
			}
			builtWire, _ := CanonicalConversationManifest(built)
			if !bytes.Equal(raw, builtWire) || conversationTestVerify(built, builtBodies) != nil {
				t.Fatal("CIR builder vector drift")
			}
		})
	}
}

func TestConversationManifestForCIROwnershipAndValidation(t *testing.T) {
	var doc CIRDocument
	raw := []byte(conversationDocPrefix + string(conversationTestEnvelope(t, true)) + conversationDocMiddle + conversationVectorStream + conversationDocSuffix)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Events[0], doc.Events[1] = doc.Events[1], doc.Events[0]
	built, bodies, err := ConversationManifestForCIR(doc)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := ConversationManifestHash(built)
	if hash != "sha256:0876308079ddda6c47a5560a9b19362ebe38cb14fc2677775ed2f3664a526640" || doc.Events[0].Seq != 1 {
		t.Fatal("builder must normalize ordering without mutating the input")
	}
	doc.Envelope.Cwd = "changed"
	doc.Events[0].Input["n"] = 0
	if err := conversationTestVerify(built, bodies); err != nil {
		t.Fatal("builder retained mutable input", err)
	}
	for name, mutate := range map[string]func(*CIRDocument){
		"implicit-legacy-version": func(d *CIRDocument) { d.Envelope.CIRVersion = "" },
		"unsupported-version":     func(d *CIRDocument) { d.Envelope.CIRVersion = "3" },
		"envelope-limit":          func(d *CIRDocument) { d.Envelope.Cwd = strings.Repeat("x", MaxConversationManifestBytes+1) },
		"event-union":             func(d *CIRDocument) { d.Events[0].Kind = "unknown" },
		"strict-number-precision": func(d *CIRDocument) { d.Events[1].Input["n"] = json.Number("9007199254740993") },
	} {
		t.Run(name, func(t *testing.T) {
			var candidate CIRDocument
			if err := json.Unmarshal(raw, &candidate); err != nil {
				t.Fatal(err)
			}
			mutate(&candidate)
			got, chunks, err := ConversationManifestForCIR(candidate)
			if !errors.Is(err, ErrConversationManifest) || got.Envelope != nil || chunks != nil {
				t.Fatalf("builder exposed invalid or partial output: %v", err)
			}
		})
	}
	doc.Events = nil
	doc.Envelope.CIRVersion = CIRVersionV1
	empty, chunks, err := ConversationManifestForCIR(doc)
	if err != nil || empty.Chunks == nil || chunks == nil || len(chunks) != 0 || empty.EventCount != 0 {
		t.Fatal("explicit v1 empty builder", err)
	}
}

func TestConversationManifestStrictWire(t *testing.T) {
	m, _ := conversationTestPlan(t, conversationTestEnvelope(t, false), []byte(`{"kind":"turn","role":"user","seq":0}`), 1)
	raw, _ := CanonicalConversationManifest(m)
	reject := func(name string, raw []byte) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConversationManifest(raw); !errors.Is(err, ErrConversationManifest) {
				t.Fatalf("accepted malformed manifest: %v", err)
			}
		})
	}
	for _, tc := range []struct{ name, old, new string }{
		{"duplicate", `"version":1`, `"version":1,"version":1`},
		{"unknown", `"version":1`, `"unknown":0,"version":1`},
		{"case", `"version":1`, `"Version":1`},
		{"fraction", `"version":1`, `"version":1.0`},
		{"exponent", `"stream_bytes":37`, `"stream_bytes":37e0`},
		{"unknown-envelope", `"cwd":`, `"unknown":0,"cwd":`},
		{"duplicate-envelope", `"cir_version":"2"`, `"cir_version":"2","cir_version":"2"`},
		{"duplicate-child", `"bytes":37`, `"bytes":37,"bytes":37`},
		{"null-array", `"chunks":[`, `"chunks":null,"ignored":[`},
		{"wrong-child-shape", `"bytes":37`, `"bytes":[]`},
		{"escaped-field", `"version"`, `"\u0076ersion"`},
		{"unpaired-surrogate", `"model"`, `"\ud800"`},
		{"object-reorder", `"event_count":1,"identity":"cxt-manifest-sha256-v1"`, `"identity":"cxt-manifest-sha256-v1","event_count":1`},
	} {
		candidate := bytes.Replace(raw, []byte(tc.old), []byte(tc.new), 1)
		if bytes.Equal(candidate, raw) {
			t.Fatalf("mutation did not match: %s in %s", tc.name, raw)
		}
		reject(tc.name, candidate)
	}
	reject("trailing-newline", append(bytes.Clone(raw), '\n'))
	reject("trailing-object", append(bytes.Clone(raw), []byte(`{}`)...))
	reject("leading-space", append([]byte{' '}, raw...))
	reject("invalid-UTF8", bytes.Replace(raw, []byte("model"), []byte{0xff}, 1))
	reject("oversize-before-decode", bytes.Repeat([]byte{' '}, MaxConversationManifestBytes+1))
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("fixture")
	}
	for key, value := range fields {
		delete(fields, key)
		missing, _ := canonicalJSON(fields)
		reject("missing-"+key, missing)
		fields[key] = json.RawMessage(`null`)
		null, _ := canonicalJSON(fields)
		reject("null-"+key, null)
		fields[key] = value
	}
	var envelope map[string]json.RawMessage
	json.Unmarshal(m.Envelope, &envelope)
	for key, value := range envelope {
		delete(envelope, key)
		fields["envelope"], _ = canonicalJSON(envelope)
		candidate, _ := canonicalJSON(fields)
		reject("missing-envelope-"+key, candidate)
		envelope[key] = json.RawMessage(`null`)
		fields["envelope"], _ = canonicalJSON(envelope)
		candidate, _ = canonicalJSON(fields)
		reject("null-envelope-"+key, candidate)
		envelope[key] = value
	}
}

func TestConversationManifestMetadataBoundsAndIdentity(t *testing.T) {
	env := conversationTestEnvelope(t, false)
	base, _ := conversationTestPlan(t, env, []byte(`{"kind":"turn","role":"user","seq":0}`), 1)
	for name, mutate := range map[string]func(*ConversationManifest){
		"version":          func(m *ConversationManifest) { m.Version++ },
		"identity":         func(m *ConversationManifest) { m.Identity = "cir-canonical-sha256-v1" },
		"format":           func(m *ConversationManifest) { m.ChunkFormat = "cxt-doc-chunks-v1" },
		"nil":              func(m *ConversationManifest) { m.Chunks = nil },
		"hash":             func(m *ConversationManifest) { m.Chunks[0].Hash = "sha256:BAD" },
		"zero":             func(m *ConversationManifest) { m.Chunks[0].Bytes = 0 },
		"negative":         func(m *ConversationManifest) { m.Chunks[0].Bytes = -1 },
		"overflow":         func(m *ConversationManifest) { m.Chunks[0].Bytes = math.MaxInt64 },
		"oversize-chunk":   func(m *ConversationManifest) { m.Chunks[0].Bytes = ConversationManifestChunkBytes + 1 },
		"short-nonfinal":   func(m *ConversationManifest) { m.Chunks = append(m.Chunks, m.Chunks[0]); m.StreamBytes *= 2 },
		"length-sum":       func(m *ConversationManifest) { m.StreamBytes++ },
		"negative-total":   func(m *ConversationManifest) { m.StreamBytes = -1 },
		"huge-count":       func(m *ConversationManifest) { m.EventCount = math.MaxInt64 },
		"negative-count":   func(m *ConversationManifest) { m.EventCount = -1 },
		"zero-count":       func(m *ConversationManifest) { m.EventCount = 0 },
		"unknown-CIR":      func(m *ConversationManifest) { m.Envelope = bytes.Replace(m.Envelope, []byte(`"2"`), []byte(`"3"`), 1) },
		"legacy-empty-CIR": func(m *ConversationManifest) { m.Envelope = bytes.Replace(m.Envelope, []byte(`"2"`), []byte(`""`), 1) },
		"manifest-limit": func(m *ConversationManifest) {
			m.Envelope = bytes.Replace(m.Envelope, []byte(`"model"`), []byte(`"`+strings.Repeat("a", MaxConversationManifestBytes-200)+`"`), 1)
		},
		"chunk-count-limit": func(m *ConversationManifest) {
			m.Chunks = make([]ConversationManifestChunk, MaxConversationManifestChunks+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := base
			m.Chunks = append([]ConversationManifestChunk{}, base.Chunks...)
			m.Envelope = bytes.Clone(base.Envelope)
			mutate(&m)
			if _, err := ConversationManifestHash(m); !errors.Is(err, ErrConversationManifest) {
				t.Fatalf("invalid metadata accepted: %v", err)
			}
		})
	}
	chunks := make([]ConversationManifestChunk, MaxConversationDocumentBytes/ConversationManifestChunkBytes)
	for i := range chunks {
		chunks[i] = ConversationManifestChunk{Hash: HashContent([]byte("claimed")), Bytes: ConversationManifestChunkBytes}
	}
	if _, err := NewConversationManifest(env, chunks, 1); err == nil {
		t.Fatal("document limit excluded framing/envelope")
	}
	chunks[len(chunks)-1].Bytes -= int64(conversationFrameBytes + len(env))
	if _, err := NewConversationManifest(env, chunks, 1); err != nil {
		t.Fatal("exact framed bound rejected", err)
	}
	if _, err := NewConversationManifest(env, []ConversationManifestChunk{{Bytes: math.MaxInt64}, {Bytes: math.MaxInt64}}, 1); err == nil {
		t.Fatal("constructor sum overflow")
	}
	largeEnv := bytes.Replace(env, []byte(`"model"`), append(append([]byte{'"'}, bytes.Repeat([]byte{'m'}, MaxConversationManifestBytes-len(env)-64+5)...), '"'), 1)
	if len(largeEnv) >= MaxConversationManifestBytes {
		t.Fatal("fixture must isolate encoded manifest limit")
	}
	if _, err := NewConversationManifest(largeEnv, nil, 0); err == nil {
		t.Fatal("encoded manifest overhead omitted from bound")
	}
	// Chunk order and every complete metadata value are hashed, not a set.
	one := ConversationManifestChunk{Hash: HashContent([]byte("one")), Bytes: ConversationManifestChunkBytes}
	two := ConversationManifestChunk{Hash: HashContent([]byte("two")), Bytes: ConversationManifestChunkBytes}
	m, _ := NewConversationManifest(env, []ConversationManifestChunk{one, two}, 1)
	h, _ := ConversationManifestHash(m)
	m.Chunks[0], m.Chunks[1] = m.Chunks[1], m.Chunks[0]
	reordered, _ := ConversationManifestHash(m)
	if h == reordered {
		t.Fatal("chunk ordering not hashed")
	}
	m.EventCount++
	counted, _ := ConversationManifestHash(m)
	if counted == reordered {
		t.Fatal("event count not hashed")
	}
	m.Chunks[1].Bytes--
	m.StreamBytes--
	length, _ := ConversationManifestHash(m)
	if length == counted {
		t.Fatal("lengths not hashed")
	}
	m.Envelope = bytes.Replace(m.Envelope, []byte(`"main"`), []byte(`"branch"`), 1)
	changed, _ := ConversationManifestHash(m)
	if changed == length {
		t.Fatal("envelope not hashed")
	}
}

func TestConversationManifestSemanticAttacks(t *testing.T) {
	env := conversationTestEnvelope(t, false)
	base := conversationVectorStream
	for _, tc := range []struct {
		name, stream string
		count        int64
	}{
		{"missing-comma", strings.Replace(base, "},{", "}{", 1), 2},
		{"extra-comma", strings.Replace(base, "},{", "},,{", 1), 2},
		{"trailing-comma", base + ",", 2},
		{"framing-prefix", "[" + base + "]", 2},
		{"framing-space", " " + base, 2},
		{"truncated", base[:len(base)-1], 2},
		{"duplicate", strings.Replace(base, `"seq":1`, `"seq":1,"seq":1`, 1), 2},
		{"nested-duplicate", strings.Replace(base, `"n":9007199254740992`, `"n":0,"n":9007199254740992`, 1), 2},
		{"unknown", strings.Replace(base, `"seq":1`, `"seq":1,"unknown":true`, 1), 2},
		{"case", strings.Replace(base, `"seq":1`, `"Seq":1`, 1), 2},
		{"count-too-small", base, 1}, {"count-too-large", base, 3},
		{"sequence-regression", strings.Replace(base, `"seq":1`, `"seq":-1`, 1), 2},
		{"reordered-events", "{" + strings.Split(base, "},{")[1] + "," + strings.Split(base, "},{")[0] + "}", 2},
		{"wrong-union", strings.Replace(base, `"kind":"tool_call"`, `"kind":"message"`, 1), 2},
		{"null-required", strings.Replace(base, `"role":"user"`, `"role":null`, 1), 2},
		{"precision-loss", strings.Replace(base, "9007199254740992", "9007199254740993", 1), 2},
		{"exponent", strings.Replace(base, "9007199254740992", "1e0", 1), 2},
		{"decimal", strings.Replace(base, "9007199254740992", "1.0", 1), 2},
		{"alternate-escape", strings.Replace(base, `\u003c`, "<", 1), 2},
		{"invalid-UTF8", strings.Replace(base, "\uD55C\uAE00", string([]byte{0xff}), 1), 2},
		{"unpaired-surrogate", strings.Replace(base, "\uD55C\uAE00", `\ud800`, 1), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, bodies := conversationTestPlan(t, env, []byte(tc.stream), tc.count)
			if !errors.Is(conversationTestVerify(m, bodies), ErrConversationManifest) {
				t.Fatal("hash-valid malformed CIR accepted")
			}
		})
	}
	for _, stream := range []string{
		strings.Replace(base, `"seq":1`, `"seq":0`, 1), // stable equal seq is valid
		`{"kind":"compaction","replacement":[],"replacement_complete":true,"seq":0}`,
		`{"kind":"compaction","replacement":[{"kind":"turn","role":"user","seq":0},{"kind":"turn","role":"assistant","seq":1}],"replacement_complete":true,"seq":0}`,
	} {
		count := int64(1)
		if strings.HasPrefix(stream, `{"blocks"`) {
			count = 2
		}
		m, bodies := conversationTestPlan(t, env, []byte(stream), count)
		if err := conversationTestVerify(m, bodies); err != nil {
			t.Fatal("valid semantic shape", err)
		}
		if strings.Contains(stream, `"replacement"`) {
			m.Envelope = bytes.Replace(env, []byte(`"2"`), []byte(`"1"`), 1)
			if conversationTestVerify(m, bodies) == nil {
				t.Fatal("v2 fields admitted under v1")
			}
		}
	}
}

func TestConversationManifestLargeEventsSeamsAndRepeatedReads(t *testing.T) {
	env := conversationTestEnvelope(t, false)
	prefix := `{"blocks":[{"text":"`
	suffix := `","type":"text"}],"kind":"message","role":"user","seq":0}`
	for _, seam := range []string{"\uD55C\uAE00\U0001F642", `\"quoted`, `\u003c`} {
		stream := []byte(prefix + strings.Repeat("a", ConversationManifestChunkBytes-len(prefix)-1) + seam + strings.Repeat("z", 2*ConversationManifestChunkBytes) + suffix)
		m, bodies := conversationTestPlan(t, env, stream, 1)
		if err := conversationTestVerify(m, bodies); err != nil {
			t.Fatalf("seam %q: %v", seam, err)
		}
	}
	stream := []byte(prefix + strings.Repeat("a", 3*ConversationManifestChunkBytes) + suffix)
	m, bodies := conversationTestPlan(t, env, stream, 1)
	if m.Chunks[1].Hash != m.Chunks[2].Hash {
		t.Fatal("fixture lacks repeated chunk")
	}
	var doc CIRDocument
	if err := json.Unmarshal([]byte(conversationDocPrefix+string(env)+conversationDocMiddle+string(stream)+conversationDocSuffix), &doc); err != nil {
		t.Fatal(err)
	}
	built, builtBodies, err := ConversationManifestForCIR(doc)
	if err != nil || len(built.Chunks) != len(m.Chunks) || len(builtBodies) != len(bodies) {
		t.Fatal("large-event builder occurrence accounting", err)
	}
	wire, _ := CanonicalConversationManifest(m)
	builtWire, _ := CanonicalConversationManifest(built)
	if !bytes.Equal(wire, builtWire) || conversationTestVerify(built, builtBodies) != nil {
		t.Fatal("large-event builder partition drift")
	}
	h, _ := ConversationManifestHash(m)
	calls := 0
	scratch := make([]byte, ConversationManifestChunkBytes)
	load := func(_ context.Context, h ContentHash) ([]byte, error) {
		calls++
		n := copy(scratch, bodies[h])
		return scratch[:n], nil
	}
	if err := VerifyConversationManifest(context.Background(), h, m, load); err != nil || calls != len(m.Chunks) {
		t.Fatal("loader reuse or occurrence omission", err, calls)
	}
	calls = 0
	if err := VerifyConversationManifest(context.Background(), h, m, func(ctx context.Context, h ContentHash) ([]byte, error) {
		b, _ := load(ctx, h)
		if calls == 3 {
			b[0] = 'x'
		}
		return b, nil
	}); err == nil {
		t.Fatal("repeat occurrence trusted by cached hash")
	}
	if calls != 3 {
		t.Fatal("did not check corrupt repeated occurrence", calls)
	}
	for _, mode := range []string{"wrong-length", "wrong-hash", "loader-error", "nil-loader", "wrong-root"} {
		t.Run(mode, func(t *testing.T) {
			marker := errors.New("loader failure")
			want := h
			calls := 0
			load := func(_ context.Context, h ContentHash) ([]byte, error) {
				calls++
				switch mode {
				case "wrong-length":
					return []byte{}, nil
				case "wrong-hash":
					return bytes.Repeat([]byte{'x'}, int(m.Chunks[0].Bytes)), nil
				case "loader-error":
					return nil, marker
				}
				return bodies[h], nil
			}
			if mode == "nil-loader" {
				load = nil
			}
			if mode == "wrong-root" {
				want = HashContent([]byte("other"))
			}
			err := VerifyConversationManifest(context.Background(), want, m, load)
			if err == nil {
				t.Fatal("bad byte/authority input accepted")
			}
			if mode == "loader-error" && !errors.Is(err, marker) {
				t.Fatal("loader cause lost", err)
			}
			if mode == "wrong-root" && calls != 0 {
				t.Fatal("loaded before validating root")
			}
		})
	}
}

type conversationCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	at, calls int
}

func (c *conversationCancelContext) Err() error {
	c.calls++
	if c.at > 0 && c.calls == c.at {
		c.cancel()
	}
	return c.Context.Err()
}

func TestConversationManifestOwnershipCancellationAndConcurrency(t *testing.T) {
	env := conversationTestEnvelope(t, false)
	m, bodies := conversationTestPlan(t, env, []byte(conversationVectorStream), 2)
	h, _ := ConversationManifestHash(m)
	before, _ := CanonicalConversationManifest(m)
	inputChunks := append([]ConversationManifestChunk{}, m.Chunks...)
	owned, err := NewConversationManifest(env, inputChunks, m.EventCount)
	if err != nil {
		t.Fatal(err)
	}
	inputChunks[0].Bytes++
	if raw, _ := CanonicalConversationManifest(owned); !bytes.Equal(raw, before) {
		t.Fatal("constructor chunk alias")
	}
	wire := bytes.Clone(before)
	decoded, err := DecodeConversationManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wire {
		wire[i] = '!'
	}
	if raw, _ := CanonicalConversationManifest(decoded); !bytes.Equal(raw, before) {
		t.Fatal("decoder input alias")
	}
	env[0] = '!'
	if after, _ := CanonicalConversationManifest(m); !bytes.Equal(after, before) {
		t.Fatal("constructor envelope alias")
	}
	load := func(_ context.Context, h ContentHash) ([]byte, error) { return bodies[h], nil }
	if err := VerifyConversationManifest(context.Background(), h, m, func(ctx context.Context, h ContentHash) ([]byte, error) {
		m.Envelope[0] = '!'
		m.Chunks[0].Hash = HashContent([]byte("changed"))
		return load(ctx, h)
	}); err != nil {
		t.Fatal("loader mutated captured metadata", err)
	}
	m, _ = DecodeConversationManifest(before)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifyConversationManifest(ctx, h, m, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("early cancellation", err)
	}
	probe := &conversationCancelContext{Context: context.Background()}
	if err := VerifyConversationManifest(probe, h, m, load); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{2, 4, probe.calls} {
		ctx, cancel := context.WithCancel(context.Background())
		observed := &conversationCancelContext{Context: ctx, cancel: cancel, at: at}
		err := VerifyConversationManifest(observed, h, m, load)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel at %d: %v", at, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				if err := VerifyConversationManifest(context.Background(), h, m, load); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestConversationManifestDepthIncludesOuterFrame(t *testing.T) {
	for _, depth := range []int{9996, 9997} {
		stream := []byte(`{"call_id":"x","input":{"n":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `},"kind":"tool_call","seq":0,"tool_name":"test"}`)
		m, bodies := conversationTestPlan(t, conversationTestEnvelope(t, false), stream, 1)
		err := conversationTestVerify(m, bodies)
		if (err == nil) != (depth == 9996) {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
}

func TestConversationManifestIndependentModuleParity(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Go tests run in the package directory, including builds using -trimpath.
	root := filepath.Clean(filepath.Join(dir, "..", "..", ".."))
	for _, name := range []string{"conversation_manifest.go", "conversation_manifest_build.go", "conversation_manifest_verify.go", "conversation_manifest_test.go"} {
		backend, err := os.ReadFile(filepath.Join(root, "backend", "internal", "domain", name))
		if err != nil {
			t.Fatal(err)
		}
		cli, err := os.ReadFile(filepath.Join(root, "cli", "internal", "domain", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(backend, cli) {
			t.Fatalf("independent algorithm/fixture drift: %s", name)
		}
	}
}

func FuzzConversationManifestDecode(f *testing.F) {
	m, _ := NewConversationManifest(conversationTestEnvelope(f, false), nil, 0)
	raw, _ := CanonicalConversationManifest(m)
	f.Add(raw)
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Add([]byte("null"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := DecodeConversationManifest(raw)
		if err != nil {
			return
		}
		canonical, err := CanonicalConversationManifest(m)
		if err != nil || !bytes.Equal(raw, canonical) {
			t.Fatal("accepted noncanonical input")
		}
		h, err := ConversationManifestHash(m)
		if err != nil || ValidateContentHash(h) != nil {
			t.Fatal("accepted unhashable manifest")
		}
		if m.StreamBytes == 0 {
			if err := VerifyConversationManifest(context.Background(), h, m, nil); err != nil {
				t.Fatal(err)
			}
		}
	})
}
