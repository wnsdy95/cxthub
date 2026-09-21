package domain

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func policyEnvelope(fp string) []byte {
	return []byte(`{"version":1,"kdf":"PBKDF2-SHA256","iterations":600000,"salt_b64":"AAAAAAAAAAAAAAAAAAAAAA==","cipher":"AES-256-GCM","nonce_b64":"AAAAAAAAAAAAAAAA","ciphertext_b64":"AAAAAAAAAAAAAAAAAAAAAA==","fingerprint":"` + fp + `"}`)
}

func TestSecretsEditPolicy(t *testing.T) {
	a, b, legacy := policyEnvelope("aaaaaaaaaaaa"), policyEnvelope("bbbbbbbbbbbb"), policyEnvelope("")
	for _, tc := range []struct {
		name          string
		current, next []byte
		edit          SecretsEdit
		want          error
	}{
		{"missing baseline", nil, a, SecretsEdit{}, ErrSecretsRevisionRequired},
		{"legacy create", nil, legacy, SecretsEdit{ExpectedRevision: "absent"}, nil},
		{"legacy upgrade", legacy, a, SecretsEdit{ExpectedRevision: SecretsRevision(legacy)}, nil},
		{"same key", a, a, SecretsEdit{ExpectedRevision: SecretsRevision(a)}, nil},
		{"changed key", a, b, SecretsEdit{ExpectedRevision: SecretsRevision(a)}, ErrSecretsPassphraseMismatch},
		{"fingerprint removed", a, legacy, SecretsEdit{ExpectedRevision: SecretsRevision(a)}, ErrSecretsFingerprintRequired},
		{"rotation no fingerprint", a, legacy, SecretsEdit{ExpectedRevision: SecretsRevision(a), Rotate: true}, ErrSecretsFingerprintRequired},
		{"rotation stale key", a, b, SecretsEdit{ExpectedRevision: SecretsRevision(a), Rotate: true, ExpectedFingerprint: "bbbbbbbbbbbb"}, ErrSecretsRotateConflict},
		{"rotation", a, b, SecretsEdit{ExpectedRevision: SecretsRevision(a), Rotate: true, ExpectedFingerprint: "aaaaaaaaaaaa"}, nil},
		{"stale edit", a, b, SecretsEdit{ExpectedRevision: "absent", Rotate: true, ExpectedFingerprint: "aaaaaaaaaaaa"}, ErrSecretsConflict},
		{"malformed", nil, []byte("{"), SecretsEdit{ExpectedRevision: "absent"}, ErrSecretsMalformedEnvelope},
		{"null", nil, []byte("null"), SecretsEdit{ExpectedRevision: "absent"}, ErrSecretsMalformedEnvelope},
		{"unsupported KDF", nil, bytes.Replace(a, []byte("600000"), []byte("2000000000"), 1), SecretsEdit{ExpectedRevision: "absent"}, ErrIntegrity},
		{"stored corruption", []byte("{}"), a, SecretsEdit{ExpectedRevision: SecretsRevision([]byte("{}"))}, ErrSecretsConsistency},
		{"oversize", nil, []byte(strings.Repeat("x", MaxSecretsEnvelopeBytes+1)), SecretsEdit{ExpectedRevision: "absent"}, ErrIntegrity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]byte(nil), tc.current...)
			next := append([]byte(nil), tc.next...)
			if err := ValidateSecretsEdit(tc.current, tc.next, tc.edit); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if !bytes.Equal(before, tc.current) || !bytes.Equal(next, tc.next) {
				t.Fatal("policy mutated caller envelope")
			}
		})
	}
}

func TestSecretsUnknownPolicyAndRoleFailClosed(t *testing.T) {
	if CanEditSecrets(Repository{SecretsPolicy: "unknown"}, RoleMaintainer) {
		t.Fatal("unknown policy granted maintainer write")
	}
	if CanEditSecrets(Repository{}, MemberRole("administrator")) {
		t.Fatal("unknown role granted write")
	}
}
