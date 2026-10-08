package capture_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type rootProjectionCodec struct {
	outbound.ProviderCodec
	full, append int
	afterDecode  func()
}

func (c *rootProjectionCodec) Decode(ctx context.Context, b []byte) (domain.CIRDocument, error) {
	c.full++
	d, e := c.ProviderCodec.Decode(ctx, b)
	if c.afterDecode != nil {
		c.afterDecode()
	}
	return d, e
}
func (c *rootProjectionCodec) DecodeAppend(ctx context.Context, b []byte, e domain.Envelope, n int) (domain.CIRDocument, error) {
	c.append++
	return c.ProviderCodec.(interface {
		DecodeAppend(context.Context, []byte, domain.Envelope, int) (domain.CIRDocument, error)
	}).DecodeAppend(ctx, b, e, n)
}

type rootProjectionStore struct {
	*storage.FileStore
	puts map[domain.ContentHash]int
}

func (s *rootProjectionStore) PutChunk(ctx context.Context, h domain.ContentHash, b []byte) error {
	s.puts[h]++
	return s.FileStore.PutChunk(ctx, h, b)
}

func TestRootProjectionRebuildsAndNeverReusesLegacyHint(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			path := filepath.Join(root, "synthetic.jsonl")
			st := storage.NewFileStore(root)
			svc := capture.NewSessionCapture(st)
			var source outbound.CaptureSource
			var base outbound.ProviderCodec
			var prefix, line string
			if provider == domain.ProviderClaude {
				source = capture.NewClaudeCapture()
				base = codec.NewClaudeCodec()
				line = `{"type":"user","sessionId":"synthetic","message":{"role":"user","content":"secret-one"}}` + "\n"
			} else {
				source = capture.NewCodexCapture()
				base = codec.NewCodexCodec()
				prefix = `{"type":"session_meta","payload":{"id":"synthetic"}}` + "\n"
				line = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"secret-one"}]}}` + "\n"
			}
			c := &rootProjectionCodec{ProviderCodec: base}
			write := func(raw string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := prefix + line
			write(raw)
			_, legacy, _, _, err := svc.Project(context.Background(), root, path, source, c, true, domain.DocumentIdentityLegacy)
			if err != nil {
				t.Fatal(err)
			}
			verify := func(raw string) {
				t.Helper()
				write(raw)
				c.full, c.append = 0, 0
				_, ref, n, _, err := svc.Project(context.Background(), root, path, source, c, true, domain.DocumentIdentityRootV1)
				if err != nil {
					t.Fatal(err)
				}
				if c.full != 1 || c.append != 0 || n != int64(len(raw)) || ref.Identity != domain.DocumentIdentityRootV1 || ref.Hash == legacy.Hash {
					t.Fatal("wrong capture path", ref, c.full, c.append, n)
				}
				clean, _ := capture.ScrubSecrets([]byte(raw), root)
				doc, err := base.Decode(context.Background(), clean)
				if err != nil {
					t.Fatal(err)
				}
				doc = capture.ScrubDoc(doc, root)
				manifest, _, err := domain.ConversationManifestForCIR(doc)
				if err != nil {
					t.Fatal(err)
				}
				want, err := domain.ConversationManifestHash(manifest)
				if err != nil || want != ref.Hash {
					t.Fatal(want, ref, err)
				}
				got, err := st.GetDocReference(context.Background(), ref)
				if err != nil || got.DocumentRef() != ref {
					t.Fatal(err)
				}
			}
			verify(raw)
			raw += line
			verify(raw)
			verify(raw)
			raw = strings.ReplaceAll(raw, "secret-one", "secret-two")
			verify(raw)
			if err := os.WriteFile(filepath.Join(root, ".cxtsecrets"), []byte("secret-two\n"), 0600); err != nil {
				t.Fatal(err)
			}
			verify(raw)
			raw = prefix + strings.ReplaceAll(line, "secret-one", "secret-two")
			verify(raw)
			write(raw + `{"type":`)
			if _, _, _, _, err := svc.Project(context.Background(), root, path, source, c, false, domain.DocumentIdentityRootV1); err == nil {
				t.Fatal("accepted incomplete explicit capture")
			}
			_, _, n, _, err := svc.Project(context.Background(), root, path, source, c, true, domain.DocumentIdentityRootV1)
			if err != nil || n != int64(len(raw)) {
				t.Fatal(n, err)
			}
			// Root hint cannot be supplied to the legacy append store on explicit opt-out.
			write(raw)
			c.full, c.append = 0, 0
			_, ref, _, _, err := svc.Project(context.Background(), root, path, source, c, true, domain.DocumentIdentityLegacy)
			if err != nil || ref.Identity != domain.DocumentIdentityLegacy || c.append != 1 {
				t.Fatal(ref, err)
			}
			if _, err := st.GetDoc(context.Background(), ref.Hash); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRootProjectionDeduplicatesAndRejectsCorruptCurrentChunk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "synthetic.jsonl")
	raw := `{"type":"user","sessionId":"synthetic","message":{"role":"user","content":"` + strings.Repeat("x", 3*domain.ConversationManifestChunkBytes) + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	st := &rootProjectionStore{FileStore: storage.NewFileStore(root), puts: map[domain.ContentHash]int{}}
	svc := capture.NewSessionCapture(st)
	run := func() error {
		_, _, _, _, err := svc.Project(context.Background(), root, path, capture.NewClaudeCapture(), codec.NewClaudeCodec(), false, domain.DocumentIdentityRootV1)
		return err
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	var corrupt domain.ContentHash
	mtimes := map[domain.ContentHash]int64{}
	for h, n := range st.puts {
		if n != 1 {
			t.Fatal("duplicate chunk staged twice", n)
		}
		p := filepath.Join(root, ".cxt", "objects", "chunks", strings.TrimPrefix(string(h), "sha256:"))
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		mtimes[h] = info.ModTime().UnixNano()
		corrupt = h
	}
	if len(st.puts) >= 4 {
		t.Fatal("fixture must contain duplicate chunk occurrences")
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	for h, stamp := range mtimes {
		info, err := os.Stat(filepath.Join(root, ".cxt", "objects", "chunks", strings.TrimPrefix(string(h), "sha256:")))
		if err != nil || info.ModTime().UnixNano() != stamp {
			t.Fatal("valid chunk was rewritten", err)
		}
	}
	p := filepath.Join(root, ".cxt", "objects", "chunks", strings.TrimPrefix(string(corrupt), "sha256:"))
	if err := os.WriteFile(p, []byte("corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("warm capture repaired or trusted corrupt chunk")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "corruption" {
		t.Fatal("capture repaired corruption")
	}
}

func TestRootProjectionPolicyRaceAndCancellationDoNotInstall(t *testing.T) {
	for _, mode := range []string{"policy", "cancel", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			path := filepath.Join(root, "synthetic.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"user","sessionId":"synthetic","message":{"role":"user","content":"secret"}}`+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			st := storage.NewFileStore(root)
			c := &rootProjectionCodec{ProviderCodec: codec.NewClaudeCodec()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			identity := domain.DocumentIdentityRootV1
			c.afterDecode = func() {
				if mode == "cancel" {
					cancel()
				} else if mode == "policy" {
					if err := os.WriteFile(filepath.Join(root, ".cxtsecrets"), []byte("secret\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "unknown" {
				identity = "future"
			}
			_, _, _, _, err := capture.NewSessionCapture(st).Project(ctx, root, path, capture.NewClaudeCapture(), c, false, identity)
			if err == nil {
				t.Fatal("capture accepted changed policy/cancellation/identity")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			docs, _ := filepath.Glob(filepath.Join(root, ".cxt", "objects", "docs", "*"))
			if len(docs) != 0 {
				t.Fatal("failed capture installed document")
			}
		})
	}
}
