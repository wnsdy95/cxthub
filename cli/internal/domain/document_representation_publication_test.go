package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Legacy compatibility matches the currently deployed backend decoder, not
// the old standalone CLI union's unnecessarily strict unknown-field policy.
func TestPublicationLegacyDescriptorCompatibility(t *testing.T) {
	h := HashContent([]byte("legacy"))
	for _, raw := range []string{
		`{"hash":"` + string(h) + `","envelope":{},"chunks":[],"future":{"large":9007199254740993}}`,
		`{"HASH":"` + string(h) + `","ENVELOPE":{},"CHUNKS":[],"format":"old","FORMAT":"cxt-doc-chunks-v1"}`,
		`{"hash":"` + string(h) + `"}`, `{}`,
	} {
		var doc DocumentRepresentation
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Errorf("legacy compatibility: %s: %v", raw, err)
		}
	}
}

func TestPublicationRepresentationStrictRootAndIdentity(t *testing.T) {
	m, _ := conversationTestPlan(t, conversationTestEnvelope(t, true), nil, 0)
	raw, _ := CanonicalConversationManifest(m)
	h, _ := ConversationManifestHash(m)
	doc := DocumentRepresentation{Hash: h, Identity: DocumentIdentityRootV1, RootManifest: raw}
	wire, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string][]byte{
		"unknown":      bytes.Replace(wire, []byte(`"hash":`), []byte(`"future":0,"hash":`), 1),
		"alias":        bytes.Replace(wire, []byte(`"hash":`), []byte(`"Hash":`), 1),
		"escaped-key":  bytes.Replace(wire, []byte(`"hash":`), []byte(`"ha\u0073h":`), 1),
		"outer-limit":  bytes.Replace(wire, []byte("{"), append([]byte("{"), bytes.Repeat([]byte(" "), MaxConversationManifestBytes+1024)...), 1),
		"duplicate":    bytes.Replace(wire, []byte(`"identity":`), []byte(`"identity":"","identity":`), 1),
		"mixed":        bytes.Replace(wire, []byte(`"hash":`), []byte(`"chunks":null,"hash":`), 1),
		"null":         bytes.Replace(wire, []byte(`"identity":"cxt-manifest-sha256-v1"`), []byte(`"identity":null`), 1),
		"inner-number": bytes.Replace(wire, []byte(`9007199254740993`), []byte(`9007199254740993.0`), 1),
		"trailing":     append(bytes.Clone(wire), []byte(`{}`)...),
		"utf8":         bytes.Replace(wire, []byte("model"), []byte{255}, 1),
	} {
		t.Run(name, func(t *testing.T) {
			dst := DocumentRepresentation{Hash: HashContent([]byte("sentinel"))}
			before := dst.Hash
			if json.Unmarshal(mutated, &dst) == nil || dst.Hash != before {
				t.Fatal("accepted invalid root or mutated receiver")
			}
		})
	}
	for _, raw := range []string{`{"identity":"","Identity":""}`, `{"identity":null}`, `{"identity":"future"}`, `{"root_manifest":null}`, `{"ROOT_MANIFEST":null}`} {
		var dst DocumentRepresentation
		if json.Unmarshal([]byte(raw), &dst) == nil {
			t.Fatalf("identity fallback: %s", raw)
		}
	}
	decoded := DocumentRepresentation{}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	first, err := decoded.ConversationManifest()
	if err != nil {
		t.Fatal(err)
	}
	first.Envelope[0] = '!'
	if _, err := decoded.ConversationManifest(); err != nil {
		t.Fatal("returned alias", err)
	}
	legacy := DocumentRepresentation{Hash: HashContent([]byte("legacy"))}
	legacyWire, err := json.Marshal(legacy)
	want := `{"hash":"` + string(legacy.Hash) + `","envelope":null,"chunks":null}`
	if err != nil || string(legacyWire) != want {
		t.Fatalf("legacy bytes: %s %v", legacyWire, err)
	}
}

func TestPublicationRootOuterOrderAndReceiverReuse(t *testing.T) {
	m, _ := conversationTestPlan(t, conversationTestEnvelope(t, false), nil, 0)
	raw, _ := CanonicalConversationManifest(m)
	h, _ := ConversationManifestHash(m)
	wire := []byte("{ \n " + `"root_manifest":` + string(raw) + `, "identity":"cxt-manifest-sha256-v1", "hash":"` + string(h) + `" }`)
	doc := DocumentRepresentation{Hash: HashContent([]byte("previous")), Format: "previous", Envelope: json.RawMessage(`{}`), Chunks: []ContentHash{HashContent([]byte("previous"))}}
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Hash != h || doc.Identity != DocumentIdentityRootV1 || doc.Format != "" || doc.Envelope != nil || doc.Chunks != nil {
		t.Fatal("stale receiver fields")
	}
	if err := json.Unmarshal([]byte(`{}`), &doc); err != nil || doc.Hash != "" || doc.Identity != "" || doc.RootManifest != nil {
		t.Fatal("legacy retained prior root", err)
	}
}
