package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// mkToken creates an RS256 Firebase ID token with a test RSA key to exercise signature verification.
func mkToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestFirebaseEmailVerifiedGate: email_verified gate — only authenticated emails pass, unauthenticated are rejected.
func TestFirebaseEmailVerifiedGate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const proj, kid = "cxthub-test", "testkid"
	v := NewFirebaseVerifier(proj)
	v.certs = map[string]*rsa.PublicKey{kid: &key.PublicKey} // bypass cert fetch
	v.certExp = time.Now().Add(time.Hour)

	claims := func(verified bool) map[string]any {
		now := time.Now()
		return map[string]any{
			"sub": "uid-1", "email": "a@t.io", "email_verified": verified,
			"aud": proj, "iss": "https://securetoken.google.com/" + proj,
			"exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(),
			"auth_time": now.Add(-2 * time.Minute).Unix(),
		}
	}
	if u, err := v.Verify(context.Background(), mkToken(t, key, kid, claims(true))); err != nil || u.ID != "uid-1" {
		t.Fatalf("authenticated email must pass: u=%+v err=%v", u, err)
	}
	if _, err := v.Verify(context.Background(), mkToken(t, key, kid, claims(false))); err != domain.ErrUnauthorized {
		t.Fatalf("unauthenticated email must be rejected, got %v", err)
	}

	for _, field := range []string{"iat", "auth_time"} {
		bad := claims(true)
		bad[field] = time.Now().Add(firebaseClockSkew + time.Minute).Unix()
		if _, err := v.Verify(context.Background(), mkToken(t, key, kid, bad)); err != domain.ErrUnauthorized {
			t.Fatalf("future %s claim must be rejected, got %v", field, err)
		}
	}
	missingIAT := claims(true)
	delete(missingIAT, "iat")
	if _, err := v.Verify(context.Background(), mkToken(t, key, kid, missingIAT)); err != domain.ErrUnauthorized {
		t.Fatalf("token without iat must be rejected, got %v", err)
	}
}
