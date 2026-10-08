package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootReaderFixture struct {
	hash                     domain.ContentHash
	manifest                 domain.ConversationManifest
	manifestBytes, canonical []byte
	bodies                   map[domain.ContentHash][]byte
	cir                      domain.CIRDocument
}

func rootFixture(t *testing.T, text string) rootReaderFixture {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: t.Name()}}
	if text != "" {
		cir.Events = []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}
	}
	m, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return rootReaderFixture{h, m, raw, canonical, bodies, cir}
}

func seedRootFS(t *testing.T, st *FSStore, repo domain.ContentHash, f rootReaderFixture) {
	t.Helper()
	for h, body := range f.bodies {
		if err := writeAtomic(st.chunkPath(repo, h), docCompress(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAtomic(st.docPath(repo, f.hash), docCompress(f.manifestBytes)); err != nil {
		t.Fatal(err)
	}
}

func rootFSImage(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = string(domain.HashContent(raw)) + info.ModTime().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertRootProof(t *testing.T, doc domain.VerifiedSessionDoc, f rootReaderFixture) {
	t.Helper()
	want := domain.DocumentRef{Hash: f.hash, Identity: domain.DocumentIdentityRootV1}
	if !doc.Valid() || doc.DocumentRef() != want || !bytes.Equal(doc.Bytes(), f.canonical) {
		t.Fatal("root proof lost scheme, identity or current bytes")
	}
	if !doc.Reference().Matches(domain.Snapshot{ID: f.hash, DocHash: f.hash, DocIdentity: domain.DocumentIdentityRootV1}) || doc.Reference().Matches(domain.Snapshot{ID: f.hash, DocHash: f.hash}) {
		t.Fatal("root proof matched wrong-scheme snapshot")
	}
	m, ok := doc.ConversationManifest()
	if !ok || !reflect.DeepEqual(m, f.manifest) {
		t.Fatal("root manifest was not retained")
	}
	if _, ok := doc.ChunkPlan(); ok {
		t.Fatal("root exported a legacy chunk manifest")
	}
}

func TestRootFSReaderCurrentBytesAndScope(t *testing.T) {
	for _, tc := range []struct{ name, text string }{{"empty", ""}, {"unicode", "Unicode \ud55c\uad6d\uc5b4 <&> identity root_manifest"}, {"giant_repeated_chunks", strings.Repeat("x", 3*domain.ConversationManifestChunkBytes)}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			f := rootFixture(t, tc.text)
			seedRootFS(t, st, repo, f)
			before := rootFSImage(t, st.dataDir)
			for _, reader := range []*FSStore{st, st, NewFSStore(st.dataDir)} {
				doc, err := reader.ReadVerifiedDoc(ctx, repo, f.hash)
				if err != nil {
					t.Fatal(err)
				}
				assertRootProof(t, doc, f)
				ref, err := reader.VerifyStoredDoc(ctx, repo, f.hash)
				if err != nil || ref.DocumentRef() != doc.DocumentRef() {
					t.Fatal(ref, err)
				}
				if len(reader.docProofs.proofs) != 0 {
					t.Fatal("root admitted a legacy physical proof")
				}
			}
			if _, err := st.GetDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("hash-only materialization", err)
			}
			if _, err := st.GetDocManifest(ctx, repo, f.hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("legacy manifest fallback", err)
			}
			if supersedes, err := st.CaptureSupersedes(ctx, repo, f.hash, f.hash, domain.ProviderCodex, ""); err != nil || supersedes {
				t.Fatal("root capture must be retained", supersedes, err)
			}
			if got := rootFSImage(t, st.dataDir); !reflect.DeepEqual(before, got) {
				t.Fatal("read wrote source/index/receipt")
			}
			if _, err := st.ReadVerifiedDoc(ctx, domain.HashContent([]byte("foreign")), f.hash); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("foreign repository", err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if doc, err := st.ReadVerifiedDoc(cancelled, repo, f.hash); !errors.Is(err, context.Canceled) || doc.Valid() {
				t.Fatal("canceled reader", err)
			}
			for _, chunk := range f.manifest.Chunks {
				body := bytes.Clone(f.bodies[chunk.Hash])
				body[len(body)-1] ^= 1
				if err := writeAtomic(st.chunkPath(repo, chunk.Hash), docCompress(body)); err != nil {
					t.Fatal(err)
				}
				if doc, err := st.ReadVerifiedDoc(ctx, repo, f.hash); err == nil || doc.Valid() {
					t.Fatal("warm reader hid current chunk corruption")
				}
				if _, err := st.VerifyStoredDoc(ctx, repo, f.hash); err == nil {
					t.Fatal("warm reference hid current chunk corruption")
				}
				// The same logical body is valid in a different physical encoding.
				if err := writeAtomic(st.chunkPath(repo, chunk.Hash), f.bodies[chunk.Hash]); err != nil {
					t.Fatal(err)
				}
				if _, err := st.ReadVerifiedDoc(ctx, repo, f.hash); err != nil {
					t.Fatal("valid recompression", err)
				}
				if err := os.Remove(st.chunkPath(repo, chunk.Hash)); err != nil {
					t.Fatal(err)
				}
				if _, err := st.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("missing chunk", err)
				}
				if err := writeAtomic(st.chunkPath(repo, chunk.Hash), docCompress(f.bodies[chunk.Hash])); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeAtomic(st.docPath(repo, f.hash), bytes.ReplaceAll(f.manifestBytes, []byte(`"identity"`), []byte(`"Identity"`))); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ReadVerifiedDoc(ctx, repo, f.hash); err == nil {
				t.Fatal("corrupt manifest fell back")
			}
		})
	}
}

func TestRootReaderStrictDispatchAndBounds(t *testing.T) {
	f := rootFixture(t, "bounded")
	cases := map[string][]byte{
		"duplicate":                append([]byte(`{"identity":"cxt-manifest-sha256-v1",`), f.manifestBytes[1:]...),
		"alias":                    bytes.ReplaceAll(f.manifestBytes, []byte(`"identity"`), []byte(`"Identity"`)),
		"null":                     bytes.ReplaceAll(f.manifestBytes, []byte(`"identity":"cxt-manifest-sha256-v1"`), []byte(`"identity":null`)),
		"unknown":                  bytes.ReplaceAll(f.manifestBytes, []byte(`cxt-manifest-sha256-v1`), []byte(`future-root-v2`)),
		"missing_identity":         bytes.ReplaceAll(f.manifestBytes, []byte(`"identity":"cxt-manifest-sha256-v1",`), nil),
		"wrapper":                  []byte(`{"root_manifest":null,"events":[]}`),
		"escaped_alias":            []byte(`{"\u0069dentity":null}`),
		"unicode_fold_alias":       []byte(`{"root_manifeſt":null}`),
		"malformed":                []byte(`{"identity":`),
		"noncanonical":             append([]byte(" "), f.manifestBytes...),
		"over_limit":               append(bytes.Clone(f.manifestBytes), bytes.Repeat([]byte(" "), domain.MaxConversationManifestBytes)...),
		"late_declaration":         []byte(`{"padding":"` + strings.Repeat("x", domain.MaxConversationManifestBytes+1) + `","identity":null}`),
		"late_escaped_declaration": []byte(`{"padding":"` + strings.Repeat("x", domain.MaxConversationManifestBytes+1) + `","\u0069dentity":null}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			for _, stored := range [][]byte{raw, docCompress(raw)} {
				if _, root, err := storedConversationManifest(context.Background(), stored); !root || err == nil {
					t.Fatalf("root=%v err=%v", root, err)
				}
			}
			for _, step := range []int{1, 7, 31} {
				var scan rootDeclarationScanner
				for i := 0; i < len(raw); i += step {
					scan.scan(raw[i:min(i+step, len(raw))])
				}
				if !scan.root {
					t.Fatal("declaration lost across input seam", step)
				}
			}
		})
	}
	for _, raw := range [][]byte{[]byte(`{"envelope":{"identity":"ordinary"},"events":[{"input":{"root_manifest":null}}]}`), []byte(`{"events":[{"text":"\"identity\":null"}]}`), []byte(`{"version":"innocent legacy extension","events":[]}`)} {
		if _, root, err := storedConversationManifest(context.Background(), docCompress(raw)); root || err != nil {
			t.Fatal("nested marker selected root", root, err)
		}
	}
	for _, length := range []int64{1, domain.ConversationManifestChunkBytes} {
		body := bytes.Repeat([]byte("a"), int(length))
		if got, err := rootChunkBytes(context.Background(), docCompress(body), length); err != nil || !bytes.Equal(got, body) {
			t.Fatal(err)
		}
		for _, bad := range [][]byte{body[:len(body)-1], append(bytes.Clone(body), 'x')} {
			if _, err := rootChunkBytes(context.Background(), docCompress(bad), length); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("decoded length bound", err)
			}
		}
		compressed := docCompress(body)
		if _, err := rootChunkBytes(context.Background(), compressed[:len(compressed)-1], length); err == nil {
			t.Fatal("truncated compressed tail accepted")
		}
	}
}

func TestRootReaderRejectsHostileZstdWindow(t *testing.T) {
	f := rootFixture(t, "small actual body; hostile advertised history")
	// A valid raw-block frame with no declared content size. Its window byte
	// requests 512 MiB although the actual decoded body fits in a single block.
	frame := func(body []byte, window byte) []byte {
		header := len(body)<<3 | 1 // raw block, last block
		return append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, window, byte(header), byte(header >> 8), byte(header >> 16)}, body...)
	}
	for _, body := range [][]byte{f.manifestBytes, f.bodies[f.manifest.Chunks[0].Hash]} {
		if len(body) >= 128<<10 {
			t.Fatal("raw-block fixture too large")
		}
		// Preserve ordinary compressed representation semantics: an explicit
		// normal-size window is accepted independently of the framing choice.
		normal := frame(body, byte((23-10)<<3))
		decoded, err := rootChunkBytes(context.Background(), normal, int64(len(body)))
		if err != nil || !bytes.Equal(decoded, body) {
			t.Fatal("normal window rejected", err)
		}
		hostile := frame(body, byte((29-10)<<3))
		if _, err := rootChunkBytes(context.Background(), hostile, int64(len(body))); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("hostile chunk window", err)
		}
	}
	hostile := frame(f.manifestBytes, byte((29-10)<<3))
	if _, _, err := storedConversationManifest(context.Background(), hostile); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("hostile metadata window", err)
	}
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	seedRootFS(t, st, repo, f)
	if err := writeAtomic(st.docPath(repo, f.hash), hostile); err != nil {
		t.Fatal(err)
	}
	if doc, err := st.ReadVerifiedDoc(context.Background(), repo, f.hash); !errors.Is(err, domain.ErrIntegrity) || doc.Valid() {
		t.Fatal("hostile frame entered reader fallback", err)
	}
}

func TestRootFSProofCannotPublishLegacy(t *testing.T) {
	for _, text := range []string{"", "root"} {
		f := rootFixture(t, text)
		var verifier domain.CanonicalDocVerifier
		doc, err := verifier.VerifyConversationManifestDoc(context.Background(), f.hash, f.manifest, func(_ context.Context, h domain.ContentHash) ([]byte, error) { return f.bodies[h], nil })
		if err != nil {
			t.Fatal(err)
		}
		st := &FSStore{dataDir: filepath.Join(t.TempDir(), "must-not-exist")}
		repo := domain.HashContent([]byte(t.Name()))
		if _, err := st.PutVerifiedDoc(context.Background(), repo, doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if _, err := st.PutDoc(context.Background(), repo, domain.SessionDoc{Hash: f.hash, Identity: domain.DocumentIdentityRootV1, CIR: f.cir}); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal(err)
		}
		if _, err := st.PrepareDocJob(context.Background(), doc); err != nil {
			t.Fatal(err)
		}
		if err := st.CompleteDocJob(context.Background(), domain.DocFinalizationJob{}, doc, time.Now()); err == nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(st.dataDir); !os.IsNotExist(err) {
			t.Fatal("writer touched filesystem", err)
		}
		if len(st.docProofs.proofs) != 0 {
			t.Fatal("rejected root cached")
		}
	}
}

func TestRootReaderLegacyCompatibility(t *testing.T) {
	ctx := context.Background()
	f := rootFixture(t, "legacy body with root_manifest and identity as ordinary text")
	hash := domain.HashContent(f.canonical)
	for _, representation := range []string{"raw", "noncanonical_version_extension", "v1", "v2"} {
		t.Run(representation, func(t *testing.T) {
			st := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			stored := f.canonical
			if representation == "noncanonical_version_extension" {
				raw, err := json.MarshalIndent(f.cir, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				stored = append([]byte(`{"version":"old extension",`), raw[1:]...)
			}
			if representation == "v1" || representation == "v2" {
				var plan domain.DocChunkPlan
				var ok bool
				if representation == "v1" {
					plan, ok = domain.PlanDocChunksV1(f.canonical)
				} else {
					plan, ok = domain.PlanDocChunks(f.canonical)
				}
				if !ok {
					t.Fatal("legacy plan")
				}
				for h, body := range plan.Bodies {
					if err := writeAtomic(st.chunkPath(repo, h), docCompress(body)); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				stored, err = json.Marshal(plan.Manifest)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := writeAtomic(st.docPath(repo, hash), docCompress(stored)); err != nil {
				t.Fatal(err)
			}
			doc, err := st.ReadVerifiedDoc(ctx, repo, hash)
			if err != nil || doc.DocumentRef() != (domain.DocumentRef{Hash: hash}) || !bytes.Equal(doc.Bytes(), f.canonical) {
				t.Fatal("legacy identity changed", err)
			}
			if _, ok := doc.ConversationManifest(); ok {
				t.Fatal("legacy acquired root metadata")
			}
			if ref, err := st.VerifyStoredDoc(ctx, repo, hash); err != nil || ref.DocumentRef() != doc.DocumentRef() {
				t.Fatal(ref, err)
			}
		})
	}
}

func TestRootFSDependencyRetention(t *testing.T) {
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	f := rootFixture(t, strings.Repeat("x", 3*domain.ConversationManifestChunkBytes))
	seedRootFS(t, st, repo, f)
	other := f
	var env domain.CIREnvelope
	if err := json.Unmarshal(other.manifest.Envelope, &env); err != nil {
		t.Fatal(err)
	}
	env.SessionOriginID += "-second"
	cir := f.cir
	cir.Envelope = env
	var err error
	other.manifest, other.bodies, err = domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	other.hash, err = domain.ConversationManifestHash(other.manifest)
	if err != nil {
		t.Fatal(err)
	}
	other.manifestBytes, err = domain.CanonicalConversationManifest(other.manifest)
	if err != nil {
		t.Fatal(err)
	}
	seedRootFS(t, st, repo, other)
	if len(f.manifest.Chunks) <= len(f.bodies) {
		t.Fatal("fixture must repeat a chunk")
	}
	old := time.Now().Add(-time.Hour)
	for h := range f.bodies {
		if err := os.Chtimes(st.chunkPath(repo, h), old, old); err != nil {
			t.Fatal(err)
		}
	}
	orphan := domain.HashContent([]byte("orphan"))
	if err := writeAtomic(st.chunkPath(repo, orphan), docCompress([]byte("orphan"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(st.chunkPath(repo, orphan), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(st.docPath(repo, f.hash)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(st.docPath(repo, other.hash))
	if err != nil {
		t.Fatal(err)
	}
	if converted, _, err := st.RepackDocs(); err != nil || converted != 0 {
		t.Fatal("root repacked", converted, err)
	}
	after, err := os.ReadFile(st.docPath(repo, other.hash))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("manifest changed", err)
	}
	if _, err := st.ReadVerifiedDoc(context.Background(), repo, other.hash); err != nil {
		t.Fatal("shared dependencies collected", err)
	}
	if _, err := os.Stat(st.chunkPath(repo, orphan)); !os.IsNotExist(err) {
		t.Fatal("orphan not collected", err)
	}
	// Unknown or unreadable closure stops the destructive sweep, even when the
	// object's filename is the correct direct hash of its malformed bytes.
	for _, bad := range [][]byte{[]byte(`{"format":"future-manifest","chunks":[]}`), []byte(`{"identity":null}`), []byte("not JSON")} {
		h := domain.HashContent(bad)
		if err := writeAtomic(st.docPath(repo, h), docCompress(bad)); err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(st.chunkPath(repo, orphan), docCompress([]byte("orphan"))); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(st.chunkPath(repo, orphan), old, old); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.RepackDocs(); err == nil {
			t.Fatal("unknown closure did not abort sweep")
		}
		if _, err := os.Stat(st.chunkPath(repo, orphan)); err != nil {
			t.Fatal("failed closure swept chunks", err)
		}
		if err := os.Remove(st.docPath(repo, h)); err != nil {
			t.Fatal(err)
		}
	}
}

// Fixed, opt-in diagnostic: no calibration, scaling sweep or performance
// assertion. Measures just the extra current-descriptor classification pass.
func TestRootReaderLegacyClassificationCost(t *testing.T) {
	if os.Getenv("CXT_ROOT_CLASSIFY_MEASURE") != "1" {
		t.Skip("fixed 16 MiB legacy classifier measurement is opt-in")
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", 16<<20)}}}}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	compressed := docCompress(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("chunk fixture")
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		raw, stored []byte
	}{{"whole_raw", raw, raw}, {"whole_zstd", raw, compressed}, {"v2_descriptor_zstd", manifest, docCompress(manifest)}} {
		for sample := 1; sample <= 3; sample++ {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			_, root, err := storedConversationManifest(context.Background(), tc.stored)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			if err != nil || root {
				t.Fatal(tc.name, root, err)
			}
			line, err := json.Marshal(map[string]any{"case": tc.name, "sample": sample, "decoded_bytes": len(tc.raw), "stored_bytes": len(tc.stored), "elapsed_ns": elapsed.Nanoseconds(), "alloc_bytes": after.TotalAlloc - before.TotalAlloc, "gomaxprocs": runtime.GOMAXPROCS(0)})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("ROOT_CLASSIFY_SAMPLE %s", line)
		}
	}
}
