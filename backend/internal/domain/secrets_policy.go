package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const secretsKDFIterations = 600_000

type secretsEnvelope struct {
	Version       int    `json:"version"`
	KDF           string `json:"kdf"`
	Iterations    int    `json:"iterations"`
	SaltB64       string `json:"salt_b64"`
	Cipher        string `json:"cipher"`
	NonceB64      string `json:"nonce_b64"`
	CiphertextB64 string `json:"ciphertext_b64"`
	Fingerprint   string `json:"fingerprint,omitempty"`
}

func validateSecretsEnvelope(raw []byte) error {
	if len(raw) > MaxSecretsEnvelopeBytes {
		return fmt.Errorf("%w: envelope exceeds 256KiB", ErrIntegrity)
	}
	var env *secretsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env == nil {
		return ErrSecretsMalformedEnvelope
	}
	if env.Version != 1 || env.KDF != "PBKDF2-SHA256" || env.Cipher != "AES-256-GCM" || env.Iterations != secretsKDFIterations {
		return fmt.Errorf("%w: unsupported secrets envelope parameters", ErrIntegrity)
	}
	salt, err := base64.StdEncoding.DecodeString(env.SaltB64)
	if err != nil || len(salt) != 16 {
		return fmt.Errorf("%w: secrets envelope salt must be 16 bytes", ErrIntegrity)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.NonceB64)
	if err != nil || len(nonce) != 12 {
		return fmt.Errorf("%w: secrets envelope nonce must be 12 bytes", ErrIntegrity)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.CiphertextB64)
	if err != nil || len(ciphertext) < 16 {
		return fmt.Errorf("%w: invalid AES-GCM ciphertext", ErrIntegrity)
	}
	if env.Fingerprint != "" {
		if len(env.Fingerprint) != 12 {
			return fmt.Errorf("%w: invalid secrets fingerprint", ErrIntegrity)
		}
		for _, r := range env.Fingerprint {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return fmt.Errorf("%w: invalid secrets fingerprint", ErrIntegrity)
			}
		}
	}
	return nil
}

// MaxSecretsEnvelopeBytes bounds both encrypted content and its metadata.
const MaxSecretsEnvelopeBytes = 256 << 10

var (
	ErrSecretsRevisionRequired    = errors.New("an editing baseline is required; load secrets before editing")
	ErrSecretsConflict            = errors.New("secrets changed since editing began; keep your draft and compare it with the latest version")
	ErrSecretsFingerprintRequired = errors.New("envelope fingerprint required; update the client")
	ErrSecretsRotateConflict      = errors.New("secrets key changed after rotation began; fetch the latest envelope and retry")
	ErrSecretsPassphraseMismatch  = errors.New("secrets use a different team passphrase; use that passphrase or explicitly rotate")
	ErrSecretsConsistency         = errors.New("existing secrets could not be verified; storage rejected")
	ErrSecretsMalformedEnvelope   = errors.New("invalid encrypted envelope JSON")
)

// SecretsEdit carries the user's original editing baseline, never a baseline
// silently refreshed during save. Fingerprint and revision guard different facts.
type SecretsEdit struct {
	ExpectedRevision    string
	Rotate              bool
	ExpectedFingerprint string
}

// ValidateSecretsEdit is the single policy for all write adapters. It only reads
// opaque envelopes and never decrypts content or changes the caller's bytes.
func ValidateSecretsEdit(current, candidate []byte, edit SecretsEdit) error {
	if edit.ExpectedRevision == "" {
		return ErrSecretsRevisionRequired
	}
	if edit.ExpectedRevision != SecretsRevision(current) {
		return ErrSecretsConflict
	}
	if len(current) != 0 {
		if err := validateSecretsEnvelope(current); err != nil {
			return ErrSecretsConsistency
		}
	}
	if err := validateSecretsEnvelope(candidate); err != nil {
		return err
	}
	var old, next secretsEnvelope
	_ = json.Unmarshal(current, &old)
	_ = json.Unmarshal(candidate, &next)
	if edit.Rotate {
		if next.Fingerprint == "" {
			return ErrSecretsFingerprintRequired
		}
		if old.Fingerprint != "" && edit.ExpectedFingerprint != old.Fingerprint {
			return ErrSecretsRotateConflict
		}
	} else {
		if old.Fingerprint != "" && next.Fingerprint == "" {
			return ErrSecretsFingerprintRequired
		}
		if old.Fingerprint != "" && old.Fingerprint != next.Fingerprint {
			return ErrSecretsPassphraseMismatch
		}
	}
	return nil
}

func CanEditSecrets(repositoryRecord Repository, role MemberRole) bool {
	return !repositoryRecord.Archived && role.AtLeast(RoleMaintainer) && PolicyAllows(repositoryRecord.SecretsPolicy, role == RoleOwner)
}
