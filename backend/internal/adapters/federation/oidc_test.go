package federation

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type oidcFixture struct {
	client                 *OIDC
	settings               outbound.OIDCSettings
	claims                 map[string]any
	metadata               map[string]any
	key                    *rsa.PrivateKey
	unsigned, badSignature bool
	requests               int
	expectedVerifier       string
}

const testNonce = "nnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnn"
const testVerifier = "vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv"
const testState = "sssssssssssssssssssssssssssssssssssssssssssssss"

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f := &oidcFixture{
		key:              key,
		expectedVerifier: testVerifier,
		settings:         outbound.OIDCSettings{Issuer: "https://issuer.example.test", ClientID: "cxthub-fixture", ClientSecret: "synthetic-client-secret", RedirectURI: "https://cxthub.example.test/api/v1/auth/enterprise/oidc/callback", AuthMethod: "client_secret_basic"},
		claims:           map[string]any{"iss": "https://issuer.example.test", "sub": "stable-subject", "aud": "cxthub-fixture", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": testNonce, "auth_time": now.Unix(), "amr": []string{"pwd", "mfa"}, "email": "untrusted@example.test"},
		metadata:         map[string]any{"issuer": "https://issuer.example.test", "authorization_endpoint": "https://issuer.example.test/authorize", "token_endpoint": "https://issuer.example.test/token", "jwks_uri": "https://issuer.example.test/keys", "id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(f.metadata)
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture-key", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		case "/token":
			f.requests++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Method != "POST" || r.Form.Get("code") != "synthetic-code" || r.Form.Get("code_verifier") != f.expectedVerifier || r.Form.Get("redirect_uri") != f.settings.RedirectURI || r.Form.Get("grant_type") != "authorization_code" {
				t.Error("token exchange parameters", r.Method)
			}
			if f.settings.AuthMethod == "client_secret_basic" {
				user, password, ok := r.BasicAuth()
				if !ok || user != f.settings.ClientID || password != f.settings.ClientSecret {
					t.Error("missing client authentication")
				}
			} else if r.Form.Get("client_secret") != f.settings.ClientSecret {
				t.Error("missing post client authentication")
			}
			header := map[string]string{"alg": "RS256", "kid": "fixture-key"}
			if f.unsigned {
				header["alg"] = "none"
			}
			a, _ := json.Marshal(header)
			b, _ := json.Marshal(f.claims)
			part := base64.RawURLEncoding.EncodeToString(a) + "." + base64.RawURLEncoding.EncodeToString(b)
			hash := sha256.Sum256([]byte(part))
			sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, hash[:])
			if err != nil {
				t.Error(err)
			}
			if f.badSignature {
				sig[0] ^= 1
			}
			if f.unsigned {
				sig = nil
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access", "token_type": "Bearer", "expires_in": 3600, "id_token": part + "." + base64.RawURLEncoding.EncodeToString(sig), "refresh_token": "must-not-leave-adapter"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	local, _ := url.Parse(srv.URL)
	// Synthetic issuer URLs route to a local protocol server only in this test.
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "issuer.example.test" {
			t.Errorf("unexpected endpoint host %s", r.URL.Host)
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Host = local.Host
		u.Scheme = local.Scheme
		copy.URL = &u
		return srv.Client().Transport.RoundTrip(copy)
	})
	f.client = &OIDC{client: &http.Client{Transport: boundedTransport{transport}, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return f
}

func TestOIDCAuthorizationAndVerifiedIdentity(t *testing.T) {
	for _, method := range []string{"client_secret_basic", "client_secret_post"} {
		t.Run(method, func(t *testing.T) {
			f := newOIDCFixture(t)
			f.settings.AuthMethod = method
			urlString, err := f.client.Authorize(context.Background(), f.settings, testState, testNonce, testVerifier)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(urlString)
			q := u.Query()
			sum := sha256.Sum256([]byte(testVerifier))
			for k, want := range map[string]string{"state": testState, "nonce": testNonce, "scope": "openid", "response_type": "code", "max_age": "0", "code_challenge_method": "S256", "code_challenge": base64.RawURLEncoding.EncodeToString(sum[:])} {
				if q.Get(k) != want {
					t.Error(k, q.Get(k))
				}
			}
			if strings.Contains(urlString, f.settings.ClientSecret) || strings.Contains(urlString, testVerifier) {
				t.Fatal("credential in browser URL")
			}
			proof, err := f.client.Exchange(context.Background(), f.settings, "synthetic-code", testNonce, testVerifier, time.Now().Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if proof.Subject != "stable-subject" || proof.Issuer != f.settings.Issuer || proof.AuthenticatedAt.IsZero() || len(proof.AMR) != 2 {
				t.Fatal(proof)
			}
			raw, _ := json.Marshal(proof)
			if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "email") {
				t.Fatal("tokens/email crossed authentication port")
			}
		})
	}
}

func TestOIDCRejectsInvalidProofs(t *testing.T) {
	for _, scenario := range []string{"issuer", "audience", "nonce", "expiry", "future-iat", "missing-auth-time", "stale-auth-time", "future-auth-time", "empty-subject", "unsigned", "signature", "azp", "multi-audience-without-azp"} {
		t.Run(scenario, func(t *testing.T) {
			f := newOIDCFixture(t)
			switch scenario {
			case "issuer":
				f.claims["iss"] = "https://other.example.test"
			case "audience":
				f.claims["aud"] = "other-client"
			case "nonce":
				f.claims["nonce"] = "other-nonce"
			case "expiry":
				f.claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "future-iat":
				f.claims["iat"] = time.Now().Add(time.Hour).Unix()
			case "missing-auth-time":
				delete(f.claims, "auth_time")
			case "stale-auth-time":
				f.claims["auth_time"] = time.Now().Add(-time.Hour).Unix()
			case "future-auth-time":
				f.claims["auth_time"] = time.Now().Add(time.Hour).Unix()
			case "empty-subject":
				f.claims["sub"] = ""
			case "unsigned":
				f.unsigned = true
			case "signature":
				f.badSignature = true
			case "azp":
				f.claims["azp"] = "other-client"
			case "multi-audience-without-azp":
				f.claims["aud"] = []string{f.settings.ClientID, "other"}
			}
			_, err := f.client.Exchange(context.Background(), f.settings, "synthetic-code", testNonce, testVerifier, time.Now().Add(-time.Second))
			if err != errProof {
				t.Fatalf("%s accepted or leaked provider error: %v", scenario, err)
			}
		})
	}
}

func TestOIDCRejectsUntrustedMetadataAndBindings(t *testing.T) {
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri", "code_challenge_methods_supported"} {
		t.Run(field, func(t *testing.T) {
			f := newOIDCFixture(t)
			if field == "issuer" {
				f.metadata[field] = "https://other.example.test"
			} else if field == "code_challenge_methods_supported" {
				f.metadata[field] = []string{"plain"}
			} else {
				f.metadata[field] = "http://169.254.169.254/metadata"
			}
			if _, err := f.client.Authorize(context.Background(), f.settings, testState, testNonce, testVerifier); err == nil {
				t.Fatal("unsafe discovery accepted")
			}
			if f.requests != 0 {
				t.Fatal("credentials sent before validation")
			}
		})
	}
	f := newOIDCFixture(t)
	if _, err := f.client.Authorize(context.Background(), f.settings, "short", testNonce, testVerifier); err == nil {
		t.Fatal("weak state accepted")
	}
	if _, err := f.client.Exchange(context.Background(), f.settings, "synthetic-code", testNonce, testVerifier, time.Now().Add(-11*time.Minute)); err == nil || f.requests != 0 {
		t.Fatal("expired attempt exchanged code")
	}
}
