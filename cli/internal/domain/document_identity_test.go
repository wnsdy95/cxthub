package domain

import (
	"encoding/json"
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
