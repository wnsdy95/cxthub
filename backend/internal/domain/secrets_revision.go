package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SecretsRevision identifies the exact editing baseline, independently of the
// passphrase fingerprint. Legacy ciphertext remains readable without migration.
func SecretsRevision(raw []byte) string {
	if len(raw) == 0 {
		return "absent"
	}
	var env struct {
		Revision string `json:"revision"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Revision != "" {
		return env.Revision
	}
	sum := sha256.Sum256(raw)
	return "legacy-" + hex.EncodeToString(sum[:])
}

// WithSecretsRevision annotates opaque ciphertext; it never decrypts it.
// Every accepted write receives a fresh generation, including identical writes.
func WithSecretsRevision(raw []byte, revision string) ([]byte, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env == nil {
		return nil, ErrIntegrity
	}
	if revision == "" {
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		revision = "v1-" + hex.EncodeToString(nonce[:])
	}
	env["revision"], _ = json.Marshal(revision)
	return json.Marshal(env)
}
