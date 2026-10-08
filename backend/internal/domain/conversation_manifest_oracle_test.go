package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// The manifest format is staged, but its semantic acceptance must agree with
// the existing strict canonical verifier, including legacy numeric behavior.
func TestConversationManifestExistingStrictVerifierParity(t *testing.T) {
	env := conversationTestEnvelope(t, false)
	base := []byte(conversationVectorStream)
	for _, stream := range [][]byte{
		base,
		bytes.Replace(base, []byte("9007199254740992"), []byte("9007199254740993"), 1),
		bytes.Replace(base, []byte("9007199254740992"), []byte("1e0"), 1),
		bytes.Replace(base, []byte(`"seq":1`), []byte(`"seq":0`), 1),
		bytes.Replace(base, []byte(`"seq":1`), []byte(`"seq":-1`), 1),
		bytes.Replace(base, []byte(`"seq":1`), []byte(`"seq":1,"seq":1`), 1),
		bytes.Replace(base, []byte(`"seq":1`), []byte(`"seq":1,"other":0`), 1),
		bytes.Replace(base, []byte(`"role":"user"`), []byte(`"role":null`), 1),
		bytes.Replace(base, []byte(`"kind":"tool_call"`), []byte(`"kind":"message"`), 1),
		bytes.Replace(base, []byte(`"seq":0`), []byte(`"agent_message":false,"seq":0`), 1),
	} {
		m, bodies := conversationTestPlan(t, env, stream, 2)
		raw := append([]byte(conversationDocPrefix), env...)
		raw = append(raw, conversationDocMiddle...)
		raw = append(raw, stream...)
		raw = append(raw, conversationDocSuffix...)
		var existing CanonicalDocVerifier
		_, old := existing.Verify(context.Background(), HashContent(raw), raw)
		got := conversationTestVerify(m, bodies)
		if (old == nil) != (got == nil) {
			t.Fatalf("semantic acceptance drift: old=%v new=%v", old, got)
		}
	}
	// An empty stream is new manifest support, still the same empty CIR bytes.
	m, bodies := conversationTestPlan(t, env, nil, 0)
	if err := conversationTestVerify(m, bodies); err != nil {
		t.Fatal(err)
	}
	legacy := CIRDocument{}
	if json.Unmarshal(env, &legacy.Envelope) != nil {
		t.Fatal("fixture")
	}
	if raw, err := CanonicalBytes(legacy); err != nil || !json.Valid(raw) {
		t.Fatal("empty CIR", err)
	}
}
