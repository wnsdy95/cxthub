package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// googleCertsURL is the endpoint for Firebase ID token signature verification using Google's public x509 certificates.
const googleCertsURL = "https://www.googleapis.com/robot/v1/metadata/x509/securetoken@system.gserviceaccount.com"

// FirebaseVerifier is an IdentityVerifier that validates Firebase ID tokens (RS256 JWTs).
//
// Validates without external SDKs using only stdlib (crypto/rsa·x509):
//  1. Splits JWT into header.payload.signature, decodes base64url.
//  2. Selects Google public certificate (cache) by kid.
//  3. Verifies RS256 signature (rsa.VerifyPKCS1v15 + SHA-256).
//  4. Validates claims: aud == projectID, iss == https://securetoken.google.com/<projectID>, exp not expired, sub exists,
//     email_verified == true (reject unverified email signups — password signups pass only after clicking the verification link).
//
// Certificates are cached for Cache-Control max-age (runtime network required — works in deployment environments).
type FirebaseVerifier struct {
	projectID string
	httpc     *http.Client

	mu      sync.Mutex
	certs   map[string]*rsa.PublicKey
	certExp time.Time
}

// NewFirebaseVerifier creates a verifier for the given projectID (Firebase project ID).
func NewFirebaseVerifier(projectID string) *FirebaseVerifier {
	return &FirebaseVerifier{projectID: projectID, httpc: &http.Client{Timeout: 10 * time.Second}}
}

var _ outbound.IdentityVerifier = (*FirebaseVerifier)(nil)

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type fbClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Aud           string `json:"aud"`
	Iss           string `json:"iss"`
	Exp           int64  `json:"exp"`
	Iat           int64  `json:"iat"`
	AuthTime      int64  `json:"auth_time"`
}

const firebaseClockSkew = 5 * time.Minute

// Verify validates a Firebase ID token and returns a User. Invalid → domain.ErrUnauthorized.
func (v *FirebaseVerifier) Verify(ctx context.Context, token string) (domain.User, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return domain.User{}, domain.ErrUnauthorized
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil || hdr.Alg != "RS256" || hdr.Kid == "" {
		return domain.User{}, domain.ErrUnauthorized
	}

	pub, err := v.keyForKid(ctx, hdr.Kid)
	if err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}

	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	var c fbClaims
	if err := json.Unmarshal(claimBytes, &c); err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	now := time.Now()
	latestPastClaim := now.Add(firebaseClockSkew).Unix()
	if c.Sub == "" || c.Aud != v.projectID ||
		c.Iss != "https://securetoken.google.com/"+v.projectID ||
		now.Unix() >= c.Exp || c.Iat <= 0 || c.AuthTime <= 0 ||
		c.Iat > latestPastClaim || c.AuthTime > latestPastClaim {
		return domain.User{}, domain.ErrUnauthorized
	}
	// Rejects unverified email tokens (defense in depth) — password signups pass only after clicking the verification link.
	// Verified providers like Google always return true, so this has no effect.
	if !c.EmailVerified {
		return domain.User{}, domain.ErrUnauthorized
	}
	name := c.Name
	if name == "" {
		name = c.Email
	}
	return domain.User{ID: c.Sub, Email: c.Email, Name: name}, nil
}

// keyForKid returns the public key for the given kid (refreshes cache on expiration).
func (v *FirebaseVerifier) keyForKid(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.certs == nil || time.Now().After(v.certExp) {
		if err := v.refresh(ctx); err != nil {
			return nil, err
		}
	}
	pub, ok := v.certs[kid]
	if !ok {
		return nil, fmt.Errorf("firebase: unknown signing key %q", kid)
	}
	return pub, nil
}

// refresh fetches and parses Google public certificates, caching them.
func (v *FirebaseVerifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleCertsURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("firebase: certs fetch status %d", resp.StatusCode)
	}
	var raw map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	certs := make(map[string]*rsa.PublicKey, len(raw))
	for kid, pemStr := range raw {
		block, _ := pem.Decode([]byte(pemStr))
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			certs[kid] = pub
		}
	}
	if len(certs) == 0 {
		return fmt.Errorf("firebase: no usable signing certs")
	}
	v.certs = certs
	v.certExp = time.Now().Add(cacheTTL(resp.Header.Get("Cache-Control")))
	return nil
}

// cacheTTL parses Cache-Control's max-age (defaults to 1 hour if not specified).
func cacheTTL(cacheControl string) time.Duration {
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)
		if sec, ok := strings.CutPrefix(part, "max-age="); ok {
			if n, err := strconv.Atoi(sec); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return time.Hour
}
