package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Build private synthetic evidence directly: no live provider files, provider
// discovery, codecs, stores or models participate in these tests.
func frozenMemoryInput(t *testing.T, root string, provider domain.ProviderKind, raw string) domain.FrozenCaptureInput {
	t.Helper()
	policy, err := ScrubPolicyFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	in := domain.FrozenCaptureInput{Version: domain.FrozenCaptureVersion, Provider: provider,
		SourcePath: filepath.Join(root, "never-read-native.jsonl"), Size: int64(len(raw)),
		Hash: domain.HashContent([]byte(raw)), CapturedAt: time.Now().UTC(), PolicyFingerprint: policy}
	dir, err := openFrozenDir(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for len(raw) > 0 {
		size := min(len(raw), int(domain.FrozenCaptureChunkBytes))
		part := []byte(raw[:size])
		chunk := domain.FrozenCaptureChunk{Hash: domain.HashContent(part), Size: int64(size)}
		if err := putFrozenChunk(context.Background(), dir, chunk, part); err != nil {
			t.Fatal(err)
		}
		in.Chunks = append(in.Chunks, chunk)
		raw = raw[size:]
	}
	return in
}

func frozenMemoryHeader(provider domain.ProviderKind, id string) string {
	if provider == domain.ProviderClaude {
		return `{"type":"user","sessionId":"` + id + `"}`
	}
	return `{"type":"session_meta","payload":{"id":"` + id + `"}}`
}

func TestFrozenSessionIDHeaderBounds(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			header := frozenMemoryHeader(provider, "synthetic-frozen")
			budget := int(domain.FrozenCaptureChunkBytes)
			for _, tc := range []struct {
				name, raw, want string
			}{
				{"first-line", header + "\n", "synthetic-frozen"},
				{"no-final-newline", header, "synthetic-frozen"},
				{"metadata-crlf", "{}\r\n\r\n" + header + "\r\n", "synthetic-frozen"},
				{"line-512", strings.Repeat("{}\n", 511) + header + "\n", "synthetic-frozen"},
				{"line-513", strings.Repeat("{}\n", 512) + header + "\n", ""},
				{"blank-lines-count", strings.Repeat("\n", 512) + header + "\n", ""},
				{"absent", "{}\n", ""},
				{"empty-id", frozenMemoryHeader(provider, "") + "\n", ""},
				{"exact-byte-bound", strings.Repeat(" ", budget-len(header)-1) + header + "\n", "synthetic-frozen"},
				{"next-chunk", strings.Repeat(" ", budget-1) + "\n" + header + "\n", ""},
				{"split-record", strings.Repeat(" ", budget-5) + header + "\n", ""},
				{"unread-malformed-tail", header + "\n{incomplete", "synthetic-frozen"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					root := t.TempDir()
					in := frozenMemoryInput(t, root, provider, tc.raw)
					in.SessionID = "untrusted-receipt-hint"
					// Future source bytes and filename identity must be irrelevant.
					frozenWrite(t, in.SourcePath, []byte(frozenMemoryHeader(provider, "future-session")))
					for _, chunk := range in.Chunks[1:] {
						if err := os.Remove(filepath.Join(root, ".cxt", "capture", "inputs", frozenChunkName(chunk.Hash))); err != nil {
							t.Fatal(err)
						}
					}
					got, err := NewSessionCapture(nil).FrozenSessionID(context.Background(), root, in)
					if err != nil || got != tc.want {
						t.Fatalf("identity = %q, %v; want %q", got, err, tc.want)
					}
					if err := os.Remove(in.SourcePath); err != nil {
						t.Fatal(err)
					}
					got, err = NewSessionCapture(nil).FrozenSessionID(context.Background(), root, in)
					if err != nil || got != tc.want {
						t.Fatalf("retry without source = %q, %v", got, err)
					}
				})
			}
		})
	}
}

func TestFrozenSessionIDMalformedEvidence(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		rows := []string{"{", "null", "[]", `{"unfinished":`, frozenMemoryHeader(provider, " leading"), frozenMemoryHeader(provider, `bad\nidentity`), frozenMemoryHeader(provider, strings.Repeat("x", 257))}
		if provider == domain.ProviderClaude {
			rows = append(rows, `{"sessionId":123}`, `{"sessionId":null}`, `{"sessionId":{}}`)
		} else {
			rows = append(rows, `{"type":123}`, `{"type":null}`, `{"type":"session_meta"}`, `{"type":"session_meta","payload":null}`, `{"type":"session_meta","payload":[]}`, `{"type":"session_meta","payload":{"id":123}}`, `{"type":"session_meta","payload":{"id":null}}`)
		}
		for _, raw := range rows {
			t.Run(provider+"/"+raw, func(t *testing.T) {
				root := t.TempDir()
				in := frozenMemoryInput(t, root, provider, raw)
				got, err := NewSessionCapture(nil).FrozenSessionID(context.Background(), root, in)
				if got != "" || !errors.Is(err, domain.ErrFrozenCaptureInput) {
					t.Fatalf("accepted malformed header: %q, %v", got, err)
				}
			})
		}
	}
}

func TestFrozenSessionIDLargeFirstRecord(t *testing.T) {
	large := strings.Repeat("x", int(domain.FrozenCaptureChunkBytes)+64)
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			prefix, field, suffix := `{`, "sessionId", "}"
			if provider == domain.ProviderCodex {
				prefix, field, suffix = `{"type":"session_meta","payload":{`, "id", "}}"
			}
			for _, tc := range []struct {
				name, body, want string
			}{
				{"early-id", `"` + field + `":"large-session","text":"` + large + `"`, "large-session"},
				{"escaped-id", `"` + field + `":"large\u002dsession","text":"` + large + `"`, "large-session"},
				{"nested-prefix", `"metadata":{"` + field + `":"wrong-nested-id","values":[1,true,null]},"` + field + `":"large-session","text":"` + large + `"`, "large-session"},
				{"id-after-budget", `"text":"` + large + `","` + field + `":"unavailable"`, ""},
				{"nested-id-only", `"metadata":{"` + field + `":"wrong-nested-id"},"text":"` + large + `"`, ""},
				{"id-crosses-budget", `"` + field + `":"` + large + `"`, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					root := t.TempDir()
					raw := prefix + tc.body + suffix + "\n"
					if !json.Valid([]byte(raw)) || len(raw) <= int(domain.FrozenCaptureChunkBytes) {
						t.Fatal("fixture must be a valid first record larger than the header budget")
					}
					in := frozenMemoryInput(t, root, provider, raw)
					// Neither a source file nor the remaining frozen chunks are
					// available: header discovery must stay within the first chunk.
					for _, chunk := range in.Chunks[1:] {
						if err := os.Remove(filepath.Join(root, ".cxt", "capture", "inputs", frozenChunkName(chunk.Hash))); err != nil {
							t.Fatal(err)
						}
					}
					got, err := NewSessionCapture(nil).FrozenSessionID(context.Background(), root, in)
					if err != nil || got != tc.want {
						t.Fatalf("large-record identity = %q, %v; want %q", got, err, tc.want)
					}
				})
			}
		})
	}
	for _, tc := range []struct{ name, prefix, suffix, want string }{
		{"payload-before-type", `{"payload":{"id":"large-session"},"type":"session_meta","text":"`, `"}`, "large-session"},
		{"type-after-budget", `{"payload":{"id":"unconfirmed","text":"`, `"},"type":"session_meta"}`, ""},
		{"non-session-payload", `{"type":"event_msg","payload":{"id":"not-a-session","text":"`, `"}}`, ""},
	} {
		t.Run("codex/"+tc.name, func(t *testing.T) {
			root := t.TempDir()
			raw := tc.prefix + large + tc.suffix + "\n"
			if !json.Valid([]byte(raw)) {
				t.Fatal("invalid Codex member-order fixture")
			}
			in := frozenMemoryInput(t, root, domain.ProviderCodex, raw)
			got, err := NewSessionCapture(nil).FrozenSessionID(context.Background(), root, in)
			if err != nil || got != tc.want {
				t.Fatalf("unverified Codex type/payload: %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestFrozenSessionIDLargeRecordRejectsMalformedClaim(t *testing.T) {
	large := strings.Repeat("x", int(domain.FrozenCaptureChunkBytes)+64)
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, value := range []string{`null`, `123`, `true`, `" leading"`, `"bad\nidentity"`, `"` + strings.Repeat("x", 257) + `"`, `{"text":"` + large + `"}`, `["` + large + `"]`} {
			t.Run(provider+"/"+string(domain.HashContent([]byte(value))), func(t *testing.T) {
				prefix, suffix := `{"sessionId":`, `}`
				if provider == domain.ProviderCodex {
					prefix, suffix = `{"type":"session_meta","payload":{"id":`, `}}`
				}
				root := t.TempDir()
				in := frozenMemoryInput(t, root, provider, prefix+value+`,"text":"`+large+`"`+suffix+"\n")
				got, err := NewSessionCapture(nil).FrozenSessionID(context.Background(), root, in)
				if got != "" || !errors.Is(err, domain.ErrFrozenCaptureInput) {
					t.Fatalf("malformed claimed ID became identity/absence: %q, %v", got, err)
				}
			})
		}
	}
}

func TestFrozenSessionIDLargeHeaderStillRequiresFullProjection(t *testing.T) {
	large := strings.Repeat("x", int(domain.FrozenCaptureChunkBytes)+64)
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, mode := range []string{"valid", "corrupt-later-chunk", "malformed-record"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				raw := `{"type":"user","sessionId":"large-session","message":{"role":"user","content":"` + large + `"}}`
				if provider == domain.ProviderCodex {
					raw = `{"type":"session_meta","payload":{"id":"large-session","text":"` + large + `"}}`
				}
				if mode == "malformed-record" {
					raw = strings.TrimSuffix(raw, "}")
				}
				in := frozenMemoryInput(t, root, provider, raw+"\n")
				if mode == "corrupt-later-chunk" {
					chunk := in.Chunks[len(in.Chunks)-1]
					frozenWrite(t, filepath.Join(root, ".cxt", "capture", "inputs", frozenChunkName(chunk.Hash)), bytes.Repeat([]byte{'!'}, int(chunk.Size)))
				}
				svc := NewSessionCapture(storage.NewFileStore(root))
				got, err := svc.FrozenSessionID(context.Background(), root, in)
				if err != nil || got != "large-session" {
					t.Fatalf("bounded early identity = %q, %v", got, err)
				}
				_, source, codec := frozenFixture(provider)
				_, _, _, _, err = svc.ProjectFrozen(context.Background(), root, in, frozenForbiddenSource{source}, codec)
				if mode == "valid" && err != nil {
					t.Fatal("valid large transcript did not project", err)
				} else if mode != "valid" && err == nil {
					t.Fatal("early identity bypassed full frozen verification")
				}
			})
		}
	}
}

func TestFrozenSessionIDVerifiesWholeHeaderChunk(t *testing.T) {
	for _, mode := range []string{"corrupt-after-header", "missing", "public", "symlink", "input-hash", "invalid-input", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			raw := frozenMemoryHeader(domain.ProviderCodex, "synthetic-frozen") + "\n{}\n"
			in := frozenMemoryInput(t, root, domain.ProviderCodex, raw)
			path := filepath.Join(root, ".cxt", "capture", "inputs", frozenChunkName(in.Chunks[0].Hash))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "corrupt-after-header":
				frozenWrite(t, path, []byte(raw[:len(raw)-1]+"x"))
			case "missing", "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if mode == "symlink" {
					frozenWrite(t, in.SourcePath, []byte(raw))
					if err := os.Symlink(in.SourcePath, path); err != nil {
						t.Fatal(err)
					}
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "input-hash":
				in.Hash = domain.HashContent([]byte("wrong"))
			case "invalid-input":
				in.Chunks = nil
			case "cancelled":
				cancel()
			}
			got, err := NewSessionCapture(nil).FrozenSessionID(ctx, root, in)
			if got != "" || err == nil {
				t.Fatalf("accepted unverified header: %q, %v", got, err)
			}
		})
	}
}

func TestScrubFrozenMemoryCopiesPrivateReceipt(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, tier := range []ScrubTier{"", ScrubStandard, ScrubStrict, ScrubOff, "unknown"} {
			t.Run(provider+"/"+string(tier), func(t *testing.T) {
				old := LoadScrubOptions
				LoadScrubOptions = func(string) ScrubOptions { return ScrubOptions{Tier: tier} }
				t.Cleanup(func() { LoadScrubOptions = old })
				root := t.TempDir()
				frozenWrite(t, filepath.Join(root, SecretsFile), []byte("private-secret\n"))
				in := frozenMemoryInput(t, root, provider, "{}\n")
				// Scrubbing must not process the native corpus or read provider files.
				if err := os.RemoveAll(filepath.Join(root, ".cxt", "capture", "inputs")); err != nil {
					t.Fatal(err)
				}
				text := "private-secret sk-123456789012345678901234 TOKEN=secretvalue"
				in.SessionID = "synthetic-frozen"
				in.NativeMemory = &domain.NativeMemory{Provider: provider, Source: provider + ":synthetic", Scope: domain.NativeMemoryScopeSession,
					Text: text, AutoLoadedPrefix: text, Structured: map[string]string{"summary": text, "empty": ""}}
				before, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				got, err := NewSessionCapture(nil).ScrubFrozenMemory(context.Background(), root, in)
				if err != nil || got == nil || got == in.NativeMemory {
					t.Fatalf("scrub = %v, %v", got, err)
				}
				want := RedactedToken + " sk-123456789012345678901234 TOKEN=secretvalue"
				if tier != ScrubOff {
					want = RedactedToken + " «redacted:api-key» TOKEN=secretvalue"
				}
				if tier == ScrubStrict {
					want = RedactedToken + " «redacted:api-key» TOKEN=«redacted:env»"
				}
				if got.Text != want || got.AutoLoadedPrefix != want || got.Structured["summary"] != want || got.Structured["empty"] != "" || got.Provider != provider || got.Source != in.NativeMemory.Source || got.Scope != in.NativeMemory.Scope {
					t.Fatal("memory fields not sanitized/preserved under pinned policy")
				}
				got.Structured["summary"] = "mutated copy"
				after, err := json.Marshal(in)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("private receipt mutated", err)
				}
				var decoded domain.FrozenCaptureInput
				if err := json.Unmarshal(before, &decoded); err != nil || decoded.Validate() != nil || !reflect.DeepEqual(decoded.NativeMemory, in.NativeMemory) || decoded.SessionID != in.SessionID {
					t.Fatal("private receipt JSON round trip", err)
				}
			})
		}
	}
}

func TestScrubFrozenMemoryPolicyAndAbsence(t *testing.T) {
	for _, absent := range []bool{false, true} {
		for _, mode := range []string{"unchanged", "secrets-drift", "tier-drift", "cancelled", "invalid-input"} {
			t.Run(mode+"/"+map[bool]string{true: "absent", false: "present"}[absent], func(t *testing.T) {
				old := LoadScrubOptions
				t.Cleanup(func() { LoadScrubOptions = old })
				root := t.TempDir()
				in := frozenMemoryInput(t, root, domain.ProviderClaude, "{}\n")
				if !absent {
					in.NativeMemory = &domain.NativeMemory{Provider: in.Provider, Text: "frozen memory"}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch mode {
				case "secrets-drift":
					frozenWrite(t, filepath.Join(root, SecretsFile), []byte("changed-policy\n"))
				case "tier-drift":
					LoadScrubOptions = func(string) ScrubOptions { return ScrubOptions{Tier: ScrubStrict} }
				case "cancelled":
					cancel()
				case "invalid-input":
					in.Version++
				}
				got, err := NewSessionCapture(nil).ScrubFrozenMemory(ctx, root, in)
				if mode != "unchanged" {
					if got != nil || err == nil {
						t.Fatal("invalid receipt or drift returned memory")
					}
					return
				}
				if err != nil || (got == nil) != absent {
					t.Fatalf("absence changed: %v, %v", got, err)
				}
				if absent {
					b, err := json.Marshal(in)
					if err != nil || !bytes.Contains(b, []byte(`"native_memory":null`)) {
						t.Fatal("absence not explicit in private receipt", err)
					}
				}
			})
		}
	}
}

func TestScrubFrozenMemoryRejectsMidScrubPolicyDrift(t *testing.T) {
	root := t.TempDir()
	in := frozenMemoryInput(t, root, domain.ProviderClaude, "{}\n")
	in.NativeMemory = &domain.NativeMemory{Provider: in.Provider, Text: "private-secret"}
	old := LoadScrubOptions
	t.Cleanup(func() { LoadScrubOptions = old })
	calls := 0
	LoadScrubOptions = func(string) ScrubOptions {
		calls++
		if calls == 2 {
			frozenWrite(t, filepath.Join(root, SecretsFile), []byte("private-secret\n"))
		}
		return ScrubOptions{}
	}
	got, err := NewSessionCapture(nil).ScrubFrozenMemory(context.Background(), root, in)
	if err == nil || got != nil || calls < 3 {
		t.Fatal("mid-scrub policy drift returned memory", err)
	}
}

func TestFrozenMemoryReceiptValidationBounds(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			in := frozenMemoryInput(t, root, provider, "{}\n")
			in.NativeMemory = &domain.NativeMemory{Provider: provider}
			base, err := json.Marshal(in.NativeMemory)
			if err != nil {
				t.Fatal(err)
			}
			in.NativeMemory.Text = strings.Repeat("x", domain.FrozenCaptureMemoryMaxBytes-len(base))
			if err := in.Validate(); err != nil {
				t.Fatal("rejected exact 8 MiB encoded memory", err)
			}
			in.NativeMemory.Text += "x"
			if !errors.Is(in.Validate(), domain.ErrFrozenCaptureInput) {
				t.Fatal("accepted memory JSON above 8 MiB")
			}
			for name, native := range map[string]*domain.NativeMemory{
				"provider-mismatch": {Provider: "other"},
				"missing-provider":  {},
				"text":              {Provider: provider, Text: strings.Repeat("x", domain.FrozenCaptureMemoryMaxBytes+1)},
				"escaped-text":      {Provider: provider, Text: strings.Repeat("<", domain.FrozenCaptureMemoryMaxBytes/6)},
				"prefix":            {Provider: provider, AutoLoadedPrefix: strings.Repeat("x", domain.FrozenCaptureMemoryMaxBytes)},
				"structured-value":  {Provider: provider, Structured: map[string]string{"key": strings.Repeat("x", domain.FrozenCaptureMemoryMaxBytes)}},
				"structured-key":    {Provider: provider, Structured: map[string]string{strings.Repeat("x", domain.FrozenCaptureMemoryMaxBytes): "value"}},
				"source":            {Provider: provider, Source: strings.Repeat("x", domain.FrozenCaptureMemoryMaxBytes)},
			} {
				t.Run(name, func(t *testing.T) {
					in.NativeMemory = native
					if !errors.Is(in.Validate(), domain.ErrFrozenCaptureInput) {
						t.Fatal("accepted invalid memory receipt")
					}
					got, err := NewSessionCapture(nil).ScrubFrozenMemory(context.Background(), root, in)
					if got != nil || !errors.Is(err, domain.ErrFrozenCaptureInput) {
						t.Fatal("scrub accepted invalid memory receipt", err)
					}
				})
			}
		})
	}
}
