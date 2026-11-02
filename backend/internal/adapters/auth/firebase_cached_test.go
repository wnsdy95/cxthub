package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type forbiddenCertTransport struct{ calls atomic.Int64 }

func (transport *forbiddenCertTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("snapshot verification must not fetch keys")
}

func TestFirebaseCachedVerificationNeverFetches(test *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		test.Fatal(err)
	}
	transport := &forbiddenCertTransport{}
	verifier := NewFirebaseVerifier("snapshot-test")
	verifier.httpc = &http.Client{Transport: transport}
	now := time.Now()
	claims := map[string]any{"sub": "snapshot-user", "email": "snapshot@example.test", "email_verified": true,
		"aud": "snapshot-test", "iss": "https://securetoken.google.com/snapshot-test", "exp": now.Add(time.Hour).Unix(),
		"iat": now.Add(-time.Minute).Unix(), "auth_time": now.Add(-time.Minute).Unix()}
	token := mkToken(test, key, "snapshot-key", claims)
	for _, state := range []string{"empty", "expired", "missing key", "valid", "busy", "expired token", "canceled"} {
		test.Run(state, func(test *testing.T) {
			verifier.certs = map[string]*rsa.PublicKey{"snapshot-key": &key.PublicKey}
			verifier.certExp = time.Now().Add(time.Hour)
			ctx := context.Background()
			currentToken := token
			switch state {
			case "empty":
				verifier.certs = nil
			case "expired":
				verifier.certExp = time.Now().Add(-time.Second)
			case "missing key":
				verifier.certs = map[string]*rsa.PublicKey{"other": &key.PublicKey}
			case "busy":
				verifier.mu.Lock()
				defer verifier.mu.Unlock()
			case "expired token":
				claims["exp"] = now.Add(-time.Second).Unix()
				currentToken = mkToken(test, key, "snapshot-key", claims)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			user, err := verifier.VerifyCached(ctx, currentToken)
			if state == "valid" {
				if err != nil || user.ID != "snapshot-user" {
					test.Fatal(user, err)
				}
			} else if err == nil || user.ID != "" {
				test.Fatal("invalid cached verification succeeded", user, err)
			}
		})
	}
	if transport.calls.Load() != 0 {
		test.Fatal("cached verification made an external request")
	}
	if _, err := verifier.Verify(context.Background(), mkToken(test, key, "unknown", claims)); err == nil {
		test.Fatal("unknown key accepted")
	}
}
