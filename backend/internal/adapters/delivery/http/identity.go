package http

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// IdentityBackend is a set of actions called by authentication/workspace/invite handlers (app.IdentityService implements).
type IdentityBackend interface {
	// Session: IDP token exchange → server session issuance/revoke, Bearer interpretation.
	Login(ctx context.Context, idpToken, label string) (domain.User, domain.Session, error)
	Logout(ctx context.Context, sessionToken string) error
	ResolveUser(ctx context.Context, bearer string) (domain.User, error)

	Authenticate(ctx context.Context, idToken string) (domain.User, error)
	UpdateProfile(ctx context.Context, u domain.User, username, nickname, loadMode, avatar, locale *string) (domain.User, error)
	CreateCLIToken(ctx context.Context, userID, label string) (domain.Session, error)
	CreateWorkspace(ctx context.Context, owner domain.User, name string) (domain.Workspace, error)
	UpdateWorkspaceSettings(ctx context.Context, userID, workspaceID string, p app.WorkspacePatch) (domain.Workspace, error)
	GetWorkspace(ctx context.Context, workspaceID string) (domain.Workspace, error)
	IsPublicWorkspace(ctx context.Context, workspaceID string) bool
	IsOwner(ctx context.Context, workspaceID, userID string) bool
	RoleOf(ctx context.Context, workspaceID, userID string) (domain.MemberRole, bool)
	TransferOwnership(ctx context.Context, actorID, workspaceID, targetID string) (domain.Workspace, error)
	UpdateMemberRole(ctx context.Context, actorID, workspaceID, targetID string, role domain.MemberRole) error
	RemoveMember(ctx context.Context, actorID, workspaceID, targetID string) error
	ListCLITokens(ctx context.Context, userID string) ([]app.CLITokenInfo, error)
	RevokeCLIToken(ctx context.Context, userID, suffix string) error
	ListWebSessions(ctx context.Context, userID string) ([]app.CLITokenInfo, error)
	RevokeWebSession(ctx context.Context, userID, suffix string) error
	PublicWorkspace(ctx context.Context, username, slug string) (domain.Workspace, error)
	PublicUser(ctx context.Context, username, viewerID string) (domain.User, []domain.Workspace, error)
	ListWorkspaces(ctx context.Context, userID string) ([]domain.Workspace, error)
	Invite(ctx context.Context, userID, workspaceID, email string, role domain.MemberRole, ttl time.Duration) (domain.Invite, error)
	ListInvites(ctx context.Context, userID, workspaceID string) ([]domain.Invite, error)
	AcceptInvite(ctx context.Context, user domain.User, token string) (domain.Workspace, error)
	ListMembers(ctx context.Context, userID, workspaceID string) ([]domain.Membership, error)
	RevokeInvite(ctx context.Context, userID, workspaceID, token string) error
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
		fn(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
	}
}

// optionalUser injects user into context if token is present, otherwise passes through (public routes — anonymous remains, logged-in user validated).
func (s *Server) optionalUser(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := s.requestToken(r); token != "" {
			if u, err := s.id.ResolveUser(r.Context(), token); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
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
	mux.HandleFunc("PATCH /api/v1/me", s.requireUser(s.updateMe))
	mux.HandleFunc("POST /api/v1/me/cli-tokens", s.requireUser(s.createCLIToken))
	mux.HandleFunc("GET /api/v1/me/cli-tokens", s.requireUser(s.listCLITokens))
	mux.HandleFunc("DELETE /api/v1/me/cli-tokens/{suffix}", s.requireUser(s.revokeCLIToken))
	mux.HandleFunc("GET /api/v1/me/sessions", s.requireUser(s.listWebSessions))
	mux.HandleFunc("DELETE /api/v1/me/sessions/{suffix}", s.requireUser(s.revokeWebSession))
	// Anonymous public read: username/slug → public workspace interpretation (private results in 404 — non-existence not exposed).
	mux.HandleFunc("GET /api/v1/public/workspaces/{username}/{slug}", s.publicWorkspace)
	mux.HandleFunc("GET /api/v1/public/users/{username}", s.optionalUser(s.publicUser))
	mux.HandleFunc("GET /api/v1/public/users/{username}/contributions", s.optionalUser(s.userContributions))
	mux.HandleFunc("GET /api/v1/public/users/{username}/activity", s.optionalUser(s.userActivity))
	mux.HandleFunc("POST /api/v1/workspaces", s.requireUser(s.createWorkspace))
	mux.HandleFunc("PATCH /api/v1/workspaces/{wsID}", s.requireUser(s.patchWorkspace))
	mux.HandleFunc("POST /api/v1/workspaces/{wsID}/transfer", s.requireUser(s.transferWorkspace))
	mux.HandleFunc("POST /api/v1/workspaces/{wsID}/sync-visibility", s.requireUser(s.syncVisibility))
	mux.HandleFunc("GET /api/v1/workspaces", s.requireUser(s.listWorkspaces))
	mux.HandleFunc("GET /api/v1/workspaces/{wsID}/members", s.requireUser(s.listMembers))
	mux.HandleFunc("PATCH /api/v1/workspaces/{wsID}/members/{userID}", s.requireUser(s.patchMember))
	mux.HandleFunc("DELETE /api/v1/workspaces/{wsID}/members/{userID}", s.requireUser(s.deleteMember))
	mux.HandleFunc("POST /api/v1/workspaces/{wsID}/invites", s.requireUser(s.createInvite))
	mux.HandleFunc("GET /api/v1/workspaces/{wsID}/invites", s.requireUser(s.listInvites))
	mux.HandleFunc("POST /api/v1/workspaces/{wsID}/invites/{token}/revoke", s.requireUser(s.revokeInvite))
	mux.HandleFunc("POST /api/v1/invites/{token}/accept", s.requireUser(s.acceptInvite))
}

// rateLimit is a simple in-memory sliding window that allows limit requests per IP window.
// It's used for indiscriminate brute force/spam defense at the login entry point (in a distributed environment, the front-end LB/gateway handles this).
func (s *Server) rateLimit(limit int, window time.Duration, fn http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	hits := map[string][]time.Time{}
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			ip = host
		}
		now := time.Now()
		mu.Lock()
		recent := hits[ip][:0]
		for _, t := range hits[ip] {
			if now.Sub(t) < window {
				recent = append(recent, t)
			}
		}
		over := len(recent) >= limit
		if over {
			hits[ip] = recent
		} else {
			hits[ip] = append(recent, now)
		}
		if len(hits) > 10000 { // memory limit — clean up old IPs
			for k, v := range hits {
				if len(v) == 0 || now.Sub(v[len(v)-1]) > window {
					delete(hits, k)
				}
			}
		}
		mu.Unlock()
		if over {
			s.writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests — please retry later")
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

// patchWorkspace updates workspace settings (visibility·policy — owner only, partial PATCH).
func (s *Server) patchWorkspace(w http.ResponseWriter, r *http.Request) {
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
	var patch app.WorkspacePatch
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
	out, err := s.id.UpdateWorkspaceSettings(r.Context(), u.ID, r.PathValue("wsID"), patch)
	if err == nil && body.GHVisibilitySync != nil && *body.GHVisibilitySync {
		// syncWorkspace runs once immediately upon enabling sync — success updates response, failure maintains setting.
		if synced, serr := s.b.SyncWorkspaceVisibility(r.Context(), r.PathValue("wsID")); serr == nil {
			out = synced
		}
	}
	s.respond(w, out, err)
}

// transferWorkspace transfers ownership to existing members (creator retains rights — URL changes).
func (s *Server) transferWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToUserID string `json:"to_user_id"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.id.TransferOwnership(r.Context(), u.ID, r.PathValue("wsID"), body.ToUserID)
	s.respond(w, out, err)
}

// syncVisibility manually runs GitHub public state sync (owner only).
func (s *Server) syncVisibility(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	if !s.id.IsOwner(r.Context(), r.PathValue("wsID"), u.ID) {
		s.writeError(w, http.StatusForbidden, "forbidden", "only owner can run sync")
		return
	}
	out, err := s.b.SyncWorkspaceVisibility(r.Context(), r.PathValue("wsID"))
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

// Public endpoints do not directly serialize internal domain objects. In the Workspace, webhook capability URLs and policy/synchronization status are included, and in the User, personal settings are also included. Therefore, new fields added via allowlist projection are not automatically exposed.
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

type publicWorkspaceView struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Slug          string            `json:"slug"`
	OwnerUsername string            `json:"owner_username"`
	Visibility    domain.Visibility `json:"visibility,omitempty"`
	PublicRole    string            `json:"public_role,omitempty"`
	Archived      bool              `json:"archived,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

func publicWorkspaceViewOf(ws domain.Workspace) publicWorkspaceView {
	return publicWorkspaceView{
		ID: ws.ID, Name: ws.Name, Slug: ws.Slug, OwnerUsername: ws.OwnerUsername,
		Visibility: ws.Visibility, PublicRole: ws.PublicRole, Archived: ws.Archived,
		CreatedAt: ws.CreatedAt,
	}
}

// publicWorkspace is the entry point for anonymous public read access — only public workspaces are interpreted (any other results in 404).
func (s *Server) publicWorkspace(w http.ResponseWriter, r *http.Request) {
	out, err := s.id.PublicWorkspace(r.Context(), r.PathValue("username"), r.PathValue("slug"))
	s.respond(w, publicWorkspaceViewOf(out), err)
}

// publicUser is the user profile entry point (/<username>) — user + publicly accessible workspaces.
// Accessible by anonymous users (only public workspaces visible), includes private if self.
func (s *Server) publicUser(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	u, wss, err := s.id.PublicUser(r.Context(), r.PathValue("username"), viewer.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	if wss == nil {
		wss = []domain.Workspace{}
	}
	publicWorkspaces := make([]publicWorkspaceView, 0, len(wss))
	for _, ws := range wss {
		publicWorkspaces = append(publicWorkspaces, publicWorkspaceViewOf(ws))
	}
	s.respond(w, map[string]any{"user": publicUserViewOf(u), "workspaces": publicWorkspaces}, nil)
}

// userContributions is user profile contribution heatmap data — daily commit counts per visible workspace (PublicUser same visibility: anonymous only public, self includes private).
func (s *Server) userContributions(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	_, wss, err := s.id.PublicUser(r.Context(), r.PathValue("username"), viewer.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	ids := make([]string, 0, len(wss))
	for _, ws := range wss {
		ids = append(ids, ws.ID)
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

// userActivity is user profile activity feed — monthly commit bundles + workspace creation (PublicUser same visibility).
func (s *Server) userActivity(w http.ResponseWriter, r *http.Request) {
	viewer, _ := userFrom(r.Context())
	_, wss, err := s.id.PublicUser(r.Context(), r.PathValue("username"), viewer.ID)
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	months, err := s.b.Activity(r.Context(), wss)
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
	err := s.id.UpdateMemberRole(r.Context(), u.ID, r.PathValue("wsID"), r.PathValue("userID"), domain.MemberRole(body.Role))
	s.respond(w, map[string]string{"status": "updated"}, err)
}

// deleteMember removes a member (owner can remove anyone, self can leave — constructor cannot).
func (s *Server) deleteMember(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RemoveMember(r.Context(), u.ID, r.PathValue("wsID"), r.PathValue("userID"))
	s.respond(w, map[string]string{"status": "removed"}, err)
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	ws, err := s.id.CreateWorkspace(r.Context(), u, body.Name)
	s.respond(w, ws, err)
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListWorkspaces(r.Context(), u.ID)
	// GitHub sync lazy TTL (1 hour): old workspaces refreshed in background (response unblocked, in-flight guard to prevent duplicate execution). Result reflected in next query.
	if err == nil {
		for _, wsp := range out {
			if wsp.GHVisibilitySync && (wsp.GHSyncedAt == nil || time.Since(*wsp.GHSyncedAt) > time.Hour) {
				s.kickVisibilitySync(wsp.ID)
			}
		}
	}
	s.respond(w, out, err)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListMembers(r.Context(), u.ID, r.PathValue("wsID"))
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
	inv, err := s.id.Invite(r.Context(), u.ID, r.PathValue("wsID"), body.Email, domain.MemberRole(body.Role), ttl)
	s.respond(w, inv, err)
}

// listInvites returns the invite list (maintainer or above — invite management screen).
func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.id.ListInvites(r.Context(), u.ID, r.PathValue("wsID"))
	s.respond(w, out, err)
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	ws, err := s.id.AcceptInvite(r.Context(), u, r.PathValue("token"))
	s.respond(w, ws, err)
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	err := s.id.RevokeInvite(r.Context(), u.ID, r.PathValue("wsID"), r.PathValue("token"))
	s.respond(w, map[string]string{"status": "revoked"}, err)
}
