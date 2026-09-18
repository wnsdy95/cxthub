package domain

import (
	"encoding/json"
	"testing"
)

func TestSecretsRevisionLegacyReadAndRepeatedCiphertextWrite(t *testing.T) {
	legacy := []byte(`{"ciphertext_b64":"opaque","fingerprint":"same"}`)
	revision := SecretsRevision(legacy)
	if revision == "absent" || revision == "" {
		t.Fatal("legacy revision missing")
	}
	view, err := WithSecretsRevision(legacy, revision)
	if err != nil {
		t.Fatal(err)
	}
	if SecretsRevision(view) != revision || SecretsRevision(legacy) != revision {
		t.Fatal("legacy read mutated its baseline")
	}
	first, err := WithSecretsRevision(legacy, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := WithSecretsRevision(first, "")
	if err != nil {
		t.Fatal(err)
	}
	if SecretsRevision(first) == SecretsRevision(second) {
		t.Fatal("identical-content update reused generation")
	}
	var env map[string]string
	if err = json.Unmarshal(second, &env); err != nil {
		t.Fatal(err)
	}
	if env["ciphertext_b64"] != "opaque" {
		t.Fatal("ciphertext changed")
	}
	if SecretsRevision(nil) != "absent" {
		t.Fatal("absence baseline changed")
	}
}
