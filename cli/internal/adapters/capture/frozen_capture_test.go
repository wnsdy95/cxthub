package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func frozenWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func frozenFixture(provider domain.ProviderKind) ([]byte, outbound.CaptureSource, outbound.ProviderCodec) {
	if provider == domain.ProviderClaude {
		return []byte(`{"type":"user","sessionId":"synthetic-frozen","message":{"role":"user","content":"private-secret captured"}}` + "\n"), NewClaudeCapture(), codec.NewClaudeCodec()
	}
	return []byte(`{"type":"session_meta","payload":{"id":"synthetic-frozen"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"private-secret captured"}]}}` + "\n"), NewCodexCapture(), codec.NewCodexCodec()
}

type frozenForbiddenSource struct{ outbound.CaptureSource }

func (f frozenForbiddenSource) ReadSession(context.Context, string) ([]byte, error) {
	return nil, errors.New("mutable native source was read")
}
func (f frozenForbiddenSource) IncrementalCapture() bool { return true }

type frozenCountingCodec struct {
	outbound.ProviderCodec
	full, appendCalls int
	after             func()
}

func (c *frozenCountingCodec) Decode(ctx context.Context, raw []byte) (domain.CIRDocument, error) {
	c.full++
	doc, err := c.ProviderCodec.Decode(ctx, raw)
	if c.after != nil {
		c.after()
	}
	return doc, err
}
func (c *frozenCountingCodec) DecodeAppend(context.Context, []byte, domain.Envelope, int) (domain.CIRDocument, error) {
	c.appendCalls++
	return domain.CIRDocument{}, errors.New("incremental decoder used")
}

func TestFrozenCaptureProjectsExactPrivateBytesAfterSourceChanges(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
			t.Run(provider+"/"+string(identity), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "native.jsonl")
				raw, source, base := frozenFixture(provider)
				frozenWrite(t, path, raw)
				frozenWrite(t, filepath.Join(root, SecretsFile), []byte("private-secret\n"))
				store := storage.NewFileStore(root)
				svc := NewSessionCapture(store)
				in, err := svc.Freeze(context.Background(), root, path, provider, identity)
				if err != nil || in.Hash != domain.HashContent(raw) || in.Size != int64(len(raw)) || in.Validate() != nil {
					t.Fatal("freeze", err)
				}
				if _, err := os.Stat(filepath.Join(root, ".cxt", "objects")); !os.IsNotExist(err) {
					t.Fatal("Freeze wrote public objects", err)
				}
				clean, _ := ScrubSecrets(raw, root)
				doc, err := base.Decode(context.Background(), clean)
				if err != nil {
					t.Fatal(err)
				}
				doc = ScrubDoc(doc, root)
				var want domain.ContentHash
				if identity == domain.DocumentIdentityLegacy {
					b, err := domain.CanonicalBytes(doc)
					if err != nil {
						t.Fatal(err)
					}
					want = domain.HashContent(b)
				} else {
					m, _, err := domain.ConversationManifestForCIR(doc)
					if err != nil {
						t.Fatal(err)
					}
					want, err = domain.ConversationManifestHash(m)
					if err != nil {
						t.Fatal(err)
					}
				}
				cdc := &frozenCountingCodec{ProviderCodec: base}
				for _, change := range []string{"append", "rewrite", "delete", "symlink"} {
					switch change {
					case "append":
						frozenWrite(t, path, append(append([]byte(nil), raw...), []byte("not frozen\n")...))
					case "rewrite":
						frozenWrite(t, path, []byte("rewritten"))
					case "delete":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					case "symlink":
						if err := os.Symlink(filepath.Join(root, "unread-missing"), path); err != nil {
							t.Fatal(err)
						}
					}
					_, ref, size, at, err := svc.ProjectFrozen(context.Background(), root, in, frozenForbiddenSource{source}, cdc)
					if err != nil || ref.Hash != want || ref.Identity != identity || size != in.Size || at == nil || !at.Equal(in.CapturedAt) {
						t.Fatal(change, "projection drift", err)
					}
					stored, err := store.GetDocReference(context.Background(), ref)
					if err != nil {
						t.Fatal(err)
					}
					b, err := domain.CanonicalBytes(stored.CIR)
					if err != nil || bytes.Contains(b, []byte("private-secret")) || !bytes.Contains(b, []byte(RedactedToken)) {
						t.Fatal("masking changed", err)
					}
				}
				if cdc.full != 4 || cdc.appendCalls != 0 {
					t.Fatal("projection trusted receipt or incremental path")
				}
			})
		}
	}
}

func TestFrozenCaptureChunkReuseBoundsAndPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "native.jsonl")
	raw := append(bytes.Repeat([]byte("x"), int(2*domain.FrozenCaptureChunkBytes)), []byte("tail")...)
	frozenWrite(t, path, raw)
	svc := NewSessionCapture(nil)
	in, err := svc.Freeze(context.Background(), root, path, domain.ProviderCodex, domain.DocumentIdentityLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Chunks) != 3 || in.Chunks[0] != in.Chunks[1] {
		t.Fatal("fixed chunk boundaries or dedup lost")
	}
	inputDir := filepath.Join(root, ".cxt", "capture", "inputs")
	entries, err := os.ReadDir(inputDir)
	if err != nil || len(entries) != 2 {
		t.Fatal("unexpected spool entries", err)
	}
	info, err := os.Stat(inputDir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("directory not private", err)
	}
	before := map[string]os.FileInfo{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("chunk not private", err)
		}
		before[entry.Name()] = info
	}
	if _, err := svc.Freeze(context.Background(), root, path, domain.ProviderCodex, domain.DocumentIdentityLegacy); err != nil {
		t.Fatal(err)
	}
	for name, old := range before {
		current, err := os.Stat(filepath.Join(inputDir, name))
		if err != nil || !os.SameFile(old, current) || !old.ModTime().Equal(current.ModTime()) {
			t.Fatal("valid chunk rewritten", err)
		}
	}
	// A sparse over-limit input is rejected before any large read/allocation.
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(domain.FrozenCaptureMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := svc.Freeze(context.Background(), root, path, domain.ProviderCodex, domain.DocumentIdentityLegacy); !errors.Is(err, domain.ErrFrozenCaptureInput) {
		t.Fatal("accepted oversized input", err)
	}
}

// Deterministically mutate the source once the first durable chunk is visible,
// at the next cooperative context check; no sleeps or production test hooks.
type frozenBoundaryContext struct {
	context.Context
	check func()
}

func (c frozenBoundaryContext) Err() error { c.check(); return c.Context.Err() }

func TestFrozenCaptureFreshPrefixRejectsMidCopyChanges(t *testing.T) {
	for _, mode := range []string{"append", "rewrite", "truncate", "rotate", "delete", "policy", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "native.jsonl")
			raw := bytes.Repeat([]byte("x"), int(domain.FrozenCaptureChunkBytes)+31)
			frozenWrite(t, path, raw)
			first := filepath.Join(root, ".cxt", "capture", "inputs", frozenChunkName(domain.HashContent(raw[:domain.FrozenCaptureChunkBytes])))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			changed := false
			boundary := frozenBoundaryContext{Context: ctx, check: func() {
				if changed {
					return
				}
				if _, err := os.Stat(first); err != nil {
					return
				}
				changed = true
				switch mode {
				case "append":
					f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.Write([]byte("growth"))
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
				case "rewrite":
					altered := bytes.Clone(raw)
					altered[0] = 'y'
					frozenWrite(t, path, altered)
				case "truncate":
					frozenWrite(t, path, []byte("short"))
				case "rotate":
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					frozenWrite(t, path, raw)
				case "delete":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "policy":
					frozenWrite(t, filepath.Join(root, SecretsFile), []byte("new-policy\n"))
				case "cancel":
					cancel()
				}
			}}
			in, err := NewSessionCapture(nil).Freeze(boundary, root, path, domain.ProviderClaude, domain.DocumentIdentityLegacy)
			if !changed {
				t.Fatal("mutation boundary not reached")
			}
			if mode == "append" {
				if err != nil || in.Size != int64(len(raw)) || in.Hash != domain.HashContent(raw) {
					t.Fatal("append changed frozen prefix", err)
				}
			} else if err == nil || in.Version != 0 {
				t.Fatal("accepted changed source/policy or cancellation")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestFrozenCaptureRejectsUnsafeOrCorruptCurrentSpool(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing", "symlink", "mode", "extra-bytes", "directory-symlink", "directory-mode", "hash"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "native.jsonl")
			raw, source, base := frozenFixture(domain.ProviderClaude)
			frozenWrite(t, path, raw)
			svc := NewSessionCapture(storage.NewFileStore(root))
			in, err := svc.Freeze(context.Background(), root, path, domain.ProviderClaude, domain.DocumentIdentityLegacy)
			if err != nil {
				t.Fatal(err)
			}
			cdc := &frozenCountingCodec{ProviderCodec: base}
			if _, _, _, _, err := svc.ProjectFrozen(context.Background(), root, in, source, cdc); err != nil {
				t.Fatal(err)
			}
			cdc.full = 0
			dir := filepath.Join(root, ".cxt", "capture", "inputs")
			chunk := filepath.Join(dir, frozenChunkName(in.Chunks[0].Hash))
			switch mode {
			case "corrupt":
				changed := bytes.Clone(raw)
				changed[0] ^= 1
				frozenWrite(t, chunk, changed)
			case "missing":
				if err := os.Remove(chunk); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(chunk, chunk+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(chunk+".original", chunk); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(chunk, 0644); err != nil {
					t.Fatal(err)
				}
			case "extra-bytes":
				frozenWrite(t, chunk, append(bytes.Clone(raw), 'x'))
			case "directory-symlink":
				if err := os.Rename(dir, dir+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+".original", dir); err != nil {
					t.Fatal(err)
				}
			case "directory-mode":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "hash":
				in.Hash = domain.HashContent([]byte("wrong"))
			}
			if _, ref, _, _, err := svc.ProjectFrozen(context.Background(), root, in, source, cdc); err == nil || ref.Hash != "" || cdc.full != 0 {
				t.Fatal("warm projection trusted corrupt input", err)
			}
			if mode == "corrupt" || mode == "symlink" || mode == "mode" || strings.HasPrefix(mode, "directory-") {
				if _, err := svc.Freeze(context.Background(), root, path, domain.ProviderClaude, domain.DocumentIdentityLegacy); err == nil {
					t.Fatal("Freeze repaired unsafe existing spool")
				}
			}
		})
	}
}

func TestFrozenCaptureProjectionPolicyAndCancelGates(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		for _, mode := range []string{"before-policy", "decode-policy", "decode-cancel", "provider", "codec-provider", "before-cancel"} {
			t.Run(string(identity)+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "native.jsonl")
				raw, source, base := frozenFixture(domain.ProviderClaude)
				frozenWrite(t, path, raw)
				svc := NewSessionCapture(storage.NewFileStore(root))
				in, err := svc.Freeze(context.Background(), root, path, domain.ProviderClaude, identity)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cdc := &frozenCountingCodec{ProviderCodec: base}
				switch mode {
				case "before-policy":
					frozenWrite(t, filepath.Join(root, SecretsFile), []byte("private-secret\n"))
				case "decode-policy":
					cdc.after = func() { frozenWrite(t, filepath.Join(root, SecretsFile), []byte("private-secret\n")) }
				case "decode-cancel":
					cdc.after = cancel
				case "before-cancel":
					cancel()
				case "provider":
					source = NewCodexCapture()
				case "codec-provider":
					cdc.ProviderCodec = codec.NewCodexCodec()
				}
				if _, ref, _, _, err := svc.ProjectFrozen(ctx, root, in, source, cdc); err == nil || ref.Hash != "" {
					t.Fatal("accepted failed projection", err)
				}
				if _, err := os.Stat(filepath.Join(root, ".cxt", "objects")); !os.IsNotExist(err) {
					t.Fatal("failure installed object before guard", err)
				}
			})
		}
	}
}

func TestFrozenCaptureSourceSymlinksAndCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "native.jsonl")
	frozenWrite(t, path, []byte("synthetic"))
	svc := NewSessionCapture(nil)
	if err := os.Symlink(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Freeze(context.Background(), root, path+".link", domain.ProviderClaude, domain.DocumentIdentityLegacy); err == nil {
		t.Fatal("source symlink accepted")
	}
	parent := filepath.Join(root, "linked")
	if err := os.Symlink(root, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Freeze(context.Background(), root, filepath.Join(parent, "native.jsonl"), domain.ProviderClaude, domain.DocumentIdentityLegacy); err == nil {
		t.Fatal("source parent symlink accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Freeze(ctx, root, path, domain.ProviderClaude, domain.DocumentIdentityLegacy); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("failed Freeze created spool", err)
	}
}

func TestFrozenCaptureConcurrentEquivalentBuilders(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "native.jsonl")
	raw := bytes.Repeat([]byte("synthetic"), 150000)
	frozenWrite(t, path, raw)
	var wg sync.WaitGroup
	inputs := make([]domain.FrozenCaptureInput, 2)
	errs := make([]error, 2)
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inputs[i], errs[i] = NewSessionCapture(nil).Freeze(context.Background(), root, path, domain.ProviderClaude, domain.DocumentIdentityLegacy)
		}(i)
	}
	wg.Wait()
	for i, in := range inputs {
		if errs[i] != nil || in.Hash != domain.HashContent(raw) {
			t.Fatal("concurrent freeze", errs[i])
		}
		got, err := readFrozenInput(context.Background(), root, in)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("concurrent durable bytes", err)
		}
	}
}

func TestFrozenCaptureIncompleteTailNeverProjects(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
			for _, suffix := range []string{`{"type":`, "{\"type\":\n", "{\"type\":\n  \n"} {
				t.Run(provider+"/"+string(identity)+"/"+suffix, func(t *testing.T) {
					root := t.TempDir()
					path := filepath.Join(root, "native.jsonl")
					raw, source, base := frozenFixture(provider)
					raw = append(raw, []byte(suffix)...)
					frozenWrite(t, path, raw)
					svc := NewSessionCapture(storage.NewFileStore(root))
					in, err := svc.Freeze(context.Background(), root, path, provider, identity)
					if err != nil || in.Size != int64(len(raw)) || in.Hash != domain.HashContent(raw) {
						t.Fatal("partial evidence was not retained exactly", err)
					}
					cdc := &frozenCountingCodec{ProviderCodec: base}
					if _, ref, _, _, err := svc.ProjectFrozen(context.Background(), root, in, source, cdc); err == nil || ref.Hash != "" || cdc.full != 0 {
						t.Fatal("partial tail reached decoder", err)
					}
					if _, err := os.Stat(filepath.Join(root, ".cxt", "objects")); !os.IsNotExist(err) {
						t.Fatal("partial input installed object", err)
					}
				})
			}
		}
	}
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		root := t.TempDir()
		path := filepath.Join(root, "native.jsonl")
		raw, source, base := frozenFixture(provider)
		raw = bytes.TrimSuffix(raw, []byte("\n"))
		frozenWrite(t, path, raw)
		svc := NewSessionCapture(storage.NewFileStore(root))
		in, err := svc.Freeze(context.Background(), root, path, provider, domain.DocumentIdentityRootV1)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, size, _, err := svc.ProjectFrozen(context.Background(), root, in, source, base); err != nil || size != int64(len(raw)) {
			t.Fatal("valid final record without newline rejected", err)
		}
	}
}

func TestFrozenCaptureOrderedChunksAndMissingRootCapability(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "native.jsonl")
	raw := append(bytes.Repeat([]byte("a"), int(domain.FrozenCaptureChunkBytes)), bytes.Repeat([]byte("b"), int(domain.FrozenCaptureChunkBytes))...)
	frozenWrite(t, path, raw)
	in, err := NewSessionCapture(nil).Freeze(context.Background(), root, path, domain.ProviderClaude, domain.DocumentIdentityRootV1)
	if err != nil {
		t.Fatal(err)
	}
	in.Chunks[0], in.Chunks[1] = in.Chunks[1], in.Chunks[0]
	if _, err := readFrozenInput(context.Background(), root, in); err == nil {
		t.Fatal("accepted reordered chunks with matching individual hashes")
	}
	// A legacy-only port wrapper must not accidentally acquire root support.
	native, source, base := frozenFixture(domain.ProviderClaude)
	frozenWrite(t, path, native)
	legacyOnly := struct{ outbound.SessionStore }{storage.NewFileStore(root)}
	svc := NewSessionCapture(legacyOnly)
	in, err = svc.Freeze(context.Background(), root, path, domain.ProviderClaude, domain.DocumentIdentityRootV1)
	if err != nil {
		t.Fatal(err)
	}
	cdc := &frozenCountingCodec{ProviderCodec: base}
	if _, _, _, _, err = svc.ProjectFrozen(context.Background(), root, in, source, cdc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || cdc.full != 0 {
		t.Fatal("missing root capability", err)
	}
}
