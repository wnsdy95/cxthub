// Package secretscrypto — .cxtsecrets end-to-end encryption (WebCrypto and byte-compatible).
//
// Design (security review points):
//   - Server stores only ciphertext (E2E). Passwords are shared externally by the team (server not involved).
//   - KDF: PBKDF2-HMAC-SHA256, 600,000 iterations (OWASP 2023 recommendation), 16B random salt.
//   - AEAD: AES-256-GCM, 12B random nonce, AAD = "cxtsecrets:v1:<repoID>" —
//     Prevents key reuse across different repos (context binding).
//   - Quantum resistance: No asymmetric exchange, no Shor surface, AES-256 considered with Grover, 128-bit effective security (NIST current quantum resistance standard). Pure stdlib implementation.
package secretscrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Iterations is the PBKDF2 iteration count (must be the same as Web).
const Iterations = 600_000

// Envelope is the ciphertext envelope stored on the server (common JSON format for Web/CLI).
type Envelope struct {
	Version    int    `json:"version"` // 1
	KDF        string `json:"kdf"`     // "PBKDF2-SHA256"
	Iterations int    `json:"iterations"`
	SaltB64    string `json:"salt_b64"`
	Cipher     string `json:"cipher"` // "AES-256-GCM"
	NonceB64   string `json:"nonce_b64"`
	CipherB64  string `json:"ciphertext_b64"`
	// Fingerprint is the password fingerprint (first 12 hex) — validation value to check if the team is using the same password. Does not expose password characters (see Fingerprint below).
	Fingerprint string    `json:"fingerprint,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

// fpSalt is a fixed salt for fingerprints (repo-bound) — deterministic unlike the encryption random salt, always produces the same fingerprint for the same (password, repo).
func fpSalt(repoID string) []byte { return []byte("cxtsecrets-fp:v1:" + repoID) }

// Fingerprint is the password's fingerprint (SHA-256(PBKDF2(pass, fixed salt, 600k)) first 12 hex). KDF cost is the same as key derivation, preventing new surfaces in offline attacks while not exposing the password plaintext for "same password" comparison. Same as Web and byte compatibility.
func Fingerprint(passphrase, repoID string) string {
	sum := sha256.Sum256(pbkdf2Key([]byte(passphrase), fpSalt(repoID), Iterations, 32))
	return hex.EncodeToString(sum[:])[:12]
}

// rePassphrase — 4 or more words, single space separation (no leading/trailing spaces, consecutive spaces, punctuation, or digits allowed). Same as Web passphrase.ts.
var rePassphrase = regexp.MustCompile(`^[a-zA-Z]+( [a-zA-Z]+){3,}$`)

// ValidatePassphrase checks team password format (must match rules): 4 or more words, 12 characters or more. ASCII English characters only, so len(bytes) == len(characters) for accurate length check.
func ValidatePassphrase(p string) error {
	if len(p) < 12 || !rePassphrase.MatchString(p) {
		return errors.New("Team passphrase must be at least 4 English words and 12 characters (e.g., harbor twist ledger cousin)")
	}
	return nil
}

// pbkdf2 (RFC 8018, HMAC-SHA256) — no external dependencies, using stdlib.
func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte {
	prf := func(data []byte) []byte {
		h := hmac.New(sha256.New, password)
		h.Write(data)
		return h.Sum(nil)
	}
	numBlocks := (keyLen + sha256.Size - 1) / sha256.Size
	out := make([]byte, 0, numBlocks*sha256.Size)
	for block := 1; block <= numBlocks; block++ {
		idx := make([]byte, 4)
		binary.BigEndian.PutUint32(idx, uint32(block))
		u := prf(append(append([]byte{}, salt...), idx...))
		t := append([]byte{}, u...)
		for i := 1; i < iter; i++ {
			u = prf(u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func aad(repoID string) []byte { return []byte("cxtsecrets:v1:" + repoID) }

// Encrypt takes a plaintext and a passphrase to create an envelope.
func Encrypt(passphrase, plaintext, repoID string) (Envelope, error) {
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return Envelope{}, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	key := pbkdf2Key([]byte(passphrase), salt, Iterations, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return Envelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), aad(repoID))
	return Envelope{
		Version: 1, KDF: "PBKDF2-SHA256", Iterations: Iterations,
		SaltB64: base64.StdEncoding.EncodeToString(salt),
		Cipher:  "AES-256-GCM", NonceB64: base64.StdEncoding.EncodeToString(nonce),
		CipherB64:   base64.StdEncoding.EncodeToString(ct),
		Fingerprint: Fingerprint(passphrase, repoID),
	}, nil
}

// ValidateEnvelope checks the wire envelope contract before starting an expensive KDF.
func ValidateEnvelope(env Envelope) error {
	if env.Version != 1 || env.KDF != "PBKDF2-SHA256" || env.Cipher != "AES-256-GCM" {
		return errors.New("Unsupported envelope format")
	}
	if env.Iterations != Iterations {
		return fmt.Errorf("Unsupported KDF iteration count: %d", env.Iterations)
	}
	salt, err := base64.StdEncoding.DecodeString(env.SaltB64)
	if err != nil || len(salt) != 16 {
		return errors.New("Envelope salt must be 16 bytes")
	}
	nonce, err := base64.StdEncoding.DecodeString(env.NonceB64)
	if err != nil || len(nonce) != 12 {
		return errors.New("Envelope nonce must be 12 bytes")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.CipherB64)
	if err != nil || len(ciphertext) < 16 {
		return errors.New("AES-GCM ciphertext format is invalid")
	}
	if env.Fingerprint != "" {
		if len(env.Fingerprint) != 12 {
			return errors.New("envelope fingerprint format is invalid")
		}
		for _, r := range env.Fingerprint {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return errors.New("envelope fingerprint format is invalid")
			}
		}
	}
	return nil
}

// Decrypt decrypts the envelope. A passphrase mismatch or corruption results in an error (GCM authentication failure).
func Decrypt(passphrase string, env Envelope, repoID string) (string, error) {
	if err := ValidateEnvelope(env); err != nil {
		return "", err
	}
	salt, err := base64.StdEncoding.DecodeString(env.SaltB64)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(env.NonceB64)
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(env.CipherB64)
	if err != nil {
		return "", err
	}
	key := pbkdf2Key([]byte(passphrase), salt, env.Iterations, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, ct, aad(repoID))
	if err != nil {
		return "", errors.New("decryption failed — passphrase is incorrect or data is corrupted")
	}
	return string(pt), nil
}
