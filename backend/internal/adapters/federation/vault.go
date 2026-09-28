package federation

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

var errVault = errors.New("identity credential encryption unavailable or invalid")

// Vault encrypts server-owned IdP credentials; this is separate from users'
// end-to-end encrypted repository secrets. The key stays outside PostgreSQL.
type Vault struct {
	active string
	keys   map[string]cipher.AEAD
}

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)

// ConfiguredVault supports a staged rollout: every replica first receives the
// read keys, then the active writer changes, then stored credentials are rewrapped.
// The legacy key remains separate because v1 ciphertext has no key identifier.
func ConfiguredVault(legacy, config string) (*Vault, error) {
	if config == "" {
		if legacy == "" {
			return nil, nil
		}
		return NewVault(legacy)
	}
	if len(config) > 16<<10 || !uniqueKeyConfig(config) {
		return nil, errVault
	}
	var c struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	d := json.NewDecoder(strings.NewReader(config))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return nil, errVault
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(c.Keys) == 0 || len(c.Keys) > 16 {
		return nil, errVault
	}
	v := &Vault{active: c.Active, keys: map[string]cipher.AEAD{}}
	for id, key := range c.Keys {
		if id == "legacy" || !keyIDPattern.MatchString(id) {
			return nil, errVault
		}
		a, err := vaultAEAD(key)
		if err != nil {
			return nil, err
		}
		v.keys[id] = a
	}
	if legacy != "" {
		a, err := vaultAEAD(legacy)
		if err != nil {
			return nil, err
		}
		v.keys["legacy"] = a
	}
	if v.keys[v.active] == nil {
		return nil, errVault
	}
	return v, nil
}

// Duplicate JSON fields are ambiguous operator intent, including a repeated key
// ID with different material. encoding/json alone silently takes the last value.
func uniqueKeyConfig(raw string) bool {
	d := json.NewDecoder(strings.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 2 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		if delim, ok := t.(json.Delim); ok {
			if delim != '{' {
				return false
			}
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := k.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		}
		_, ok := t.(string)
		return ok
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func NewVault(encodedKey string) (*Vault, error) {
	a, err := vaultAEAD(encodedKey)
	if err != nil {
		return nil, err
	}
	return &Vault{active: "legacy", keys: map[string]cipher.AEAD{"legacy": a}}, nil
}
func vaultAEAD(encodedKey string) (cipher.AEAD, error) {
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
	return aead, nil
}
func (v *Vault) ActiveKeyID() string { return v.active }
func (v *Vault) Seal(purpose, plain string) (string, error) {
	if purpose == "" || plain == "" {
		return "", errVault
	}
	a := v.keys[v.active]
	if a == nil {
		return "", errVault
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errVault
	}
	prefix, aad := "v1.", purpose
	if v.active != "legacy" {
		prefix, aad = "v2."+v.active+".", "cxt-identity-v2\x00"+v.active+"\x00"+purpose
	}
	out := a.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return prefix + base64.RawURLEncoding.EncodeToString(out), nil
}
func (v *Vault) Open(purpose, sealed string) (string, error) {
	plain, _, err := v.open(purpose, sealed)
	return plain, err
}

// Inspect authenticates the whole value; an untrusted header alone never proves
// that a credential is readable or already uses the current key.
func (v *Vault) Inspect(purpose, sealed string) (string, error) {
	_, id, err := v.open(purpose, sealed)
	return id, err
}
func (v *Vault) open(purpose, sealed string) (string, string, error) {
	if purpose == "" || len(sealed) > 1<<20 {
		return "", "", errVault
	}
	id, body, aad := "legacy", "", purpose
	if strings.HasPrefix(sealed, "v1.") {
		body = sealed[3:]
	} else if p := strings.Split(sealed, "."); len(p) == 3 && p[0] == "v2" && p[1] != "legacy" && keyIDPattern.MatchString(p[1]) {
		id, body = p[1], p[2]
		aad = "cxt-identity-v2\x00" + id + "\x00" + purpose
	} else {
		return "", "", errVault
	}
	a := v.keys[id]
	if a == nil {
		return "", "", errVault
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(b) < a.NonceSize()+a.Overhead() {
		return "", "", errVault
	}
	out, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte(aad))
	if err != nil || len(out) == 0 {
		return "", "", errVault
	}
	return string(out), id, nil
}
