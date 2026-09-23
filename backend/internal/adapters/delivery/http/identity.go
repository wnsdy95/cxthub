package http

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// IdentityBackend is a set of actions called by authentication/repository/invite handlers (app.IdentityService implements).
type IdentityBackend interface {
	TeamIdentity
	EnterpriseIdentity
	ResolveRepositoryConnection(context.Context, string, string) (app.RepositoryConnection, error)
	// Session: IDP token exchange → server session issuance/revoke, Bearer interpretation.
	Login(ctx context.Context, idpToken, label string) (domain.User, domain.Session, error)
	Logout(ctx context.Context, sessionToken string) error
	ResolveUser(ctx context.Context, bearer string) (domain.User, error)

	Authenticate(ctx context.Context, idToken string) (domain.User, error)
	UpdateProfile(ctx context.Context, u domain.User, username, nickname, loadMode, avatar, locale *string) (domain.User, error)
	CreateCLIToken(ctx context.Context, userID, label string) (domain.Session, error)
	CreateRepository(ctx context.Context, owner domain.User, name string) (domain.Repository, error)
	UpdateRepositorySettings(ctx context.Context, userID, repositoryID string, p app.RepositoryPatch) (domain.Repository, error)
	GetRepository(ctx context.Context, repositoryID string) (domain.Repository, error)
	IsPublicRepository(ctx context.Context, repositoryID string) bool
	IsOwner(ctx context.Context, repositoryID, userID string) bool
	CanTransferOwnership(ctx context.Context, repositoryID, userID string) bool
	RoleOf(ctx context.Context, repositoryID, userID string) (domain.MemberRole, bool)
	TransferOwnership(ctx context.Context, actorID, repositoryID, targetID string) (domain.Repository, error)
	UpdateMemberRole(ctx context.Context, actorID, repositoryID, targetID string, role domain.MemberRole) error
	RemoveMember(ctx context.Context, actorID, repositoryID, targetID string) error
	ListCLITokens(ctx context.Context, userID string) ([]app.CLITokenInfo, error)
	RevokeCLIToken(ctx context.Context, userID, suffix string) error
	ListWebSessions(ctx context.Context, userID string) ([]app.CLITokenInfo, error)
	RevokeWebSession(ctx context.Context, userID, suffix string) error
	PublicRepository(ctx context.Context, username, slug string) (domain.Repository, error)
	ReadableRepository(ctx context.Context, namespace, slug, viewerID string) (domain.Repository, error)
	PublicUser(ctx context.Context, username, viewerID string) (domain.User, []domain.Repository, error)
	ListRepositories(ctx context.Context, userID string) ([]domain.Repository, error)
	Invite(ctx context.Context, userID, repositoryID, email string, role domain.MemberRole, ttl time.Duration) (domain.Invite, error)
	ListInvites(ctx context.Context, userID, repositoryID string) ([]domain.Invite, error)
	AcceptInvite(ctx context.Context, user domain.User, token string) (domain.Repository, error)
	ListMembers(ctx context.Context, userID, repositoryID string) ([]domain.Membership, error)
	RevokeInvite(ctx context.Context, userID, repositoryID, token string) error

	CreateOrganization(ctx context.Context, creator domain.User, name, slug string) (domain.Organization, error)
	ListOrganizations(ctx context.Context, userID string) ([]domain.Organization, error)
	GetOrganization(ctx context.Context, organizationID string) (domain.Organization, error)
	UpdateOrganizationProfile(ctx context.Context, actorID, organizationID string, name, logo *string) (domain.Organization, error)
	PublicOrganization(ctx context.Context, slug string) (domain.Organization, []domain.Repository, error)
	OrganizationRoleOf(ctx context.Context, organizationID, userID string) (domain.OrganizationRole, bool)
	ListOrganizationMembers(ctx context.Context, actorID, organizationID string) ([]domain.OrganizationMembership, error)
	UpdateOrganizationMember(ctx context.Context, actorID, organizationID, targetID string, role domain.OrganizationRole) error
	OffboardOrganizationMember(ctx context.Context, actorID, organizationID, targetID, repositoryAccess string) error
	GetOrganizationPolicy(ctx context.Context, actorID, organizationID string) (domain.OrganizationPolicy, error)
	UpdateOrganizationPolicy(ctx context.Context, actorID string, policy domain.OrganizationPolicy) (domain.OrganizationPolicy, error)
	CreateOrganizationRepository(ctx context.Context, actor domain.User, organizationID, name string) (domain.Repository, error)
	ListOrganizationRepositories(ctx context.Context, actorID, organizationID string) ([]domain.Repository, error)
	CreateBreakGlassGrant(ctx context.Context, actorID, organizationID, repositoryID, reason string, minutes int) (domain.BreakGlassGrant, error)
	HasBreakGlassAccess(ctx context.Context, repositoryID, userID string) (bool, error)
	ListOrganizationAudit(ctx context.Context, actorID, organizationID string, limit int) ([]domain.OrganizationAuditEvent, error)
}

type ctxKey int

const userCtxKey ctxKey = iota

func userFrom(ctx context.Context) (domain.User, bool) {
	u, ok := ctx.Value(userCtxKey).(domain.User)
	return u, ok
}

// sessionCookie is the name of the HttpOnly session cookie. Tokens are passed to the browser only via this cookie, and JS cannot read it.
const sessionCookie = "cxt_session"

// bearerToken extracts the token from the Authorization: Bearer <token> header.
func bearerToken(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// requestToken finds the authentication token in the order: cookie (browser) → Authorization header (CLI).
// Cookie takes precedence, so the web UI does not store the token in JS, and the CLI continues to operate with the existing Bearer header.
func (s *Server) requestToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	return bearerToken(r)
}

// setSessionCookie stores the session token in an HttpOnly cookie (with Max-Age until expiration).
func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Domain:   s.cookie.domain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   s.cookie.secure,
		SameSite: s.cookie.sameSite,
	})
}

// clearSessionCookie immediately expires the session cookie (logout).
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		Domain:   s.cookie.domain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookie.secure,
		SameSite: s.cookie.sameSite,
	})
}

// decodeLoose is a conventional decoder that silently ignores the body if it is empty or not JSON —
// suitable for bodies containing only optional fields (e.g., display labels). Required inputs use s.decode.
func decodeLoose(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v) // include EOF for empty body — caller ignores
}

// uaLabel summarizes User-Agent as "Browser · OS" (device name tag for session list).
// Minimal identifier for distinguishing devices in session list, not precise parsing.
func uaLabel(ua string) string {
	browser := ""
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	osName := ""
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		osName = "iOS"
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		osName = "macOS"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	switch {
	case browser != "" && osName != "":
		return browser + " · " + osName
	case browser != "":
		return browser
	default:
		return osName
	}
}

// requireUser is a middleware that validates session cookie (or CLI Bearer token) and injects User into context.
func (s *Server) requireUser(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := s.requestToken(r)
		if token == "" {
			s.writeError(w, http.StatusUnauthorized, "unauthenticated", "missing session cookie or Authorization token")
			return
		}
		u, err := s.id.ResolveUser(r.Context(), token) // session token (sess_) or IDP token
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired token")
			return
		}
		fn(w, r.WithContext(inbound.WithRepositoryActor(context.WithValue(r.Context(), userCtxKey, u), u.ID)))
	}
}

// optionalUser injects user into context if token is present, otherwise passes through (public routes — anonymous remains, logged-in user validated).
func (s *Server) optionalUser(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := s.requestToken(r); token != "" {
			if u, err := s.id.ResolveUser(r.Context(), token); err == nil {
				r = r.WithContext(inbound.WithRepositoryActor(context.WithValue(r.Context(), userCtxKey, u), u.ID))
			}
		}
		fn(w, r)
	}
}

func (s *Server) registerIdentity(mux *http.ServeMux) {
	// Session exchange: IDP token (Bearer) → server session token issuance (requireUser not applied — login entry point).
	mux.HandleFunc("POST /api/v1/auth/session", s.rateLimit(20, time.Minute, s.createSession))
	// Device flow (CLI token issuance automation) — start is unauthenticated (rate limit), approve is login required.
	mux.HandleFunc("POST /api/v1/auth/device/start", s.rateLimit(10, time.Minute, s.deviceStart))
	mux.HandleFunc("POST /api/v1/auth/device/approve", s.requireUser(s.deviceApprove))
	mux.HandleFunc("POST /api/v1/auth/device/poll", s.rateLimit(40, time.Minute, s.devicePoll))
	mux.HandleFunc("DELETE /api/v1/auth/session", s.requireUser(s.logout))

	mux.HandleFunc("GET /api/v1/me", s.requireUser(s.me))
	mux.HandleFunc("GET /api/v1/me/storage", s.requireUser(s.getStorageUsage))
	mux.HandleFunc("POST /api/v1/me/storage/reconcile", s.requireUser(s.rateLimit(2, time.Minute, s.reconcileStorageUsage)))
	mux.HandleFunc("GET /api/v1/namespaces/{namespaceID}/storage", s.requireUser(s.getStorageUsage))
	mux.HandleFunc("POST /api/v1/namespaces/{namespaceID}/storage/reconcile", s.requireUser(s.rateLimit(2, time.Minute, s.reconcileStorageUsage)))
	mux.HandleFunc("PATCH /api/v1/me", s.requireUser(s.updateMe))
	mux.HandleFunc("POST /api/v1/me/cli-tokens", s.requireUser(s.createCLIToken))
	mux.HandleFunc("GET /api/v1/me/cli-tokens", s.requireUser(s.listCLITokens))
	mux.HandleFunc("DELETE /api/v1/me/cli-tokens/{suffix}", s.requireUser(s.revokeCLIToken))
	mux.HandleFunc("GET /api/v1/me/sessions", s.requireUser(s.listWebSessions))
	mux.HandleFunc("DELETE /api/v1/me/sessions/{suffix}", s.requireUser(s.revokeWebSession))
	// Anonymous public read: username/slug → public repository interpretation (private results in 404 — non-existence not exposed).
	mux.HandleFunc("GET /api/v1/public/repositories/{username}/{slug}", s.optionalUser(s.publicRepository))
	mux.HandleFunc("GET /api/v1/public/users/{username}", s.optionalUser(s.publicUser))
	mux.HandleFunc("GET /api/v1/public/users/{username}/contributions", s.optionalUser(s.userContributions))
	mux.HandleFunc("GET /api/v1/public/users/{username}/activity", s.optionalUser(s.userActivity))
	mux.HandleFunc("POST /api/v1/repositories", s.requireUser(s.createRepository))
	mux.HandleFunc("PATCH /api/v1/repositories/{repositoryID}", s.requireUser(s.patchRepository))
	mux.HandleFunc("POST /api/v1/repositories/{repositoryID}/transfer", s.requireUser(s.transferRepository))
	mux.HandleFunc("POST /api/v1/repositories/{repositoryID}/sync-visibility", s.requireUser(s.syncVisibility))
	mux.HandleFunc("GET /api/v1/repositories", s.requireUser(s.listRepositories))
	mux.HandleFunc("GET /api/v1/repositories/{repositoryID}/notifications", s.requireUser(s.listNotifications))
	mux.HandleFunc("POST /api/v1/repositories/{repositoryID}/notifications/{notificationID}/retry", s.requireUser(s.retryNotification))
	mux.HandleFunc("GET /api/v1/repositories/{repositoryID}/members", s.requireUser(s.listMembers))
	mux.HandleFunc("PATCH /api/v1/repositories/{repositoryID}/members/{userID}", s.requireUser(s.patchMember))
	mux.HandleFunc("DELETE /api/v1/repositories/{repositoryID}/members/{userID}", s.requireUser(s.deleteMember))
	mux.HandleFunc("POST /api/v1/repositories/{repositoryID}/invites", s.requireUser(s.createInvite))
	mux.HandleFunc("GET /api/v1/repositories/{repositoryID}/invites", s.requireUser(s.listInvites))
	mux.HandleFunc("POST /api/v1/repositories/{repositoryID}/invites/{token}/revoke", s.requireUser(s.revokeInvite))
	mux.HandleFunc("POST /api/v1/invites/{token}/accept", s.requireUser(s.acceptInvite))

	s.registerTeamRoutes(mux)
	s.registerEnterpriseRoutes(mux)
	mux.HandleFunc("GET /api/v1/repository-connections", s.optionalUser(s.resolveRepositoryConnection))

	// Organization administration plane. Organization roles administer namespaces,
	// people, and policy; repository context still uses Repository membership.
	mux.HandleFunc("POST /api/v1/organizations", s.requireUser(s.createOrganization))
	mux.HandleFunc("GET /api/v1/organizations", s.requireUser(s.listOrganizations))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}", s.requireUser(s.getOrganization))
	mux.HandleFunc("PATCH /api/v1/organizations/{organizationID}", s.requireUser(s.patchOrganization))
	mux.HandleFunc("GET /api/v1/public/organizations/{slug}", s.publicOrganization)
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/members", s.requireUser(s.listOrganizationMembers))
	mux.HandleFunc("PATCH /api/v1/organizations/{organizationID}/members/{userID}", s.requireUser(s.patchOrganizationMember))
	mux.HandleFunc("DELETE /api/v1/organizations/{organizationID}/members/{userID}", s.requireUser(s.deleteOrganizationMember))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/policy", s.requireUser(s.getOrganizationPolicy))
	mux.HandleFunc("PATCH /api/v1/organizations/{organizationID}/policy", s.requireUser(s.patchOrganizationPolicy))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/repositories", s.requireUser(s.listOrganizationRepositories))
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/repositories", s.requireUser(s.createOrganizationRepository))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/audit", s.requireUser(s.listOrganizationAudit))
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/break-glass", s.requireUser(s.createBreakGlassGrant))
}

// rateLimit uses a shared GCRA allowance. Proxy forwarding headers are never
// trusted implicitly; deployments should additionally enforce source limits at the edge.
func (s *Server) rateLimit(limit int, window time.Duration, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.runtime == nil {
			s.writeError(w, 503, "unavailable", "shared request allowance unavailable")
			return
		}
		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		allowed, err := s.runtime.AllowRequest(ctx, "http:"+r.URL.Path+":"+domain.HashToken(ip), limit, window, time.Time{})
		if err != nil {
			s.writeError(w, 503, "unavailable", "request allowance unavailable")
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", fmt.Sprint(max(1, int(window.Seconds()))))
			s.writeError(w, 429, "rate_limited", "Too many requests — please retry later")
			return
		}
		fn(w, r)
	}
}

// --- Handlers ---

// sessionResponse is the response to POST /auth/session. Session tokens are passed only via HttpOnly cookies and are not included in the body (to prevent JS exposure). The client only sees user/expires_at.
type sessionResponse struct {
	User      domain.User `json:"user"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// createSession exchanges an IDP token (Authorization: Bearer) for a server session and stores it as an HttpOnly cookie (for login). If relogging (if the request includes a previous session cookie), the previous session is discarded — the cookie is overwritten by the new token, leaving an orphan session on the server without creating a new one. Other device sessions are not affected.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	idp := bearerToken(r) // Bearer token for exchange IDP is received only from the header (to prevent confusion with cookies).
	if idp == "" {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "missing Authorization: Bearer <idp token>")
		return
	}
	replaced := "" // existing session in the same browser (if any)
	if c, cerr := r.Cookie(sessionCookie); cerr == nil {
		replaced = c.Value
	}
	u, sess, err := s.id.Login(r.Context(), idp, uaLabel(r.UserAgent()))
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	if replaced != "" && replaced != sess.Token {
		_ = s.id.Logout(r.Context(), replaced) // clean up replaced previous session (idempotent — no-op if invalid cookie)
	}
	s.setSessionCookie(w, sess.Token, sess.ExpiresAt)
	s.respond(w, sessionResponse{User: u, ExpiresAt: sess.ExpiresAt}, nil)
}

// logout deletes the current session and expires the cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	err := s.id.Logout(r.Context(), s.requestToken(r))
	s.clearSessionCookie(w)
	s.respond(w, map[string]string{"status": "logged_out"}, err)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	s.respond(w, u, nil)
}

// updateMe updates account settings. nil fields are unchanged (partial PATCH).
// username is a heavy change that alters the URL (422 format error / 409 conflict), nickname is free.
func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username *string `json:"username"`
		Nickname *string `json:"nickname"`
		LoadMode *string `json:"load_mode"`
		Avatar   *string `json:"avatar"`
		Locale   *string `json:"locale"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.UpdateProfile(r.Context(), u, body.Username, body.Nickname, body.LoadMode, body.Avatar, body.Locale)
	s.respond(w, out, err)
}

// patchRepository updates repository settings (visibility·policy — owner only, partial PATCH).
func (s *Server) patchRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Visibility       *string `json:"visibility"`
		SecretsPolicy    *string `json:"secrets_policy"`
		SettingsPolicy   *string `json:"settings_policy"`
		GHVisibilitySync *bool   `json:"gh_visibility_sync"`
		Archived         *bool   `json:"archived"`
		WebhookURL       *string `json:"webhook_url"`
		Slug             *string `json:"slug"`
		PublicRole       *string `json:"public_role"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	var patch app.RepositoryPatch
	if body.Visibility != nil {
		v := domain.Visibility(*body.Visibility)
		patch.Visibility = &v
	}
	patch.SecretsPolicy = body.SecretsPolicy
	patch.SettingsPolicy = body.SettingsPolicy
	patch.GHVisibilitySync = body.GHVisibilitySync
	patch.Archived = body.Archived
	patch.WebhookURL = body.WebhookURL
	patch.Slug = body.Slug
	patch.PublicRole = body.PublicRole
	u, _ := userFrom(r.Context())
	out, err := s.id.UpdateRepositorySettings(r.Context(), u.ID, r.PathValue("repositoryID"), patch)
	if err == nil && body.GHVisibilitySync != nil && *body.GHVisibilitySync {
		// syncRepository runs once immediately upon enabling sync — success updates response, failure maintains setting.
		if synced, serr := s.b.SyncRepositoryVisibility(r.Context(), r.PathValue("repositoryID")); serr == nil {
			out = synced
		}
	}
	s.respond(w, s.repositoryAccess(r.Context(), u.ID, out), err)
}

// transferRepository transfers ownership to existing members (creator retains rights — URL changes).
func (s *Server) transferRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToUserID string `json:"to_user_id"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.TransferOwnership(r.Context(), u.ID, r.PathValue("repositoryID"), body.ToUserID)
	s.respond(w, out, err)
}

// syncVisibility manually runs GitHub public state sync (owner only).
func (s *Server) syncVisibility(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	if !s.id.IsOwner(r.Context(), r.PathValue("repositoryID"), u.ID) {
		s.writeError(w, http.StatusForbidden, "forbidden", "only owner can run sync")
		return
	}
	out, err := s.b.SyncRepositoryVisibility(r.Context(), r.PathValue("repositoryID"))
	s.respond(w, out, err)
}

// createCLIToken generates a CLI token for use (exposed only once — `cxt login <token>`).
// body.label (optional) is the device display name — the name tag used on the web issuance screen.
func (s *Server) createCLIToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	_ = decodeLoose(r, &body) // backward compatibility for existing calls without body (label is optional)
	u, _ := userFrom(r.Context())
	sess, err := s.id.CreateCLIToken(r.Context(), u.ID, body.Label)
	s.respond(w, map[string]any{"token": sess.Token, "expires_at": sess.ExpiresAt}, err)
}

// listCLITokens returns the list of your CLI tokens (values are suffixes only — no reissuance allowed).
func (s *Server) listCLITokens(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListCLITokens(r.Context(), u.ID)
	s.respond(w, out, err)
}

// revokeCLIToken revokes your CLI token by suffix.
func (s *Server) revokeCLIToken(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RevokeCLIToken(r.Context(), u.ID, r.PathValue("suffix"))
	s.respond(w, map[string]string{"status": "revoked"}, err)
}

// listWebSessions returns the list of your web login sessions. The current session is marked as current.
func (s *Server) listWebSessions(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	list, err := s.id.ListWebSessions(r.Context(), u.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	cur := s.requestToken(r)
	type item struct {
		Suffix    string    `json:"suffix"`
		Label     string    `json:"label,omitempty"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
		Current   bool      `json:"current"`
	}
	out := make([]item, 0, len(list))
	for _, t := range list {
		out = append(out, item{Suffix: t.Suffix, Label: t.Label, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Current: strings.HasSuffix(cur, t.Suffix)})
	}
	s.respond(w, out, nil)
}

// revokeWebSession revokes your web session by suffix (logs out from other devices — your session can also be revoked).
func (s *Server) revokeWebSession(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RevokeWebSession(r.Context(), u.ID, r.PathValue("suffix"))
	s.respond(w, map[string]string{"status": "revoked"}, err)
}

// Public endpoints do not directly serialize internal domain objects. In the Repository, webhook capability URLs and policy/synchronization status are included, and in the User, personal settings are also included. Therefore, new fields added via allowlist projection are not automatically exposed.
type publicUserView struct {
	Name      string    `json:"name"`
	Username  string    `json:"username"`
	Nickname  string    `json:"nickname,omitempty"`
	Avatar    string    `json:"avatar,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func publicUserViewOf(u domain.User) publicUserView {
	return publicUserView{
		Name: u.Name, Username: u.Username, Nickname: u.Nickname,
		Avatar: u.Avatar, CreatedAt: u.CreatedAt,
	}
}

type publicRepositoryView struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Slug          string            `json:"slug"`
	OwnerUsername string            `json:"owner_username"`
	Visibility    domain.Visibility `json:"visibility,omitempty"`
	PublicRole    string            `json:"public_role,omitempty"`
	Archived      bool              `json:"archived,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

func publicRepositoryViewOf(repositoryRecord domain.Repository) publicRepositoryView {
	return publicRepositoryView{
		ID: repositoryRecord.ID, Name: repositoryRecord.Name, Slug: repositoryRecord.Slug, OwnerUsername: repositoryRecord.OwnerUsername,
		Visibility: repositoryRecord.Visibility, PublicRole: repositoryRecord.PublicRole, Archived: repositoryRecord.Archived,
		CreatedAt: repositoryRecord.CreatedAt,
	}
}

// publicRepository is the entry point for anonymous public read access — only public repositories are interpreted (any other results in 404).
func (s *Server) publicRepository(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	out, err := s.id.ReadableRepository(r.Context(), r.PathValue("username"), r.PathValue("slug"), viewer.ID)
	s.respond(w, publicRepositoryViewOf(out), err)
}

// publicUser is the user profile entry point (/<username>) — user + publicly accessible repositories.
// Accessible by anonymous users (only public repositories visible), includes private if self.
func (s *Server) publicUser(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	u, repositoryList, err := s.id.PublicUser(r.Context(), r.PathValue("username"), viewer.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	if repositoryList == nil {
		repositoryList = []domain.Repository{}
	}
	publicRepositories := make([]publicRepositoryView, 0, len(repositoryList))
	for _, repositoryRecord := range repositoryList {
		publicRepositories = append(publicRepositories, publicRepositoryViewOf(repositoryRecord))
	}
	s.respond(w, map[string]any{"user": publicUserViewOf(u), "repositories": publicRepositories}, nil)
}

// userContributions is user profile contribution heatmap data — daily commit counts per visible repository (PublicUser same visibility: anonymous only public, self includes private).
func (s *Server) userContributions(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	_, repositoryList, err := s.id.PublicUser(r.Context(), r.PathValue("username"), viewer.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	ids := make([]string, 0, len(repositoryList))
	for _, repositoryRecord := range repositoryList {
		ids = append(ids, repositoryRecord.ID)
	}
	counts, err := s.b.Contributions(r.Context(), ids)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	type day struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}
	days := make([]day, 0, len(counts))
	total := 0
	for d, c := range counts {
		days = append(days, day{Date: d, Count: c})
		total += c
	}
	s.respond(w, map[string]any{"days": days, "total": total}, nil)
}

// userActivity is user profile activity feed — monthly commit bundles + repository creation (PublicUser same visibility).
func (s *Server) userActivity(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	_, repositoryList, err := s.id.PublicUser(r.Context(), r.PathValue("username"), viewer.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	months, err := s.b.Activity(r.Context(), repositoryList)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	if months == nil {
		months = []domain.ActivityMonth{}
	}
	s.respond(w, map[string]any{"months": months}, nil)
}

// patchMember changes member role (owner only, constructor role fixed).
func (s *Server) patchMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	err := s.id.UpdateMemberRole(r.Context(), u.ID, r.PathValue("repositoryID"), r.PathValue("userID"), domain.MemberRole(body.Role))
	s.respond(w, map[string]string{"status": "updated"}, err)
}

// deleteMember removes a member (owner can remove anyone, self can leave — constructor cannot).
func (s *Server) deleteMember(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RemoveMember(r.Context(), u.ID, r.PathValue("repositoryID"), r.PathValue("userID"))
	s.respond(w, map[string]string{"status": "removed"}, err)
}

func (s *Server) createRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	repositoryRecord, err := s.id.CreateRepository(r.Context(), u, body.Name)
	s.respond(w, s.repositoryAccess(r.Context(), u.ID, repositoryRecord), err)
}

func (s *Server) listRepositories(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListRepositories(r.Context(), u.ID)
	// GitHub sync lazy TTL (1 hour): old repositories refreshed in background (response unblocked, in-flight guard to prevent duplicate execution). Result reflected in next query.
	if err == nil {
		for _, repositoryRecord := range out {
			if repositoryRecord.GHVisibilitySync && (repositoryRecord.GHSyncedAt == nil || time.Since(*repositoryRecord.GHSyncedAt) > time.Hour) {
				s.kickVisibilitySync(repositoryRecord.ID)
			}
		}
	}
	s.respond(w, s.repositoryAccessList(r.Context(), u.ID, out), err)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListMembers(r.Context(), u.ID, r.PathValue("repositoryID"))
	s.respond(w, out, err)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email         string `json:"email"`
		Role          string `json:"role"`
		ExpiresInDays int    `json:"expires_in_days"` // 0=never, negative values result in ErrValidation
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	ttl := time.Duration(body.ExpiresInDays) * 24 * time.Hour
	inv, err := s.id.Invite(r.Context(), u.ID, r.PathValue("repositoryID"), body.Email, domain.MemberRole(body.Role), ttl)
	s.respond(w, inv, err)
}

// listInvites returns the invite list (maintainer or above — invite management screen).
func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListInvites(r.Context(), u.ID, r.PathValue("repositoryID"))
	s.respond(w, out, err)
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	repositoryRecord, err := s.id.AcceptInvite(r.Context(), u, r.PathValue("token"))
	s.respond(w, repositoryRecord, err)
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RevokeInvite(r.Context(), u.ID, r.PathValue("repositoryID"), r.PathValue("token"))
	s.respond(w, map[string]string{"status": "revoked"}, err)
}

type publicOrganizationResponse struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Slug         string                 `json:"slug"`
	Logo         string                 `json:"logo,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	Repositories []publicRepositoryView `json:"repositories"`
}

func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	user, _ := userFrom(r.Context())
	organization, err := s.id.CreateOrganization(r.Context(), user, body.Name, body.Slug)
	s.respond(w, organization, err)
}

type organizationAccessView struct {
	domain.Organization
	EffectiveRole domain.OrganizationRole `json:"effective_role,omitempty"`
}

func (s *Server) listOrganizations(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	organizations, err := s.id.ListOrganizations(r.Context(), user.ID)
	result := make([]organizationAccessView, 0, len(organizations))
	for _, organization := range organizations {
		role, _ := s.id.OrganizationRoleOf(r.Context(), organization.ID, user.ID)
		result = append(result, organizationAccessView{Organization: organization, EffectiveRole: role})
	}
	s.respond(w, result, err)
}

func (s *Server) getOrganization(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	organizationID := r.PathValue("organizationID")
	if _, ok := s.id.OrganizationRoleOf(r.Context(), organizationID, user.ID); !ok {
		s.respond(w, domain.Organization{}, domain.ErrForbidden)
		return
	}
	organization, err := s.id.GetOrganization(r.Context(), organizationID)
	s.respond(w, organization, err)
}

func (s *Server) patchOrganization(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name *string `json:"name"`
		Logo *string `json:"logo"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Name == nil && body.Logo == nil {
		s.respond(w, domain.Organization{}, domain.ErrValidation)
		return
	}
	user, _ := userFrom(r.Context())
	organization, err := s.id.UpdateOrganizationProfile(r.Context(), user.ID, r.PathValue("organizationID"), body.Name, body.Logo)
	s.respond(w, organization, err)
}

func (s *Server) publicOrganization(w http.ResponseWriter, r *http.Request) {
	organization, repositories, err := s.id.PublicOrganization(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.respond(w, publicOrganizationResponse{}, err)
		return
	}
	publicRepositories := make([]publicRepositoryView, 0, len(repositories))
	for _, repository := range repositories {
		publicRepositories = append(publicRepositories, publicRepositoryViewOf(repository))
	}
	s.respond(w, publicOrganizationResponse{
		ID: organization.ID, Name: organization.Name, Slug: organization.Slug,
		Logo: organization.Logo, CreatedAt: organization.CreatedAt, Repositories: publicRepositories,
	}, nil)
}

func (s *Server) listOrganizationMembers(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	members, err := s.id.ListOrganizationMembers(r.Context(), user.ID, r.PathValue("organizationID"))
	s.respond(w, members, err)
}

func (s *Server) patchOrganizationMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	user, _ := userFrom(r.Context())
	err := s.id.UpdateOrganizationMember(r.Context(), user.ID, r.PathValue("organizationID"), r.PathValue("userID"), domain.OrganizationRole(body.Role))
	s.respond(w, map[string]string{"status": "updated"}, err)
}

func (s *Server) deleteOrganizationMember(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	err := s.id.OffboardOrganizationMember(r.Context(), user.ID, r.PathValue("organizationID"), r.PathValue("userID"), r.URL.Query().Get("repository_access"))
	s.respond(w, map[string]string{"status": "removed"}, err)
}

func (s *Server) getOrganizationPolicy(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	policy, err := s.id.GetOrganizationPolicy(r.Context(), user.ID, r.PathValue("organizationID"))
	s.respond(w, policy, err)
}

func (s *Server) patchOrganizationPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepositoryCreation          *domain.OrganizationRepositoryCreation `json:"repository_creation"`
		DefaultRepositoryVisibility *domain.Visibility                     `json:"default_repository_visibility"`
		AllowPublicRepositories     *bool                                  `json:"allow_public_repositories"`
		BreakGlassEnabled           *bool                                  `json:"break_glass_enabled"`
		BreakGlassMaxMinutes        *int                                   `json:"break_glass_max_minutes"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	user, _ := userFrom(r.Context())
	policy, err := s.id.GetOrganizationPolicy(r.Context(), user.ID, r.PathValue("organizationID"))
	if err != nil {
		s.respond(w, domain.OrganizationPolicy{}, err)
		return
	}
	if body.RepositoryCreation != nil {
		policy.RepositoryCreation = *body.RepositoryCreation
	}
	if body.DefaultRepositoryVisibility != nil {
		policy.DefaultRepositoryVisibility = *body.DefaultRepositoryVisibility
	}
	if body.AllowPublicRepositories != nil {
		policy.AllowPublicRepositories = *body.AllowPublicRepositories
	}
	if body.BreakGlassEnabled != nil {
		policy.BreakGlassEnabled = *body.BreakGlassEnabled
	}
	if body.BreakGlassMaxMinutes != nil {
		policy.BreakGlassMaxMinutes = *body.BreakGlassMaxMinutes
	}
	updated, err := s.id.UpdateOrganizationPolicy(r.Context(), user.ID, policy)
	s.respond(w, updated, err)
}

func (s *Server) listOrganizationRepositories(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	repositories, err := s.id.ListOrganizationRepositories(r.Context(), user.ID, r.PathValue("organizationID"))
	s.respond(w, s.repositoryAccessList(r.Context(), user.ID, repositories), err)
}

func (s *Server) createOrganizationRepository(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	user, _ := userFrom(r.Context())
	repository, err := s.id.CreateOrganizationRepository(r.Context(), user, r.PathValue("organizationID"), body.Name)
	s.respond(w, s.repositoryAccess(r.Context(), user.ID, repository), err)
}

func (s *Server) listOrganizationAudit(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	events, err := s.id.ListOrganizationAudit(r.Context(), user.ID, r.PathValue("organizationID"), 100)
	s.respond(w, events, err)
}

func (s *Server) createBreakGlassGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepositoryID string `json:"repository_id"`
		Reason       string `json:"reason"`
		Minutes      int    `json:"minutes"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	user, _ := userFrom(r.Context())
	grant, err := s.id.CreateBreakGlassGrant(r.Context(), user.ID, r.PathValue("organizationID"), body.RepositoryID, body.Reason, body.Minutes)
	s.respond(w, grant, err)
}

func (s *Server) resolveRepositoryConnection(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	out, err := s.id.ResolveRepositoryConnection(r.Context(), user.ID, r.URL.Query().Get("remote_url"))
	s.respond(w, out, err)
}
