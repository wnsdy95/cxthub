package federation

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

var errVault = errors.New("identity credential encryption unavailable or invalid")

// Vault encrypts server-owned IdP credentials; this is separate from users'
// end-to-end encrypted repository secrets. The key stays outside PostgreSQL.
type Vault struct{ aead cipher.AEAD }

func NewVault(encodedKey string) (*Vault, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errVault
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errVault
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errVault
	}
	return &Vault{aead: aead}, nil
}
func (v *Vault) Seal(purpose, plain string) (string, error) {
	if purpose == "" || plain == "" {
		return "", errVault
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errVault
	}
	out := v.aead.Seal(nonce, nonce, []byte(plain), []byte(purpose))
	return "v1." + base64.RawURLEncoding.EncodeToString(out), nil
}
func (v *Vault) Open(purpose, sealed string) (string, error) {
	if purpose == "" || !strings.HasPrefix(sealed, "v1.") {
		return "", errVault
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "v1."))
	if err != nil || len(b) < v.aead.NonceSize()+v.aead.Overhead() {
		return "", errVault
	}
	out, err := v.aead.Open(nil, b[:v.aead.NonceSize()], b[v.aead.NonceSize():], []byte(purpose))
	if err != nil {
		return "", errVault
	}
	return string(out), nil
}
