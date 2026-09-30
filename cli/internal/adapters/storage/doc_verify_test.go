package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func verificationV2Fixture(t testing.TB, envelope, events string) (*FileStore, domain.ContentHash, chunkcas.Manifest) {
	t.Helper()
	st := NewFileStore(t.TempDir())
	body := []byte(events)
	split := len(body) / 2
	parts := [][]byte{body[:split], body[split:]}
	man := chunkcas.Manifest{Format: chunkcas.FormatV2, Envelope: json.RawMessage(envelope)}
	for _, part := range parts {
		h := domain.HashContent(part)
		man.Chunks = append(man.Chunks, h)
		if err := writeAtomic(st.objectPath("chunks", h), docCompress(part)); err != nil {
			t.Fatal(err)
		}
	}
	cb := []byte(`{"envelope":` + envelope + `,"events":[` + events + `]}`)
	hash := domain.HashContent(cb)
	raw, _ := json.Marshal(man)
	if err := writeAtomic(st.objectPath("docs", hash), docCompress(raw)); err != nil {
		t.Fatal(err)
	}
	return st, hash, man
}

func assertSameDocValidation(t *testing.T, st *FileStore, hash domain.ContentHash) {
	t.Helper()
	_, expected := st.GetDoc(context.Background(), hash)
	var reuse inspectionEventReuse
	if raw, err := readCxtFile(st.objectPath("docs", hash)); err == nil {
		if body, err := docDecompress(raw); err == nil {
			if manifest, ok := chunkcas.ParseManifest(body); ok {
				reuse.observe(manifest.Chunks)
			}
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"seq":1}`} {
		reuse.proofs.remember(sha256.Sum256([]byte(raw)))
	}
	for _, actual := range []error{st.VerifyDoc(context.Background(), hash), st.verifyDoc(context.Background(), hash, &reuse)} {
		if (expected == nil) != (actual == nil) {
			t.Fatalf("validation changed: GetDoc=%v VerifyDoc=%v", expected, actual)
		}
		for _, kind := range []error{domain.ErrHashMismatch, domain.ErrInvalidCIR, domain.ErrNotFound} {
			if errors.Is(expected, kind) != errors.Is(actual, kind) {
				t.Fatalf("error precedence changed: GetDoc=%v VerifyDoc=%v", expected, actual)
			}
		}
	}
}

func TestVerifyDocV2MatchesGetDocTypedAndJSONValidation(t *testing.T) {
	for _, tc := range []struct{ name, envelope, events string }{
		{"unicode", `{"cir_version":"1"}`, `{"kind":"message","role":"user","seq":0,"blocks":[{"type":"text","text":"\uD55C\uAE00 🐈 \\ \""}]}`},
		{"multiple", `{}`, `{},null,{"seq":1}`},
		{"replacement", `{}`, `{"kind":"compaction","replacement":[{"seq":0},null],"replacement_complete":true}`},
		{"bad replacement", `{}`, `{"kind":"compaction","replacement":[{"seq":"invalid"}],"replacement_complete":true}`},
		{"bad integer", `{}`, `{"seq":"invalid"}`},
		{"bad envelope", `{"cir_version":42}`, `{}`},
		{"malformed JSON", `{}`, `{"seq":]}`},
		{"unknown root field", `{}`, `{}],"unknown":{"x":[1,2,3]},"events":[{}`},
		{"case folded duplicate", `{}`, `{}],"EVENTS":"wrong","events":[{}`},
		{"unicode folded duplicate", `{}`, `{}],"eventſ":"wrong","events":[{}`},
		{"trailing JSON", `{}`, `{}]} {"events":[{}`},
		{"large event", `{}`, `{"blocks":[{"type":"text","text":"` + strings.Repeat("a", 3<<20) + `"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, h, _ := verificationV2Fixture(t, tc.envelope, tc.events)
			assertSameDocValidation(t, st, h)
		})
	}
}

func TestVerifyDocV2RechecksCurrentBytesAndErrorPrecedence(t *testing.T) {
	for _, scenario := range []string{"corrupt bytes", "bad compression", "missing later chunk", "invalid chunk hash", "corrupt and malformed JSON", "malformed and missing later chunk"} {
		t.Run(scenario, func(t *testing.T) {
			events := `{"seq":0}`
			if scenario == "malformed and missing later chunk" {
				events = `{"seq":]}`
			}
			st, h, man := verificationV2Fixture(t, `{}`, events)
			if scenario != "malformed and missing later chunk" {
				if err := st.VerifyDoc(context.Background(), h); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "corrupt bytes", "corrupt and malformed JSON":
				replacement := []byte("bad")
				if scenario == "corrupt and malformed JSON" {
					replacement = []byte("{]")
				}
				if err := writeAtomic(st.objectPath("chunks", man.Chunks[0]), docCompress(replacement)); err != nil {
					t.Fatal(err)
				}
			case "bad compression":
				if err := writeAtomic(st.objectPath("chunks", man.Chunks[0]), []byte{'C', 'X', 'T', 'Z', 1, 2, 3}); err != nil {
					t.Fatal(err)
				}
			case "missing later chunk", "malformed and missing later chunk":
				if err := os.Remove(st.objectPath("chunks", man.Chunks[1])); err != nil {
					t.Fatal(err)
				}
			case "invalid chunk hash":
				man.Chunks[1] = "invalid"
				raw, _ := json.Marshal(man)
				if err := writeAtomic(st.objectPath("docs", h), docCompress(raw)); err != nil {
					t.Fatal(err)
				}
			}
			assertSameDocValidation(t, st, h)
			if err := st.VerifyDoc(context.Background(), h); err == nil {
				t.Fatal("current corruption accepted after prior successful verification")
			}
		})
	}
}

func TestVerifyDocLegacyAndV1PreserveCanonicalRules(t *testing.T) {
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "legacy"}}}}}
	cb, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	h := domain.HashContent(cb)
	for _, format := range []string{"canonical legacy", "noncanonical legacy", chunkcas.FormatV1} {
		t.Run(format, func(t *testing.T) {
			st := NewFileStore(t.TempDir())
			raw := cb
			if format == "noncanonical legacy" {
				var document any
				_ = json.Unmarshal(cb, &document)
				raw, _ = json.MarshalIndent(document, "", "  ")
			}
			if format == chunkcas.FormatV1 {
				plan, ok := chunkcas.PlanDocV1(cb)
				if !ok {
					t.Fatal("fixture not chunked")
				}
				for hash, body := range plan.Bodies {
					if err := writeAtomic(st.objectPath("chunks", hash), docCompress(body)); err != nil {
						t.Fatal(err)
					}
				}
				raw, _ = json.Marshal(plan.Manifest)
			}
			if err := writeAtomic(st.objectPath("docs", h), docCompress(raw)); err != nil {
				t.Fatal(err)
			}
			assertSameDocValidation(t, st, h)
			if err := st.VerifyDoc(context.Background(), h); err != nil {
				t.Fatal("valid legacy representation rejected")
			}
		})
	}
}

func TestVerifyDocPreCancelledDoesNotReadObjects(t *testing.T) {
	st := NewFileStore(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.VerifyDoc(ctx, domain.HashContent([]byte("absent"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestVerifyDocPreservesWholeDocumentJSONDepthLimit(t *testing.T) {
	for _, depth := range []int{9997, 9998, 9999} {
		nested := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
		for _, location := range []string{"event output", "unknown root"} {
			t.Run(fmt.Sprintf("%s/%d", location, depth), func(t *testing.T) {
				events := `{"kind":"tool_result","output":` + nested + `}`
				if location == "unknown root" {
					events = `{}],"unknown":{"value":` + nested + `},"events":[{}`
				}
				st, hash, _ := verificationV2Fixture(t, `{}`, events)
				assertSameDocValidation(t, st, hash)
			})
		}
	}
}

type cancelVerificationReader struct {
	input  *strings.Reader
	cancel context.CancelFunc
	bytes  int
}

func (r *cancelVerificationReader) Read(p []byte) (int, error) {
	if len(p) > 64 {
		p = p[:64]
	}
	n, err := r.input.Read(p)
	r.bytes += n
	if r.bytes >= 256 {
		r.cancel()
	}
	return n, err
}

func TestVerifyDocStreamingCancellationAtEventBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelVerificationReader{input: strings.NewReader(`{"envelope":{},"events":[` + strings.Repeat(`{"seq":1},`, 200) + `{}]}`), cancel: cancel}
	if err := verifyCIRStream(ctx, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream cancellation lost: %v", err)
	}
	if reader.input.Len() == 0 {
		t.Fatal("canceled parser consumed all subsequent events")
	}
}

func TestVerifyDocChunkReaderStopsBeforeNextChunkAfterCancellation(t *testing.T) {
	st, h, man := verificationV2Fixture(t, `{}`, `{"seq":1},{"seq":2}`)
	ctx, cancel := context.WithCancel(context.Background())
	r := &verificationChunkReader{ctx: ctx, store: st, doc: h, hashes: man.Chunks}
	p := make([]byte, 8)
	if _, err := r.Read(p); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.Read(p); !errors.Is(err, context.Canceled) {
		t.Fatalf("chunk cancellation lost: %v", err)
	}
	if r.next != 1 {
		t.Fatal("canceled reader accessed next chunk")
	}
}
