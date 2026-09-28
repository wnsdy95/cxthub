package federation

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"golang.org/x/oauth2"
)

var errProof = errors.New("identity provider authentication could not be verified")

type OIDC struct{ client *http.Client }

var _ outbound.OIDCProvider = (*OIDC)(nil)

func NewOIDC() *OIDC { return &OIDC{client: newPublicClient()} }

func validSettings(c outbound.OIDCSettings) bool {
	u, err := url.Parse(c.Issuer)
	callback, cerr := url.Parse(c.RedirectURI)
	loopbackCallback := cerr == nil && callback.Scheme == "http" && (callback.Hostname() == "127.0.0.1" || callback.Hostname() == "localhost" || callback.Hostname() == "::1")
	return err == nil && providerURL(c.Issuer) && u.RawQuery == "" && c.ClientID != "" && len(c.ClientID) <= 256 && c.ClientSecret != "" && len(c.ClientSecret) <= 4096 && cerr == nil && callback.User == nil && callback.Fragment == "" && callback.RawQuery == "" && (providerURL(c.RedirectURI) || loopbackCallback) && (c.AuthMethod == "client_secret_basic" || c.AuthMethod == "client_secret_post")
}

func (o *OIDC) discover(ctx context.Context, c outbound.OIDCSettings) (*oidc.Provider, *oauth2.Config, error) {
	if !validSettings(c) {
		return nil, nil, errProof
	}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, o.client), c.Issuer)
	if err != nil {
		return nil, nil, errProvider
	}
	var metadata struct {
		Issuer  string   `json:"issuer"`
		Keys    string   `json:"jwks_uri"`
		Methods []string `json:"code_challenge_methods_supported"`
	}
	if p.Claims(&metadata) != nil || metadata.Issuer != c.Issuer || !providerURL(metadata.Keys) {
		return nil, nil, errProof
	}
	endpoint := p.Endpoint()
	if !providerURL(endpoint.AuthURL) || !providerURL(endpoint.TokenURL) {
		return nil, nil, errProof
	}
	// PKCE remains mandatory even when older providers omit this optional field.
	if len(metadata.Methods) > 0 {
		supported := false
		for _, method := range metadata.Methods {
			supported = supported || method == "S256"
		}
		if !supported {
			return nil, nil, errProof
		}
	}
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	if c.AuthMethod == "client_secret_post" {
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	return p, &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: c.RedirectURI, Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID}}, nil
}

func validBinding(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~", ch)) {
			return false
		}
	}
	return true
}

func (o *OIDC) Authorize(ctx context.Context, c outbound.OIDCSettings, state, nonce, verifier string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !validBinding(state) || !validBinding(nonce) || !validBinding(verifier) {
		return "", errProof
	}
	_, config, err := o.discover(ctx, c)
	if err != nil {
		return "", err
	}
	return config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce), oauth2.SetAuthURLParam("max_age", "0")), nil
}

func (o *OIDC) Exchange(ctx context.Context, c outbound.OIDCSettings, code, nonce, verifier string, started time.Time) (outbound.FederationProof, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var zero outbound.FederationProof
	now := time.Now().UTC()
	if code == "" || len(code) > 4096 || !validBinding(nonce) || !validBinding(verifier) || started.IsZero() || started.After(now) || now.Sub(started) > 10*time.Minute {
		return zero, errProof
	}
	p, config, err := o.discover(ctx, c)
	if err != nil {
		return zero, err
	}
	token, err := config.Exchange(oidc.ClientContext(ctx, o.client), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return zero, errProof
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return zero, errProof
	}
	id, err := p.Verifier(&oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}}).Verify(ctx, raw)
	if err != nil {
		return zero, errProof
	}
	var claims struct {
		AuthorizedParty string   `json:"azp"`
		AuthTime        int64    `json:"auth_time"`
		AMR             []string `json:"amr"`
		ACR             string   `json:"acr"`
	}
	if id.Claims(&claims) != nil {
		return zero, errProof
	}
	now = time.Now().UTC()
	authenticated := time.Unix(claims.AuthTime, 0).UTC()
	if id.Issuer != c.Issuer || id.Subject == "" || len(id.Subject) > 512 || id.IssuedAt.IsZero() || id.IssuedAt.After(now.Add(time.Minute)) || !now.Before(id.Expiry) || id.IssuedAt.After(id.Expiry) || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return zero, errProof
	}
	if len(id.Audience) > 1 && claims.AuthorizedParty != c.ClientID || claims.AuthorizedParty != "" && claims.AuthorizedParty != c.ClientID {
		return zero, errProof
	}
	if claims.AuthTime <= 0 || authenticated.Before(started.Add(-time.Minute)) || authenticated.After(now.Add(time.Minute)) || authenticated.After(id.IssuedAt.Add(time.Minute)) {
		return zero, errProof
	}
	if len(claims.AMR) > 16 || len(claims.ACR) > 256 {
		return zero, errProof
	}
	for _, method := range claims.AMR {
		if method == "" || len(method) > 64 {
			return zero, errProof
		}
	}
	// The access/refresh/ID tokens are discarded. Only verified authentication
	// evidence crosses the port; interpretation as MFA belongs to tenant policy.
	return outbound.FederationProof{Issuer: id.Issuer, Subject: id.Subject, AuthenticatedAt: authenticated, ExpiresAt: id.Expiry, AMR: claims.AMR, ACR: claims.ACR}, nil
}
