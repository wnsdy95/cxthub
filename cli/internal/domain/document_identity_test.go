package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func identityTestCIR(t *testing.T, text string) CIRDocument {
	t.Helper()
	var doc CIRDocument
	const raw = `{"envelope":{"captured_at":"2026-10-07T00:00:00Z","cir_version":"2","cwd":"/tmp/` + "\uD55C\uAE00" + `","fidelity":"full","git_branch":"main","session_origin_id":"synthetic","source_model":"model","source_provider":"codex"},"events":[]}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if text != "" {
		doc.Events = []Event{{Seq: 0, Kind: EventMessage, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: text}}}}
	}
	return doc
}

func identityTestRoot(t *testing.T, cir CIRDocument) (SessionDoc, DocumentRepresentation, map[ContentHash][]byte) {
	t.Helper()
	manifest, bodies, err := ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return SessionDoc{Hash: hash, Identity: DocumentIdentityRootV1, CIR: cir}, DocumentRepresentation{Hash: hash, Identity: DocumentIdentityRootV1, RootManifest: raw}, bodies
}

func TestDocumentIdentityExplicitVerifierAndLegacyFence(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"", "<b>&\uD55C\uAE00\U0001F642\u2028\u2029", strings.Repeat("x", ConversationManifestChunkBytes+17)} {
		cir := identityTestCIR(t, text)
		root, _, _ := identityTestRoot(t, cir)
		canonical, err := CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		legacy := SessionDoc{Hash: HashContent(canonical), CIR: cir}
		if err := ValidateSessionDocHash(legacy); err != nil {
			t.Fatal("legacy production boundary", err)
		}
		if err := VerifySessionDocIdentity(ctx, legacy); err != nil {
			t.Fatal("explicit legacy verifier", err)
		}
		if err := ValidateSessionDocHash(root); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
			t.Fatalf("root crossed legacy production fence: %v", err)
		}
		if err := VerifySessionDocIdentity(ctx, root); err != nil {
			t.Fatal("explicit root verifier", err)
		}
		if text == "" {
			if legacy.Hash != "sha256:aa26eee6bbb489caa2983098ec976b8e382105f7cc4c327d8706d4b4b696e643" || root.Hash != "sha256:ed840f598f733b77a265c201a5b47878c405243c373574972cf109f5a412b774" {
				t.Fatal("shared fixed vector changed", legacy.Hash, root.Hash)
			}
		}
		stripped := root
		stripped.Identity = DocumentIdentityLegacy
		if err := VerifySessionDocIdentity(ctx, stripped); !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("root relabeled as legacy: %v", err)
		}
		legacy.Identity = DocumentIdentityRootV1
		if err := VerifySessionDocIdentity(ctx, legacy); !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("legacy relabeled as root: %v", err)
		}
		var stored StoredDocumentVerifier
		if err := stored.Verify(ctx, HashContent(canonical), canonical); err != nil {
			t.Fatal(err)
		}
		if err := stored.Verify(ctx, root.Hash, canonical); !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("warm legacy raw verifier accepted root: %v", err)
		}
	}
}

func TestDocumentIdentityRechecksBodyAndCancellation(t *testing.T) {
	doc, _, _ := identityTestRoot(t, identityTestCIR(t, "safe"))
	if err := VerifySessionDocIdentity(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	doc.CIR.Events[0].Blocks[0].Text = "evil"
	if err := VerifySessionDocIdentity(context.Background(), doc); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("same-length changed body accepted after success: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifySessionDocIdentity(ctx, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	// A metadata hash can be valid while its CIR is invalid. The explicit full
	// verifier must reject this, rather than just checking the manifest digest.
	invalid := identityTestCIR(t, "")
	invalid.Events = []Event{{Seq: 0, Kind: "unknown"}}
	envelope, _ := canonicalJSON(invalid.Envelope)
	body := []byte(`{"kind":"unknown","seq":0}`)
	manifest, err := NewConversationManifest(envelope, []ConversationManifestChunk{{Hash: HashContent(body), Bytes: int64(len(body))}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySessionDocIdentity(context.Background(), SessionDoc{Hash: hash, Identity: DocumentIdentityRootV1, CIR: invalid}); err == nil {
		t.Fatal("hash-valid invalid CIR accepted")
	}
}

func TestDocumentIdentityUnknownAndReferenceJoin(t *testing.T) {
	doc, _, _ := identityTestRoot(t, identityTestCIR(t, ""))
	snapshot := Snapshot{ID: doc.Hash, DocHash: doc.Hash, DocIdentity: doc.Identity}
	if !doc.DocumentRef().MatchesSnapshot(snapshot) {
		t.Fatal("matching root rejected")
	}
	snapshot.DocIdentity = DocumentIdentityLegacy
	if doc.DocumentRef().MatchesSnapshot(snapshot) {
		t.Fatal("scheme-stripped snapshot matched")
	}
	snapshot.DocIdentity = doc.Identity
	snapshot.ID = HashContent([]byte("other"))
	if doc.DocumentRef().MatchesSnapshot(snapshot) || (DocumentRef{}).MatchesSnapshot(Snapshot{}) {
		t.Fatal("invalid identity join matched")
	}
	for _, identity := range []DocumentIdentity{"cxt-cir-sha256-v1", "future", "CXT-MANIFEST-SHA256-V1"} {
		doc.Identity = identity
		if err := VerifySessionDocIdentity(context.Background(), doc); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
			t.Fatalf("unknown identity %q accepted: %v", identity, err)
		}
		if err := ValidateSessionDocHash(doc); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
			t.Fatalf("legacy boundary accepted unknown identity: %v", err)
		}
		if _, err := json.Marshal(doc); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
			t.Fatalf("unknown identity serialized: %v", err)
		}
	}
	for _, raw := range []string{`null`, `17`, `"future"`} {
		value := DocumentIdentityRootV1
		if err := json.Unmarshal([]byte(raw), &value); !errors.Is(err, ErrUnsupportedDocumentIdentity) || value != DocumentIdentityRootV1 {
			t.Fatalf("invalid identity decoded or changed receiver: %s: %v", raw, err)
		}
	}
}

func TestDocumentIdentityLegacyJSONUnchanged(t *testing.T) {
	cir := identityTestCIR(t, "")
	canonical, _ := CanonicalBytes(cir)
	doc := SessionDoc{Hash: HashContent(canonical), CIR: cir}
	cirJSON, _ := json.Marshal(cir)
	raw, err := json.Marshal(doc)
	want := fmt.Sprintf(`{"hash":%q,"cir":%s}`, doc.Hash, cirJSON)
	if err != nil || string(raw) != want {
		t.Fatalf("legacy document JSON changed: %s: %v", raw, err)
	}
	snapshot := Snapshot{ID: doc.Hash, DocHash: doc.Hash}
	raw, err = json.Marshal(snapshot)
	want = fmt.Sprintf(`{"id":%q,"repo_id":"","branch":"","parents":null,"doc_hash":%q,"provider":"","fidelity":"","message":"","author":{"name":"","email":"","team":""},"created_at":"0001-01-01T00:00:00Z"}`, doc.Hash, doc.Hash)
	if err != nil || string(raw) != want {
		t.Fatalf("legacy snapshot JSON changed: %s: %v", raw, err)
	}
	snapshot.DocIdentity = DocumentIdentityRootV1
	raw, err = json.Marshal(snapshot)
	var decoded Snapshot
	if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded.DocumentRef() != snapshot.DocumentRef() {
		t.Fatal("snapshot scheme round trip", err)
	}
	for _, format := range []string{"", "cxt-doc-chunks-v1", "cxt-doc-chunks-v2"} {
		descriptor := DocumentRepresentation{Hash: doc.Hash, Format: format, Envelope: json.RawMessage(`{}`), Chunks: []ContentHash{doc.Hash}}
		raw, err := json.Marshal(descriptor)
		formatField := ""
		if format != "" {
			formatField = fmt.Sprintf(`,"format":%q`, format)
		}
		want := fmt.Sprintf(`{"hash":%q%s,"envelope":{},"chunks":[%q]}`, doc.Hash, formatField, doc.Hash)
		if err != nil || string(raw) != want {
			t.Fatalf("legacy descriptor JSON changed: %s: %v", raw, err)
		}
		var decoded DocumentRepresentation
		if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded, descriptor) {
			t.Fatal("legacy descriptor round trip", err)
		}
	}
	raw, err = json.Marshal(DocumentRepresentation{Hash: doc.Hash})
	if err != nil || string(raw) != fmt.Sprintf(`{"hash":%q,"envelope":null,"chunks":null}`, doc.Hash) {
		t.Fatal("legacy nil encoding changed", string(raw), err)
	}
}

func TestDocumentRepresentationRootRoundTripIsMetadataOnly(t *testing.T) {
	for _, text := range []string{"", "safe"} {
		doc, descriptor, bodies := identityTestRoot(t, identityTestCIR(t, text))
		full, err := json.Marshal(doc)
		var roundTrip SessionDoc
		if err != nil || json.Unmarshal(full, &roundTrip) != nil || roundTrip.DocumentRef() != doc.DocumentRef() || VerifySessionDocIdentity(context.Background(), roundTrip) != nil {
			t.Fatal("full root document round trip", err)
		}
		raw, err := json.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 || !bytes.Equal(fields["root_manifest"], descriptor.RootManifest) {
			t.Fatal("root wire is not the three-field union", err)
		}
		var decoded DocumentRepresentation
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.DocumentRef() != doc.DocumentRef() {
			t.Fatal("root descriptor round trip", err)
		}
		manifest, err := decoded.ConversationManifest()
		if err != nil {
			t.Fatal(err)
		}
		if text == "" && (manifest.Chunks == nil || len(manifest.Chunks) != 0) {
			t.Fatal("empty root lost explicit empty chunk list")
		}
		if text != "" {
			delete(bodies, manifest.Chunks[0].Hash)
			if err := VerifyConversationManifest(context.Background(), doc.Hash, manifest, func(_ context.Context, hash ContentHash) ([]byte, error) { return bodies[hash], nil }); err == nil {
				t.Fatal("metadata decoding substituted for current-byte proof")
			}
		}
		manifest.Envelope[0] = '!'
		if _, err := decoded.ConversationManifest(); err != nil {
			t.Fatal("returned manifest aliases descriptor bytes", err)
		}
	}
}

func TestDocumentRepresentationRejectsAmbiguityWithoutFallback(t *testing.T) {
	_, descriptor, _ := identityTestRoot(t, identityTestCIR(t, ""))
	wire, _ := json.Marshal(descriptor)
	base := string(wire)
	candidates := []string{
		strings.Replace(base, `"identity":`, `"Identity":`, 1),
		strings.Replace(base, `"identity":`, `"identity":"","identity":`, 1),
		strings.Replace(base, `"identity":"cxt-manifest-sha256-v1",`, "", 1),
		strings.Replace(base, `"identity":"cxt-manifest-sha256-v1"`, `"identity":"future"`, 1),
		strings.Replace(base, `"identity":"cxt-manifest-sha256-v1"`, `"identity":null`, 1),
		strings.Replace(base, `"root_manifest":`, `"format":"","root_manifest":`, 1),
		strings.Replace(base, `"root_manifest":`, `"chunks":null,"root_manifest":`, 1),
		strings.Replace(base, `"root_manifest":`, `"envelope":null,"root_manifest":`, 1),
		strings.Replace(base, `"root_manifest":`, `"unknown":0,"root_manifest":`, 1),
		strings.Replace(base, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(base, `"version":1`, `"version":2`, 1),
		strings.Replace(base, string(descriptor.Hash), string(HashContent([]byte("wrong"))), 1),
		base + `{}`,
		`null`,
		`{"hash":"` + string(descriptor.Hash) + `","identity":"cxt-manifest-sha256-v1"}`,
		`{"hash":"` + string(descriptor.Hash) + `","envelope":{},"chunks":[],"root_manifest":null}`,
	}
	for i, raw := range candidates {
		before := DocumentRepresentation{Hash: HashContent([]byte("unchanged")), Format: "old"}
		decoded := before
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil || !reflect.DeepEqual(decoded, before) {
			t.Fatalf("case %d accepted or modified receiver: %v", i, err)
		}
	}
	for name, mutate := range map[string]func(*DocumentRepresentation){
		"stripped":         func(d *DocumentRepresentation) { d.Identity = DocumentIdentityLegacy },
		"unknown":          func(d *DocumentRepresentation) { d.Identity = "future" },
		"legacy fields":    func(d *DocumentRepresentation) { d.Chunks = []ContentHash{} },
		"missing manifest": func(d *DocumentRepresentation) { d.RootManifest = nil },
	} {
		candidate := descriptor
		mutate(&candidate)
		if _, err := json.Marshal(candidate); err == nil {
			t.Fatal("invalid descriptor serialized", name)
		}
	}
}
