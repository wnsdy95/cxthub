package federation

import (
	"encoding/base64"
	"encoding/json"
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

func TestIdentityKeyringStagedRotationAndRetirement(t *testing.T) {
	old := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	newKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))
	config := func(active string) string {
		b, _ := json.Marshal(map[string]any{"active": active, "keys": map[string]string{"new": newKey, "alias": newKey}})
		return string(b)
	}
	legacy, _ := NewVault(old)
	before, _ := legacy.Seal("tenant:revision", "fixture")
	reader, err := ConfiguredVault(old, config("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Open("tenant:revision", before); err != nil || got != "fixture" {
		t.Fatal(got, err)
	}
	stillOld, _ := reader.Seal("tenant:revision", "fixture")
	if !strings.HasPrefix(stillOld, "v1.") {
		t.Fatal("staging switched writer")
	}
	writer, err := ConfiguredVault(old, config("new"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := writer.Seal("tenant:revision", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Open("tenant:revision", after); err != nil || got != "fixture" {
		t.Fatal("old-active replica cannot read new value", err)
	}
	if key, err := writer.Inspect("tenant:revision", before); err != nil || key != "legacy" {
		t.Fatal(key, err)
	}
	if key, err := writer.Inspect("tenant:revision", after); err != nil || key != "new" {
		t.Fatal(key, err)
	}
	// Even aliases containing identical key bytes cannot authenticate a changed ID.
	for _, bad := range []string{strings.Replace(after, "v2.new.", "v2.alias.", 1), "v2.new.bad", "v3.new.bad"} {
		if _, err := writer.Inspect("tenant:revision", bad); err == nil {
			t.Fatal("unauthenticated header accepted")
		}
	}
	if _, err := writer.Open("other:revision", after); err == nil {
		t.Fatal("purpose not authenticated")
	}
	retired, _ := ConfiguredVault("", config("new"))
	if _, err := retired.Open("tenant:revision", before); err == nil {
		t.Fatal("missing legacy key accepted")
	}
	if got, err := retired.Open("tenant:revision", after); err != nil || got != "fixture" {
		t.Fatal("new value lost", err)
	}
	// Restoring the exact old key backup restores old ciphertext, without rewriting it.
	restored, _ := ConfiguredVault(old, config("new"))
	if got, err := restored.Open("tenant:revision", before); err != nil || got != "fixture" {
		t.Fatal("backup restore failed", err)
	}
}

func TestIdentityKeyringRejectsInvalidConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	for _, raw := range []string{`{"active":"x","active":"x","keys":{"x":"` + key + `"}}`, `{"active":"x","keys":{"x":"` + key + `","x":"` + key + `"}}`} {
		if _, err := ConfiguredVault("", raw); err == nil {
			t.Fatal("duplicate config key accepted")
		}
	}
	for _, raw := range []string{`{}`, `{"active":"x","keys":{}}`, `{"active":"x","keys":{"x":"bad"}}`, `{"active":"legacy","keys":{"legacy":"bad"}}`, `{"active":"x","keys":{},"typo":true}`, `{} {}`} {
		if _, err := ConfiguredVault("", raw); err == nil {
			t.Fatal("accepted invalid keyring")
		}
	}
	if v, err := ConfiguredVault("", ""); err != nil || v != nil {
		t.Fatal("absent configuration enabled federation")
	}
}

func FuzzIdentityVaultInput(f *testing.F) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	v, _ := NewVault(key)
	valid, _ := v.Seal("fixture", "value")
	f.Add(valid)
	f.Add("v2.unknown.invalid")
	f.Add(`{"active":"legacy","keys":{}}`)
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 20<<10 {
			t.Skip()
		}
		_, _ = ConfiguredVault(key, s)
		_, _ = v.Inspect("fixture", s)
	})
}
