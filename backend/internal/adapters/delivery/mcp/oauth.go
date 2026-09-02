package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const (
	authorizationTTL     = 10 * time.Minute
	authorizationCodeTTL = 5 * time.Minute
)

func (s *Server) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource": s.resource, "authorization_servers": []string{s.publicURL},
		"bearer_methods_supported": []string{"header"}, "scopes_supported": []string{readScope},
	})
}

func (s *Server) authorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.publicURL,
		"authorization_endpoint":                s.publicURL + "/oauth/authorize",
		"token_endpoint":                        s.publicURL + "/oauth/token",
		"revocation_endpoint":                   s.publicURL + "/oauth/revoke",
		"registration_endpoint":                 s.publicURL + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{readScope},
	})
}

type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func validRedirectURI(raw string) bool {
	if len(raw) == 0 || len(raw) > 512 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	if !hasMediaType(r, "application/json") {
		writeOAuthError(w, http.StatusUnsupportedMediaType, "invalid_client_metadata", "Content-Type application/json is required")
		return
	}
	var body registrationRequest
	if err := decodeSingleJSON(w, r, 64<<10, &body); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON registration document")
		return
	}
	name := strings.TrimSpace(body.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if len(name) > 128 || len(body.RedirectURIs) == 0 || len(body.RedirectURIs) > 16 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "one to sixteen redirect_uris are required")
		return
	}
	for _, redirect := range body.RedirectURIs {
		if !validRedirectURI(redirect) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be HTTPS or an HTTP loopback address")
			return
		}
	}
	if body.TokenEndpointAuthMethod != "" && body.TokenEndpointAuthMethod != "none" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public PKCE clients are supported")
		return
	}
	if len(body.GrantTypes) > 0 {
		if !slices.Contains(body.GrantTypes, "authorization_code") {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "authorization_code grant is required")
			return
		}
		for _, grant := range body.GrantTypes {
			if grant != "authorization_code" && grant != "refresh_token" {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant type")
				return
			}
		}
	}
	if len(body.ResponseTypes) > 0 && !slices.Contains(body.ResponseTypes, "code") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "code response type is required")
		return
	}
	client := domain.OAuthClient{
		ID: domain.NewID("mcp_client_"), Name: name,
		RedirectURIs: slices.Clone(body.RedirectURIs), CreatedAt: time.Now().UTC(),
	}
	if err := s.oauth.CreateOAuthClient(r.Context(), client); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "client registration failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id": client.ID, "client_name": client.Name, "redirect_uris": client.RedirectURIs,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none", "client_id_issued_at": client.CreatedAt.Unix(),
	})
}

func exactRedirect(client domain.OAuthClient, redirect string) bool {
	return slices.Contains(client.RedirectURIs, redirect)
}

func validPKCEValue(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("-._~", r) {
			return false
		}
	}
	return true
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client, err := s.oauth.GetOAuthClient(r.Context(), q.Get("client_id"))
	if err != nil || q.Get("response_type") != "code" || !exactRedirect(client, q.Get("redirect_uri")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "unknown client, response type, or redirect URI")
		return
	}
	if q.Get("code_challenge_method") != "S256" || !validPKCEValue(q.Get("code_challenge")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "S256 PKCE is required")
		return
	}
	resource := q.Get("resource")
	if subtle.ConstantTimeCompare([]byte(resource), []byte(s.resource)) != 1 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must match the CXTHub MCP endpoint")
		return
	}
	scope := strings.TrimSpace(q.Get("scope"))
	if scope == "" {
		scope = readScope
	}
	if scope != readScope || len(q.Get("state")) > 512 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "only mcp:read is supported")
		return
	}
	now := time.Now().UTC()
	req := domain.OAuthAuthorizationRequest{
		ID: domain.NewID("oauth_req_"), ClientID: client.ID, RedirectURI: q.Get("redirect_uri"),
		State: q.Get("state"), CodeChallenge: q.Get("code_challenge"), Resource: resource,
		Scope: scope, CreatedAt: now, ExpiresAt: now.Add(authorizationTTL),
	}
	if err := s.oauth.CreateOAuthAuthorizationRequest(r.Context(), req); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "authorization request could not be stored")
		return
	}
	http.Redirect(w, r, s.publicURL+"/connect/mcp?request="+url.QueryEscape(req.ID), http.StatusSeeOther)
}

func (s *Server) getConsentRequest(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requestUser(r, true); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "sign in to approve this connector"}})
		return
	}
	req, err := s.oauth.GetOAuthAuthorizationRequest(r.Context(), r.PathValue("requestID"))
	if err != nil || !time.Now().UTC().Before(req.ExpiresAt) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "authorization request expired"}})
		return
	}
	client, err := s.oauth.GetOAuthClient(r.Context(), req.ClientID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "OAuth client not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": req.ID, "client_name": client.Name, "scope": req.Scope,
		"resource": req.Resource, "redirect_uri": req.RedirectURI, "expires_at": req.ExpiresAt,
	})
}

func (s *Server) validConsentCSRF(r *http.Request) bool {
	if r.Header.Get("X-Cxt-CSRF") != "1" || !hasMediaType(r, "application/json") {
		return false
	}
	origin := strings.TrimRight(r.Header.Get("Origin"), "/")
	return origin != "" && subtle.ConstantTimeCompare([]byte(origin), []byte(s.publicURL)) == 1
}

func redirectWithOAuthResult(raw, state, code, oauthErr string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if code != "" {
		q.Set("code", code)
	} else {
		q.Set("error", oauthErr)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Server) decideConsentRequest(w http.ResponseWriter, r *http.Request) {
	user, err := s.requestUser(r, true)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "sign in required"}})
		return
	}
	if !s.validConsentCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "csrf_rejected", "message": "same-origin JSON consent required"}})
		return
	}
	var body struct {
		Approve bool `json:"approve"`
	}
	if err := decodeSingleJSON(w, r, 8<<10, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "validation", "message": "invalid consent decision"}})
		return
	}
	requestID := r.PathValue("requestID")
	consent, err := s.oauth.GetOAuthAuthorizationRequest(r.Context(), requestID)
	if err != nil || !time.Now().UTC().Before(consent.ExpiresAt) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "authorization request expired or was already used"}})
		return
	}
	var redirect string
	if body.Approve {
		rawCode := domain.NewID("oauth_code_")
		code, approveErr := s.oauth.ApproveOAuthAuthorizationRequest(
			r.Context(), requestID, user.ID, domain.HashToken(rawCode), time.Now().UTC().Add(authorizationCodeTTL),
		)
		if approveErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "authorization request expired or was already used"}})
			return
		}
		redirect, err = redirectWithOAuthResult(code.RedirectURI, consent.State, rawCode, "")
	} else {
		req, denyErr := s.oauth.DenyOAuthAuthorizationRequest(r.Context(), requestID)
		if denyErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "authorization request expired or was already used"}})
			return
		}
		redirect, err = redirectWithOAuthResult(req.RedirectURI, req.State, "", "access_denied")
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "internal", "message": "redirect could not be created"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect_url": redirect})
}

// revokeToken implements RFC 7009 for public PKCE clients. A valid client may
// revoke only tokens bound to its own client ID. Unknown or already-revoked
// tokens deliberately return success so the endpoint cannot be used as a
// token-existence oracle.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if !hasMediaType(r, "application/x-www-form-urlencoded") {
		writeOAuthError(w, http.StatusUnsupportedMediaType, "invalid_request", "Content-Type application/x-www-form-urlencoded is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid revocation request")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if _, err := s.oauth.GetOAuthClient(r.Context(), clientID); err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown public client")
		return
	}
	if err := s.identity.RevokeMCPToken(r.Context(), r.PostForm.Get("token"), clientID); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "token revocation failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if !hasMediaType(r, "application/x-www-form-urlencoded") {
		writeOAuthError(w, http.StatusUnsupportedMediaType, "invalid_request", "Content-Type application/x-www-form-urlencoded is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid token request")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if _, err := s.oauth.GetOAuthClient(r.Context(), clientID); err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown public client")
		return
	}
	if r.PostForm.Get("resource") != s.resource {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must match the CXTHub MCP endpoint")
		return
	}
	var pair domain.OAuthTokenPair
	var err error
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		verifier := r.PostForm.Get("code_verifier")
		if !validPKCEValue(verifier) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "valid PKCE verifier required")
			return
		}
		code, consumeErr := s.oauth.ConsumeOAuthAuthorizationCode(
			r.Context(), domain.HashToken(r.PostForm.Get("code")), clientID, r.PostForm.Get("redirect_uri"), pkceChallenge(verifier),
		)
		if consumeErr != nil || code.Resource != s.resource || code.Scope != readScope {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid, expired, or already used")
			return
		}
		pair, err = s.identity.IssueMCPTokenPair(r.Context(), code.UserID, clientID)
	case "refresh_token":
		pair, err = s.identity.RefreshMCPAccessToken(r.Context(), r.PostForm.Get("refresh_token"), clientID)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code and refresh_token are supported")
		return
	}
	if err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "token grant is invalid or expired")
		} else {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "token issuance failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": pair.AccessToken, "token_type": "Bearer", "expires_in": pair.ExpiresIn,
		"refresh_token": pair.RefreshToken, "scope": pair.Scope,
	})
}
