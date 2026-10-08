package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Product writers stay closed. These fixtures use the exact shared canonical
// bare-manifest layout, including its domain-separated ID and existing chunks.
func seedRootDoc(t *testing.T, s *FileStore, cir domain.CIRDocument) (domain.DocumentRef, domain.ConversationManifest, map[domain.ContentHash][]byte) {
	t.Helper()
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for hash, body := range bodies {
		inspectionWrite(t, s.objectPath("chunks", hash), docCompress(body))
	}
	inspectionWrite(t, s.objectPath("docs", hash), docCompress(raw))
	return domain.DocumentRef{Hash: hash, Identity: domain.DocumentIdentityRootV1}, manifest, bodies
}

func TestRootDocumentReferenceReaders(t *testing.T) {
	ctx := context.Background()
	empty := sampleCIR("")
	empty.Events = nil
	for name, cir := range map[string]domain.CIRDocument{
		"empty":             empty,
		"unicode":           sampleCIR("\ud55c\uae00 \U0001f642 <tag> \\ \""),
		"seams-and-repeats": sampleCIR(strings.Repeat("a", 3*domain.ConversationManifestChunkBytes)),
	} {
		t.Run(name, func(t *testing.T) {
			s := verificationStore(t)
			ref, manifest, _ := seedRootDoc(t, s, cir)
			if name == "seams-and-repeats" && manifest.Chunks[1].Hash != manifest.Chunks[2].Hash {
				t.Fatal("fixture must contain repeated chunk occurrences")
			}
			before := inspectionDiskState(t, filepath.Join(s.storeDir(), "objects"))
			for _, reader := range []*FileStore{s, s, NewFileStore(s.repoRoot)} {
				if err := reader.VerifyStoredDocReference(ctx, ref); err != nil {
					t.Fatal(err)
				}
				doc, err := reader.GetDocReference(ctx, ref)
				if err != nil || doc.DocumentRef() != ref {
					t.Fatalf("reference read: %v %v", doc.DocumentRef(), err)
				}
				want, _ := domain.CanonicalBytes(cir)
				got, _ := domain.CanonicalBytes(doc.CIR)
				if !bytes.Equal(got, want) || domain.HashContent(got) == ref.Hash {
					t.Fatal("CIR changed or root was relabeled as a CIR hash")
				}
				if err := domain.VerifySessionDocIdentity(ctx, doc); err != nil {
					t.Fatal("shared full-CIR verifier disagrees", err)
				}
			}
			if got := inspectionDiskState(t, filepath.Join(s.storeDir(), "objects")); !reflect.DeepEqual(before, got) {
				t.Fatal("reader modified source objects")
			}
			for _, path := range []string{s.docReceiptPath(ref.Hash), s.docProofKeyPath} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("root read created receipt/key", err)
				}
			}
		})
	}
}

func TestRootDocumentLegacyBoundariesRemainClosed(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	ref, _, _ := seedRootDoc(t, s, sampleCIR("root"))
	for name, read := range map[string]func() error{
		"GetDoc":                   func() error { _, err := s.GetDoc(ctx, ref.Hash); return err },
		"VerifyDoc":                func() error { return s.VerifyDoc(ctx, ref.Hash) },
		"VerifyStoredDoc":          func() error { return s.VerifyStoredDoc(ctx, ref.Hash) },
		"wrong-expected-scheme":    func() error { return s.VerifyStoredDocReference(ctx, domain.DocumentRef{Hash: ref.Hash}) },
		"wrong-materialize-scheme": func() error { _, err := s.GetDocReference(ctx, domain.DocumentRef{Hash: ref.Hash}); return err },
		"upload": func() error {
			_, err := s.WithVerifiedDocChunks(ctx, ref.Hash, func(outbound.DocumentChunks) error { t.Fatal("root reached upload callback"); return nil })
			return err
		},
		"capture-append": func() error { _, err := s.AppendCaptureDoc(ctx, ref.Hash, sampleCIR("delta")); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := read(); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatalf("legacy boundary: %v", err)
			}
		})
	}
	legacy, err := s.PutDoc(ctx, domain.SessionDoc{CIR: sampleCIR("legacy")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDocReference(ctx, domain.DocumentRef{Hash: legacy}); err != nil {
		t.Fatal("explicit legacy reference rejected", err)
	}
	if _, err := s.GetDocReference(ctx, domain.DocumentRef{Hash: legacy, Identity: domain.DocumentIdentityRootV1}); err == nil {
		t.Fatal("legacy record accepted as root")
	}
	wrong := ref
	wrong.Hash = domain.HashContent([]byte("another root"))
	raw, _ := readCxtFile(s.objectPath("docs", ref.Hash))
	inspectionWrite(t, s.objectPath("docs", wrong.Hash), raw)
	if err := s.VerifyStoredDocReference(ctx, wrong); err == nil {
		t.Fatal("wrong root filename accepted")
	}
}

func TestRootDocumentPutRejectsBeforeEffects(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityRootV1, "future"} {
		for _, hash := range []domain.ContentHash{"", domain.HashContent([]byte("claimed"))} {
			s := NewFileStore(t.TempDir())
			doc := domain.SessionDoc{Hash: hash, Identity: identity, CIR: sampleCIR("not a root writer")}
			if _, err := s.PutDoc(context.Background(), doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("public root write did not fail closed", err)
			}
			if _, err := s.putDoc(doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("private root write did not fail closed", err)
			}
			if _, err := os.Stat(s.storeDir()); !os.IsNotExist(err) {
				t.Fatal("rejected write created replica files", err)
			}
		}
	}
}

func TestRootDocumentCurrentBytesAndNoReceiptTrust(t *testing.T) {
	ctx := context.Background()
	for _, damage := range []string{"same-length-tail", "missing-tail", "invalid-event-count", "oversized-chunk", "truncated-chunk"} {
		t.Run(damage, func(t *testing.T) {
			s := verificationStore(t)
			ref, manifest, bodies := seedRootDoc(t, s, bigDoc(16))
			if err := s.VerifyStoredDocReference(ctx, ref); err != nil {
				t.Fatal(err)
			}
			// Even a correctly MACed legacy receipt cannot stand for a root.
			key, err := s.docVerificationKey()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := readCxtFile(s.objectPath("docs", ref.Hash))
			proof := docVerificationProof{Version: docVerificationVersion, Doc: ref.Hash, Files: []docVerifiedFile{{Kind: "docs", ID: ref.Hash, Stored: domain.HashContent(raw)}}}
			inspectionWriteJSON(t, s.docReceiptPath(ref.Hash), docVerificationReceipt{Proof: proof, MAC: signDocProof(proof, key)})
			if s.matchesDocReceipt(ctx, ref.Hash, key) {
				t.Fatal("legacy receipt admitted root metadata without dependencies")
			}
			last := manifest.Chunks[len(manifest.Chunks)-1].Hash
			body := bytes.Clone(bodies[last])
			switch damage {
			case "same-length-tail":
				body[len(body)-2] ^= 1
			case "missing-tail":
				if err := os.Remove(s.objectPath("chunks", last)); err != nil {
					t.Fatal(err)
				}
			case "invalid-event-count":
				manifest.EventCount++
				ref.Hash, _ = domain.ConversationManifestHash(manifest)
				raw, _ := domain.CanonicalConversationManifest(manifest)
				inspectionWrite(t, s.objectPath("docs", ref.Hash), docCompress(raw))
			case "oversized-chunk":
				body = append(body, 'x')
			case "truncated-chunk":
				body = body[:len(body)-1]
			}
			if damage != "missing-tail" && damage != "invalid-event-count" {
				inspectionWrite(t, s.objectPath("chunks", last), docCompress(body))
			}
			for _, reader := range []*FileStore{s, NewFileStore(s.repoRoot)} {
				if err := reader.VerifyStoredDocReference(ctx, ref); err == nil {
					t.Fatal("verification reused stale root evidence")
				}
				if _, err := reader.GetDocReference(ctx, ref); err == nil {
					t.Fatal("materialization accepted damaged root")
				}
			}
		})
	}
}

func TestRootDocumentStrictBareFormatAndFallback(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	ref, manifest, _ := seedRootDoc(t, s, sampleCIR("strict"))
	good, _ := domain.CanonicalConversationManifest(manifest)
	wrapper, _ := json.Marshal(domain.DocumentRepresentation{Hash: ref.Hash, Identity: ref.Identity, RootManifest: good})
	identity := `"identity":"` + string(ref.Identity) + `"`
	for name, raw := range map[string][]byte{
		"wrapper":            wrapper,
		"missing-identity":   bytes.Replace(good, []byte(identity+","), nil, 1),
		"duplicate":          bytes.Replace(good, []byte(identity), []byte(identity+`,"identity":""`), 1),
		"case-alias":         bytes.Replace(good, []byte(`"identity"`), []byte(`"IDENTITY"`), 1),
		"escaped-key":        bytes.Replace(good, []byte(`"identity"`), []byte(`"ident\u0069ty"`), 1),
		"null":               bytes.Replace(good, []byte(identity), []byte(`"identity":null`), 1),
		"unknown-scheme":     bytes.Replace(good, []byte(identity), []byte(`"identity":"future"`), 1),
		"trailing":           append(bytes.Clone(good), []byte(` {}`)...),
		"whitespace":         append([]byte(" "), good...),
		"malformed":          []byte(`{"identity":"cxt-manifest-sha256-v1",`),
		"oversized-manifest": append(bytes.Clone(good), bytes.Repeat([]byte(" "), domain.MaxConversationManifestBytes)...),
	} {
		t.Run(name, func(t *testing.T) {
			inspectionWrite(t, s.objectPath("docs", ref.Hash), docCompress(raw))
			if _, err := s.GetDocReference(ctx, ref); err == nil {
				t.Fatal("noncanonical/ambiguous root accepted")
			}
			if _, err := s.GetDoc(ctx, ref.Hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("root declaration entered legacy fallback", err)
			}
		})
	}
	for _, raw := range []string{
		`{"events":[{"text":"identity: root_manifest"}],"envelope":{}}`,
		`{"envelope":{"identity":"nested"},"events":[]}`,
		`{"text":"identity"}`,
	} {
		root, err := rootDocumentDeclaration(ctx, strings.NewReader(raw))
		if err != nil || root {
			t.Fatal("legacy value mistaken for root declaration", err)
		}
	}
	// A late declaration cannot escape a short prefix probe or key-order sniff.
	late := `{"events":["` + strings.Repeat("x", 300<<10) + `"],"ident\u0069ty":"future"}`
	if root, err := rootDocumentDeclaration(ctx, strings.NewReader(late)); err != nil || !root {
		t.Fatal("late root declaration missed", err)
	}
}

func TestRootDocumentRetentionCancellationAndDecodeBounds(t *testing.T) {
	s := NewFileStore(t.TempDir())
	ref, manifest, bodies := seedRootDoc(t, s, sampleCIR("retained"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetDocReference(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.VerifyStoredDocReference(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, materialize := range []bool{false, true} {
		acquired, err := s.TryCollectObjects(context.Background(), func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			var err error
			if materialize {
				_, err = s.GetDocReference(ctx, ref)
			} else {
				err = s.VerifyStoredDocReference(ctx, ref)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("reader entered collection: %v", err)
			}
			return nil
		})
		if err != nil || !acquired {
			t.Fatal(acquired, err)
		}
	}
	if err := s.VerifyStoredDocReference(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if acquired, err := s.TryCollectObjects(context.Background(), func() error { return nil }); err != nil || !acquired {
		t.Fatal("read leaked retention", acquired, err)
	}
	// Streaming decompression never returns a limit+1 object as valid output.
	for _, compressed := range []bool{false, true} {
		raw := bytes.Repeat([]byte("x"), 2<<20)
		if compressed {
			raw = docCompress(raw)
		}
		if got, err := boundedRootData(context.Background(), bytes.NewReader(raw), 31); err == nil || got != nil {
			t.Fatal("oversized decoded output escaped", len(got), err)
		}
	}
	// Physical recompression is not a durable trust token and preserves identity.
	raw, _ := domain.CanonicalConversationManifest(manifest)
	inspectionWrite(t, s.objectPath("docs", ref.Hash), raw)
	for hash, body := range bodies {
		inspectionWrite(t, s.objectPath("chunks", hash), body)
	}
	if err := s.VerifyStoredDocReference(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	marker := errors.New("reader failure")
	if _, err := boundedRootData(context.Background(), failingRootReader{marker}, 31); !errors.Is(err, marker) {
		t.Fatal(err)
	}
}

type failingRootReader struct{ err error }

func (r failingRootReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = failingRootReader{}

func TestRootDocumentInspectionAndGC(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	ref, _, bodies := seedRootDoc(t, s, sampleCIR(strings.Repeat("a", 3*domain.ConversationManifestChunkBytes)))
	secondCIR := sampleCIR(strings.Repeat("a", 3*domain.ConversationManifestChunkBytes))
	secondCIR.Envelope.SessionOriginID = "another root sharing the complete stream"
	other, _, _ := seedRootDoc(t, s, secondCIR)
	legacy, err := s.PutDoc(ctx, domain.SessionDoc{CIR: secondCIR})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []domain.DocumentRef{ref, other, {Hash: legacy}} {
		inspectionWriteJSON(t, s.objectPath("snapshots", r.Hash), domain.Snapshot{ID: r.Hash, DocHash: r.Hash, DocIdentity: r.Identity, Branch: "main"})
	}
	before := inspectionDiskState(t, s.repoRoot)
	result := s.InspectReplica(ctx)
	if !result.Completed || result.DocumentsChecked != 3 || len(result.Issues) != 0 {
		t.Fatalf("inspect: %+v", result)
	}
	assertInspectionUnchanged(t, s, before)
	// A wrong-scheme snapshot is damage even when the token itself is unchanged.
	inspectionWriteJSON(t, s.objectPath("snapshots", ref.Hash), domain.Snapshot{ID: ref.Hash, DocHash: ref.Hash, Branch: "main"})
	if result := s.InspectReplica(ctx); !result.Completed || len(result.Issues) == 0 {
		t.Fatalf("wrong-scheme inspect: %+v", result)
	}
	if err := s.DeleteDoc(ctx, ref.Hash); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for hash := range bodies {
		if err := os.Chtimes(s.objectPath("chunks", hash), old, old); err != nil {
			t.Fatal(err)
		}
	}
	orphan := domain.HashContent([]byte("old orphan"))
	inspectionWrite(t, s.objectPath("chunks", orphan), docCompress([]byte("old orphan")))
	if err := os.Chtimes(s.objectPath("chunks", orphan), old, old); err != nil {
		t.Fatal(err)
	}
	rawBefore, _ := readCxtFile(s.objectPath("docs", other.Hash))
	if converted, _, err := s.RepackDocs(); err != nil || converted != 0 {
		t.Fatal(converted, err)
	}
	rawAfter, _ := readCxtFile(s.objectPath("docs", other.Hash))
	if !bytes.Equal(rawBefore, rawAfter) {
		t.Fatal("repack rewrote root manifest")
	}
	if err := s.VerifyStoredDocReference(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDoc(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if s.HasChunk(orphan) {
		t.Fatal("valid-root marking prevented ordinary orphan collection")
	}
	for _, malformed := range [][]byte{[]byte(`{"identity":"future"}`), []byte(`{"chunk_format":null}`), []byte(`{"unknown":true}`), {0x28, 0xb5, 0x2f, 0xfd, 0xff}} {
		inspectionWrite(t, s.objectPath("docs", other.Hash), malformed)
		inspectionWrite(t, s.objectPath("chunks", orphan), docCompress([]byte("old orphan")))
		if err := os.Chtimes(s.objectPath("chunks", orphan), old, old); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.RepackDocs(); err == nil {
			t.Fatal("unknown dependency closure did not stop sweep")
		}
		if !s.HasChunk(orphan) {
			t.Fatal("sweep ran despite unknown dependencies")
		}
	}
	inspectionWrite(t, s.objectPath("docs", other.Hash), rawBefore)
	unknown := []byte(`{"format":"future","chunks":[]}`)
	unknownPath := s.objectPath("docs", domain.HashContent(unknown))
	inspectionWrite(t, unknownPath, unknown)
	if _, _, err := s.RepackDocs(); err == nil || !s.HasChunk(orphan) {
		t.Fatal("self-hashed unknown object did not stop sweep", err)
	}
	if err := os.Remove(unknownPath); err != nil {
		t.Fatal(err)
	}
	// A directory or symlink at a valid document key is an unreadable object,
	// not permission to collect its former dependencies.
	path := s.objectPath("docs", other.Hash)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RepackDocs(); err == nil || !s.HasChunk(orphan) {
		t.Fatal("directory object did not stop sweep", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "descriptor")
	if err := os.WriteFile(target, rawBefore, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RepackDocs(); err == nil || !s.HasChunk(orphan) {
		t.Fatal("symlink object did not stop sweep", err)
	}
	if err := s.VerifyStoredDocReference(ctx, other); err == nil {
		t.Fatal("root reader followed a descriptor symlink")
	}
}

func TestRootDocumentGCRejectsUnknownCanonicalProjection(t *testing.T) {
	cir := sampleCIR("valid CIR inside a descriptor of unknown dependencies")
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(canonical)
	dependency := []byte("aged dependency that normalization must not discard")
	chunk := domain.HashContent(dependency)
	quoted, _ := json.Marshal(chunk)
	plan, ok := chunkcas.PlanDoc(canonical)
	if !ok {
		t.Fatal("fixture needs a chunked representation")
	}
	known, _ := json.Marshal(plan.Manifest)
	project := func(fields string, body []byte) []byte {
		return append([]byte("{"+fields+","), body[1:]...)
	}
	for name, raw := range map[string][]byte{
		"future-format":      project(`"format":"future","chunks":[`+string(quoted)+`]`, canonical),
		"unknown-version":    project(`"format":"cxt-doc-chunks-v999","chunks":[`+string(quoted)+`]`, canonical),
		"null-format":        project(`"format":null,"chunks":[`+string(quoted)+`]`, canonical),
		"case-alias":         project(`"FORMAT":"future","CHUNKS":[`+string(quoted)+`]`, canonical),
		"escaped-key":        project(`"f\u006frmat":"future","chunks":[`+string(quoted)+`]`, canonical),
		"chunks-only":        project(`"chunks":[`+string(quoted)+`]`, canonical),
		"unknown-field":      project(`"future_dependencies":[`+string(quoted)+`]`, canonical),
		"duplicate-format":   project(`"format":"future"`, known),
		"duplicate-chunks":   project(`"chunks":[`+string(quoted)+`]`, known),
		"known-with-unknown": project(`"future_dependencies":[`+string(quoted)+`]`, known),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewFileStore(t.TempDir())
			path := s.objectPath("docs", hash)
			stored := docCompress(raw)
			inspectionWrite(t, path, stored)
			for hash, body := range plan.Bodies {
				inspectionWrite(t, s.objectPath("chunks", hash), docCompress(body))
			}
			inspectionWrite(t, s.objectPath("chunks", chunk), docCompress(dependency))
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(s.objectPath("chunks", chunk), old, old); err != nil {
				t.Fatal(err)
			}
			converted, _, err := s.RepackDocs()
			after, readErr := readCxtFile(path)
			if err == nil || converted != 0 || readErr != nil || !bytes.Equal(after, stored) || !s.HasChunk(chunk) {
				t.Fatalf("unknown dependency set was rewritten or swept: converted=%d error=%v read=%v", converted, err, readErr)
			}
		})
	}
	t.Run("valid-noncanonical-legacy", func(t *testing.T) {
		s := NewFileStore(t.TempDir())
		indented, err := json.MarshalIndent(cir, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		indented = bytes.Replace(indented, []byte(`"envelope"`), []byte(`"ENVELOPE"`), 1)
		inspectionWrite(t, s.objectPath("docs", hash), docCompress(indented))
		if converted, _, err := s.RepackDocs(); err != nil || converted != 1 {
			t.Fatal("valid noncanonical legacy migration changed", converted, err)
		}
		if _, err := s.GetDoc(context.Background(), hash); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRootDocumentRepackPostConversionMarkFailure(t *testing.T) {
	for _, damage := range []string{"missing", "directory", "compressed", "json", "unknown", "invalid-chunk", "valid"} {
		t.Run(damage, func(t *testing.T) {
			s := NewFileStore(t.TempDir())
			canonical, err := domain.CanonicalBytes(sampleCIR("converted legacy fixture"))
			if err != nil {
				t.Fatal(err)
			}
			hash := domain.HashContent(canonical)
			if ok, _, err := s.putDocChunked(hash, canonical); err != nil || !ok {
				t.Fatal("fixture conversion failed", err)
			}
			path := s.objectPath("docs", hash)
			original, err := readCxtFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := docDecompress(original)
			manifest, _ := chunkcas.ParseManifest(data)
			switch damage {
			case "missing", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if damage == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "compressed":
				inspectionWrite(t, path, []byte{0x28, 0xb5, 0x2f, 0xfd, 0xff})
			case "json":
				inspectionWrite(t, path, docCompress([]byte(`{"format":`)))
			case "unknown":
				manifest.Format = "future"
				bad, _ := json.Marshal(manifest)
				inspectionWrite(t, path, docCompress(bad))
			case "invalid-chunk":
				manifest.Chunks = append(manifest.Chunks, "not-a-hash")
				bad, _ := json.Marshal(manifest)
				inspectionWrite(t, path, docCompress(bad))
			}
			previous := domain.HashContent([]byte("previously marked object"))
			live := map[domain.ContentHash]bool{previous: true}
			storedBytes, err := markRepackedDoc(path, live)
			if damage == "valid" {
				if err != nil || storedBytes != int64(len(original)) {
					t.Fatal("converted manifest was not marked", storedBytes, err)
				}
				for _, chunk := range manifest.Chunks {
					if !live[chunk] {
						t.Fatal("converted dependency omitted")
					}
				}
			} else {
				if err == nil || storedBytes != 0 || !reflect.DeepEqual(live, map[domain.ContentHash]bool{previous: true}) {
					t.Fatal("failed conversion readback admitted partial liveness/accounting", storedBytes, err, live)
				}
				if damage == "missing" && !errors.Is(err, os.ErrNotExist) {
					t.Fatal("readback failure cause lost", err)
				}
			}
			for _, chunk := range manifest.Chunks {
				if chunk != "not-a-hash" && !s.HasChunk(chunk) {
					t.Fatal("failed marking performed speculative cleanup")
				}
			}
		})
	}
}
