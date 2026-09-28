package federation

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestIdentityCredentialsAreBoundToConnectionAndPurpose(t *testing.T) {
	v, err := NewVault(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	a, err := v.Seal("enterprise:revision", "synthetic-secret")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := v.Seal("enterprise:revision", "synthetic-secret")
	if a == b || strings.Contains(a, "synthetic-secret") {
		t.Fatal("nonce reuse/plaintext")
	}
	if got, err := v.Open("enterprise:revision", a); err != nil || got != "synthetic-secret" {
		t.Fatal(got, err)
	}
	if _, err := v.Open("other:revision", a); err == nil {
		t.Fatal("cross-tenant ciphertext accepted")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(a, "v1."))
	raw[len(raw)-1] ^= 1
	if _, err := v.Open("enterprise:revision", "v1."+base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	for _, key := range []string{"", "bad", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 16)))} {
		if _, err := NewVault(key); err == nil {
			t.Fatal("weak key")
		}
	}
}
