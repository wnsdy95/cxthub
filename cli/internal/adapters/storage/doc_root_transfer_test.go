package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type rootInstaller interface {
	PutConversationManifest(context.Context, domain.DocumentRepresentation) error
}

func rootTransferFixture(t *testing.T, text string) (domain.DocumentRepresentation, map[domain.ContentHash][]byte) {
	t.Helper()
	cir := sampleCIR(text)
	if text == "" {
		cir.Events = nil
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, bodies
}

func TestP4RootInstall(t *testing.T) {
	for _, name := range []string{"empty", "repeated", "missing", "corrupt", "canceled", "existing-corrupt"} {
		t.Run(name, func(t *testing.T) {
			s := NewFileStore(t.TempDir())
			installer, ok := any(s).(rootInstaller)
			if !ok {
				t.Fatal("explicit root installer is not implemented")
			}
			text := strings.Repeat("x", 3*domain.ConversationManifestChunkBytes)
			if name == "empty" {
				text = ""
			}
			rep, bodies := rootTransferFixture(t, text)
			ctx := context.Background()
			for hash, body := range bodies {
				if name != "missing" {
					if err := s.PutChunk(ctx, hash, body); err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "existing-corrupt" {
				if err := installer.PutConversationManifest(ctx, rep); err != nil {
					t.Fatal(err)
				}
			}
			if name == "corrupt" || name == "existing-corrupt" {
				for hash, body := range bodies {
					bad := bytes.Clone(body)
					bad[len(bad)-1] ^= 1
					inspectionWrite(t, s.objectPath("chunks", hash), docCompress(bad))
					break
				}
			}
			if name == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := installer.PutConversationManifest(ctx, rep)
			failed := name == "missing" || name == "corrupt" || name == "canceled" || name == "existing-corrupt"
			if (err != nil) != failed {
				t.Fatalf("installation: %v", err)
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if failed && name != "existing-corrupt" {
				if _, err := os.Stat(s.objectPath("docs", rep.Hash)); !os.IsNotExist(err) {
					t.Fatal("failed install exposed descriptor", err)
				}
			}
			if !failed {
				raw, err := readCxtFile(s.objectPath("docs", rep.Hash))
				if err != nil {
					t.Fatal(err)
				}
				got, err := docDecompress(raw)
				if err != nil || !bytes.Equal(got, rep.RootManifest) {
					t.Fatal("root disk is not exact bare manifest", err)
				}
				if err := s.VerifyStoredDocReference(ctx, rep.DocumentRef()); err != nil {
					t.Fatal(err)
				}
				if err := installer.PutConversationManifest(ctx, rep); err != nil {
					t.Fatal("valid replay failed", err)
				}
				if _, err := s.GetDoc(ctx, rep.Hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
					t.Fatal("legacy reader widened", err)
				}
			}
		})
	}
}

func TestP4RootDescriptorRetentionAndCurrentBytes(t *testing.T) {
	for _, empty := range []bool{false, true} {
		s := verificationStore(t)
		text := strings.Repeat("a", 3*domain.ConversationManifestChunkBytes)
		if empty {
			text = ""
		}
		rep, bodies := rootTransferFixture(t, text)
		for h, b := range bodies {
			if err := s.PutChunk(context.Background(), h, b); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.PutConversationManifest(context.Background(), rep); err != nil {
			t.Fatal(err)
		}
		var escaped func(context.Context, domain.ContentHash) ([]byte, error)
		supported, err := s.WithVerifiedDocChunks(context.Background(), rep.DocumentRef(), func(doc outbound.DocumentChunks) error {
			if doc.Representation.DocumentRef() != rep.DocumentRef() || !bytes.Equal(doc.Representation.RootManifest, rep.RootManifest) {
				t.Fatal("root descriptor relabeled")
			}
			if acquired, err := s.TryCollectObjects(context.Background(), func() error { t.Fatal("collector entered active callback"); return nil }); acquired || err != nil {
				t.Fatal(acquired, err)
			}
			escaped = doc.ReadChunk
			doc.Representation.RootManifest[0] = '!'
			for h, b := range bodies {
				got, err := doc.ReadChunk(context.Background(), h)
				if err != nil || !bytes.Equal(got, b) {
					t.Fatal("descriptor mutation changed loader", err)
				}
				bad := bytes.Clone(b)
				bad[0] ^= 1
				inspectionWrite(t, s.objectPath("chunks", h), docCompress(bad))
				if _, err := doc.ReadChunk(context.Background(), h); err == nil {
					t.Fatal("callback reused old current-byte proof")
				}
				break
			}
			return nil
		})
		if !supported || err != nil {
			t.Fatal(supported, err)
		}
		if _, err := escaped(context.Background(), domain.HashContent([]byte("not listed"))); err == nil {
			t.Fatal("loader survived callback")
		}
		if _, err := os.Stat(s.docReceiptPath(rep.Hash)); !os.IsNotExist(err) {
			t.Fatal("root receipt created", err)
		}
		if _, err := os.Stat(s.docProofKeyPath); !os.IsNotExist(err) {
			t.Fatal("root key created", err)
		}
	}
}

func TestP4RootInstallConcurrentAndInvalid(t *testing.T) {
	ctx := context.Background()
	rep, bodies := rootTransferFixture(t, "concurrent synthetic root")
	dir := t.TempDir()
	s := NewFileStore(dir)
	for h, b := range bodies {
		if err := s.PutChunk(ctx, h, b); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; done <- NewFileStore(dir).PutConversationManifest(ctx, rep) }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(s.objectPath("docs", rep.Hash))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"legacy", "wrong-hash", "mixed", "corrupt-manifest"} {
		t.Run(mode, func(t *testing.T) {
			bad := rep
			switch mode {
			case "legacy":
				bad.Identity = domain.DocumentIdentityLegacy
			case "wrong-hash":
				bad.Hash = domain.HashContent([]byte("wrong"))
			case "mixed":
				bad.Format = "cxt-chunks-v2"
			case "corrupt-manifest":
				bad.RootManifest = []byte(`{}`)
			}
			if err := s.PutConversationManifest(ctx, bad); err == nil {
				t.Fatal("invalid descriptor accepted")
			}
			after, err := os.ReadFile(s.objectPath("docs", rep.Hash))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("existing winner mutated", err)
			}
		})
	}
}
