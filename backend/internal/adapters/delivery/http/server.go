// Package http is the REST delivery adapter for the sync API.
//
// base = /api/v1/... , body application/json. frontend uses a separate CDN, so CORS is allowed.
// Handlers call a single Backend interface (consumer-defined) — app.Service implements this.
package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Backend is a set of server actions required by REST handlers (app.Service implements).
type Backend interface {
	Negotiate(ctx context.Context, in inbound.PushNegotiateInput) (inbound.PushNegotiateOutput, error)
	StoreChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error)
	PullChunks(ctx context.Context, in inbound.PullChunksInput) (inbound.PullChunksOutput, error)
	Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error)
	Send(ctx context.Context, in inbound.PullSendInput) (inbound.PullSendOutput, error)
	UpdateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error)
	List(ctx context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error)
	Diff(ctx context.Context, in inbound.DiffInput) (inbound.DiffOutput, error)
	Search(ctx context.Context, in inbound.SearchInput) (inbound.SearchOutput, error)
	Fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error)
	PromoteSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error
	GraftSnapshotParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error
	Join(ctx context.Context, in inbound.JoinInput) (inbound.JoinOutput, error)
	GetManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error)
	EnsureRepo(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error)
	ListRepos(ctx context.Context, team string) ([]domain.Repo, error)
	Contributions(ctx context.Context, workspaceIDs []string) (map[string]int, error)
	Activity(ctx context.Context, workspaces []domain.Workspace) ([]domain.ActivityMonth, error)
	GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error)
	Fsck(ctx context.Context, repoID domain.ContentHash) (inbound.FsckReport, error)
	Reflog(ctx context.Context, repoID domain.ContentHash) ([]domain.RefLogEntry, error)
	GetSnapshot(ctx context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error)
	GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error)
	ListRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error)
	// MemoryDigest: snapshot derivative carried with the raw document (compatibility rules).
	PutMemoryDigest(ctx context.Context, repoID domain.ContentHash, d domain.MemoryDigest) (domain.ContentHash, error)
	GetMemoryDigest(ctx context.Context, repoID, snapshotID domain.ContentHash) (domain.MemoryDigest, error)
	// About + team default settings bundle (web editing).
	UpdateAbout(ctx context.Context, repoID domain.ContentHash, description, website string, topics []string) error
	PutSettings(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettings(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error)
	PutSecrets(ctx context.Context, repoID domain.ContentHash, raw []byte) error
	GetSecrets(ctx context.Context, repoID domain.ContentHash) ([]byte, error)
	PutSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettingsObjectByHash(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error)
	// In-progress context pointer (CLI hook capture mirror — resolved by deletion on commit, for web display).
	PutPending(ctx context.Context, repoID domain.ContentHash, sessionID string, p domain.Pending) error
	ListPendings(ctx context.Context, repoID domain.ContentHash) ([]domain.Pending, error)
	DeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	DismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	UndismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	// GitHub PR merged webhook → append base context from head branch to base.
	PromoteMergedPR(ctx context.Context, gitURL, baseBranch, headBranch string) (int, error)
	// Push pending (unsync) pointer ((user, branch) key — resolved by deletion on git push, for On Hold display).
	PutUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string, u domain.Unsync) error
	ListUnsyncs(ctx context.Context, repoID domain.ContentHash) ([]domain.Unsync, error)
	DeleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error
	// GitHub public state sync (GHVisibilitySync enabled workspace — GitHub → cxthub unidirectional).
	SyncWorkspaceVisibility(ctx context.Context, workspaceID string) (domain.Workspace, error)
	// Repository structure setup (default branch, protected branch) — for maintainers and above.
	UpdateRepoConfig(ctx context.Context, repoID domain.ContentHash, defaultBranch *string, protectDefault *bool) error
}

// Server binds REST handlers to Backend (session synchronization) + IdentityBackend (authentication/workspace).
type Server struct {
	// syncInflight is a guard against duplicate execution of GitHub sync lazy TTL (workspace ID set).
	syncInflight sync.Map
	// device represents the current state of the CLI pairing (device flow) in progress (device_flow.go).
	device devicePairings
	b      Backend
	id     IdentityBackend
	cookie cookieCfg // session cookie attributes (env injected; tuned per deployment topology)
	cors   []string  // allowed Origin whitelist (empty reflects requested Origin — dev convenience)
}

// cookieCfg are security attributes for session cookies (HttpOnly). Tokens are passed as cookies that JS cannot read.
type cookieCfg struct {
	secure   bool          // HTTPS only. Forced to true if SameSite=None.
	sameSite http.SameSite // default Lax (CSRF defense in first-party sites)
	domain   string        // e.g., ".example.com" — for sharing across subdomains
}

// NewServer creates a Server by injecting Backend and IdentityBackend.
// Cookie/CORS settings are read from environment variables (CXT_COOKIE_*, CXT_CORS_ORIGINS).
func NewServer(b Backend, id IdentityBackend) *Server {
	return &Server{b: b, id: id, cookie: loadCookieCfg(), cors: splitCSV(os.Getenv("CXT_CORS_ORIGINS"))}
}

// loadCookieCfg reads cookie attributes from CXT_COOKIE_SECURE / _SAMESITE / _DOMAIN.
func loadCookieCfg() cookieCfg {
	c := cookieCfg{sameSite: http.SameSiteLaxMode, secure: envBool("CXT_COOKIE_SECURE")}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CXT_COOKIE_SAMESITE"))) {
	case "strict":
		c.sameSite = http.SameSiteStrictMode
	case "none":
		c.sameSite = http.SameSiteNoneMode
		c.secure = true // Browser rule: SameSite=None cookies require Secure flag.
	case "lax", "":
		c.sameSite = http.SameSiteLaxMode
	}
	c.domain = strings.TrimSpace(os.Getenv("CXT_COOKIE_DOMAIN"))
	return c
}

func envBool(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Handler returns an http.Handler with all routes registered (go 1.22+ method-pattern routing).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Deployment/LoadBalancer health check. A process-level probe that does not expose authentication/storage content.
	mux.HandleFunc("GET /api/v1/health", s.health)

	// 5-tier role gate (guard): viewer=Read / puller=Team asset pull / member=Context write
	// / maintainer=Team asset write (+policy by action) / owner=Operations.
	// GET at viewer level is allowed for public workspaces. Detailed rules are in requireRepoRole.
	mux.HandleFunc("GET /api/v1/repos", s.listRepos)
	mux.HandleFunc("POST /api/v1/repos", s.requireUser(s.createRepo)) // distillation (empirically verified usage of bound ws is checked by subsequent gate)
	mux.HandleFunc("GET /api/v1/repos/{repoID}", s.guard(domain.RoleViewer, s.getRepo))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/fsck", s.guard(domain.RoleViewer, s.fsck))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/reflog", s.guard(domain.RoleViewer, s.reflog))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/manifest", s.guard(domain.RoleViewer, s.getManifest))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/branches", s.guard(domain.RoleViewer, s.listRefs))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/refs", s.guard(domain.RoleViewer, s.listRefs))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/snapshots", s.guard(domain.RoleViewer, s.listSnapshots))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/snapshots/{id}", s.guard(domain.RoleViewer, s.getSnapshot))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/snapshots/{id}/promote", s.guard(domain.RoleMember, s.promoteSnapshot))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/snapshots/{id}/graft", s.guard(domain.RoleMember, s.graftSnapshot))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/join", s.guard(domain.RoleMember, s.joinSnapshot))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/search", s.guard(domain.RoleViewer, s.search))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/docs/{hash}", s.guard(domain.RoleViewer, s.getDoc))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/memories/{snapshotID}", s.guard(domain.RoleViewer, s.getMemory))
	// About/team settings/secrets: writes require maintainer or higher plus the action-specific requireRepoAction policy; reads require puller or higher for local team-asset synchronization.
	mux.HandleFunc("PATCH /api/v1/repos/{repoID}/about", s.guard(domain.RoleMaintainer, s.patchAbout))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/settings/{kind}", s.guard(domain.RolePuller, s.getSettings))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/settings/{kind}", s.requireUser(s.putSettings))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/secrets", s.requireUser(s.putSecrets))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/secrets", s.guard(domain.RolePuller, s.getSecrets))
	// Commit attachment settings object: Write = context push part (member), Read = pull layer (puller).
	mux.HandleFunc("GET /api/v1/repos/{repoID}/settings-objects/{hash}", s.guard(domain.RolePuller, s.getSettingsObject))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/settings-objects/{hash}", s.guard(domain.RoleMember, s.putSettingsObject))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/memories/{snapshotID}", s.guard(domain.RoleMember, s.putMemory))
	// In-progress context pointer: Write/Delete = context push layer (member), Read = pull/web layer (puller).
	mux.HandleFunc("GET /api/v1/repos/{repoID}/pending", s.guard(domain.RolePuller, s.listPending))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/pending/{sessionID}", s.guard(domain.RoleMember, s.putPending))
	mux.HandleFunc("DELETE /api/v1/repos/{repoID}/pending/{sessionID}", s.guard(domain.RoleMember, s.deletePending))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/pending/{sessionID}/dismiss", s.guard(domain.RoleMember, s.dismissPending))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/pending/{sessionID}/undismiss", s.guard(domain.RoleMember, s.undismissPending))
	// Push pending (unsync) pointer: Key = (authenticated user, branch) — write/delete only by owner.
	mux.HandleFunc("GET /api/v1/repos/{repoID}/unsync", s.guard(domain.RolePuller, s.listUnsync))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/unsync/{branch...}", s.guard(domain.RoleMember, s.putUnsync))
	mux.HandleFunc("DELETE /api/v1/repos/{repoID}/unsync/{branch...}", s.guard(domain.RoleMember, s.deleteUnsync))
	// {name...}: Branch names can include slashes like in git (feature/login, etc.) — full matching of the rest.
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/refs/{kind}/{name...}", s.guard(domain.RoleMember, s.putRef))

	// GitHub webhook reception (PR merged → context promotion). HMAC signature verification instead of authentication — inactive (404) if secret (CXT_GITHUB_WEBHOOK_SECRET) is not set.
	mux.HandleFunc("POST /api/v1/hooks/github", s.githubWebhook)

	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/negotiate", s.guard(domain.RoleMember, s.pushNegotiate))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/chunks", s.guard(domain.RoleMember, s.pushChunks))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/objects", s.guard(domain.RoleMember, s.pushObjects))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/pull/chunks", s.guard(domain.RolePuller, s.pullChunks))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/pull/objects", s.guard(domain.RolePuller, s.pullObjects))

	// Actions (for web UI). Diff is read operation (viewer), Fork is write operation (member). Load is local CLI exclusive (501 error).
	mux.HandleFunc("POST /api/v1/repos/{repoID}/diff", s.guard(domain.RoleViewer, s.diff))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/fork", s.guard(domain.RoleMember, s.fork))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/load", s.notImplemented)

	// Authentication · Workspace · Invite (all Firebase/dev tokens required — requireUser middleware).
	s.registerIdentity(mux)

	return s.withSecurityHeaders(s.withCORS(s.withCSRF(mux)))
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// --- Handlers ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.respond(w, map[string]string{"status": "ok"}, nil)
}

// listRepos: if ?workspace=<id> is present, requires login + corresponding workspace membership and
// returns only repos within that workspace (visibility boundary). Without parameters, returns accessible repos only.
func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("workspace")
	team := r.URL.Query().Get("team")
	if team == "" {
		team = "default"
	}
	repos, err := s.b.ListRepos(r.Context(), team)
	if err != nil {
		s.respond(w, repos, err)
		return
	}
	if wsID == "" {
		// Workspace filter-less full list exposes enumeration info (review found #4) —
		// Anonymous gets empty list (server survival check 200), authenticated user gets only accessible ones:
		// My member workspaces · Public workspaces. Membership or policy lookup failures are hidden.
		token := s.requestToken(r)
		u, uerr := s.id.ResolveUser(r.Context(), token)
		if token == "" || uerr != nil {
			s.respond(w, []domain.Repo{}, nil)
			return
		}
		visible := make([]domain.Repo, 0, len(repos))
		for _, rp := range repos {
			if rp.WorkspaceID == "" {
				continue
			}
			wsp, werr := s.id.GetWorkspace(r.Context(), rp.WorkspaceID)
			if werr != nil {
				continue
			}
			if wsp.IsPublic() {
				visible = append(visible, rp)
				continue
			}
			if _, ok := s.id.RoleOf(r.Context(), rp.WorkspaceID, u.ID); ok {
				visible = append(visible, rp)
			}
		}
		s.respond(w, visible, nil)
		return
	}
	// Workspace filter — members only. However, public (public) workspaces are accessible by anyone.
	wsp, werr := s.id.GetWorkspace(r.Context(), wsID)
	if werr != nil {
		code, status := mapError(werr)
		s.writeError(w, status, code, werr.Error())
		return
	}
	if !wsp.IsPublic() {
		token := s.requestToken(r)
		u, uerr := s.id.ResolveUser(r.Context(), token)
		if token == "" || uerr != nil {
			s.writeError(w, http.StatusUnauthorized, "unauthenticated", "workspace filter requires login")
			return
		}
		if _, merr := s.id.ListMembers(r.Context(), u.ID, wsID); merr != nil {
			code, status := mapError(merr)
			s.writeError(w, status, code, "not a workspace member")
			return
		}
	}
	filtered := make([]domain.Repo, 0, len(repos))
	for _, rp := range repos {
		if rp.WorkspaceID == wsID {
			filtered = append(filtered, rp)
		}
	}
	s.respond(w, filtered, nil)
}

func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	var repo domain.Repo
	if !s.decode(w, r, &repo) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.b.EnsureRepo(r.Context(), u.ID, repo)
	s.respond(w, out, err)
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetRepo(r.Context(), s.repoID(r))
	s.respond(w, out, err)
}

// fsck returns reference reachability audit results (read-only — makes no changes).
func (s *Server) fsck(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.Fsck(r.Context(), s.repoID(r))
	s.respond(w, out, err)
}

// reflog returns the repository's ref movement records (newest first) (read-only).
func (s *Server) reflog(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.Reflog(r.Context(), s.repoID(r))
	if out == nil {
		out = []domain.RefLogEntry{}
	}
	s.respond(w, out, err)
}

func (s *Server) getManifest(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetManifest(r.Context(), s.repoID(r))
	s.respond(w, out, err)
}

func (s *Server) listRefs(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.ListRefs(r.Context(), s.repoID(r))
	s.respond(w, out, err)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.List(r.Context(), inbound.ListSnapshotsInput{RepoID: s.repoID(r), Branch: r.URL.Query().Get("branch")})
	s.respond(w, out, err)
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetSnapshot(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("id")))
	s.respond(w, out, err)
}

// requireRepoMember checks if the caller is a member of the repo if it belongs to the workspace.
// guard gates the route as a 5-step role ladder (serial AND front — roles are layer boundaries,
// policy specifics are narrowed by requireRepoAction behind).
//
//   - If a token is present, it is interpreted and injected into the context (anonymous is initially allowed — determination is by requireRepoRole).
//   - Reading at the viewer level in a public workspace is allowed for anonymous users (GitHub public repo compatibility).
//   - Repos not belonging to the workspace have no policy boundary and are all rejected.
func (s *Server) guard(min domain.MemberRole, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := s.requestToken(r); token != "" {
			if u, err := s.id.ResolveUser(r.Context(), token); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
			}
		}
		if !s.requireRepoRole(w, r, min) {
			return
		}
		fn(w, r)
	}
}

// kickVisibilitySync runs GitHub public sync in the background once (preventing duplicates).
func (s *Server) kickVisibilitySync(wsID string) {
	if _, running := s.syncInflight.LoadOrStore(wsID, true); running {
		return
	}
	go func() {
		defer s.syncInflight.Delete(wsID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.b.SyncWorkspaceVisibility(ctx, wsID)
	}()
}

// requireRepoRole checks if the user's role in the repo's workspace is at least min.
//
//   - Reading at the viewer level in a public workspace is allowed for anonymous users (GitHub public repo compatibility).
//   - Repos not belonging to the workspace have no policy boundary and are all rejected.
func (s *Server) requireRepoRole(w http.ResponseWriter, r *http.Request, min domain.MemberRole) bool {
	repo, err := s.b.GetRepo(r.Context(), s.repoID(r))
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return false
	}
	u, authed := userFrom(r.Context())
	if repo.WorkspaceID == "" {
		s.writeError(w, http.StatusForbidden, "repo_unbound",
			"repository is not bound to a workspace — verify that <username>/<workspace> in the remote URL matches an existing workspace, then reconnect with cxt setup <url>")
		return false
	}
	wsp, werr := s.id.GetWorkspace(r.Context(), repo.WorkspaceID)
	if werr != nil {
		code, status := mapError(werr)
		s.writeError(w, status, code, werr.Error())
		return false
	}
	// Public workspace: non-members (including anonymous) receive the default role set by the workspace (viewer by default, owner can go up to puller). Members are determined by their actual role below.
	if wsp.IsPublic() {
		if wsp.PublicBaseRole().AtLeast(min) && (min == domain.RoleViewer || !wsp.Archived) {
			return true // Satisfies the default role for non-members (viewer exceeds storage is blocked)
		}
	}
	if !authed {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "Login required — cxt login <token> (generate from Account Settings ⚙)")
		return false
	}
	// The archived workspace is read-only — deny actions beyond viewer (check after authentication —
	// does not leak workspace status to anonymous users).
	if min != domain.RoleViewer && wsp.Archived {
		s.writeError(w, http.StatusForbidden, "forbidden", "archived workspace (read-only) — owner can disable in settings")
		return false
	}
	role, ok := s.id.RoleOf(r.Context(), repo.WorkspaceID, u.ID)
	if !ok || !role.AtLeast(min) {
		s.writeError(w, http.StatusForbidden, "forbidden", "insufficient role permissions — at least "+string(min)+" required")
		return false
	}
	return true
}

// requireRepoAction narrows down policy by action after role gate (maintainer and above).
// action ∈ {secrets, settings}. Policy "list" allows only specified user (owner always allowed).
func (s *Server) requireRepoAction(w http.ResponseWriter, r *http.Request, action string) bool {
	if !s.requireRepoRole(w, r, domain.RoleMaintainer) {
		return false
	}
	repo, err := s.b.GetRepo(r.Context(), s.repoID(r))
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return false
	}
	if repo.WorkspaceID == "" {
		s.writeError(w, http.StatusForbidden, "repo_unbound", "repo not bound to workspace, policy determination impossible")
		return false
	}
	wsp, werr := s.id.GetWorkspace(r.Context(), repo.WorkspaceID)
	if werr != nil {
		code, status := mapError(werr)
		s.writeError(w, status, code, werr.Error())
		return false
	}
	policy := wsp.SecretsPolicy
	if action == "settings" {
		policy = wsp.SettingsPolicy
	}
	if policy == "" || policy == "members" {
		return true // role-based only (no additional narrowing)
	}
	u, _ := userFrom(r.Context())
	if !domain.PolicyAllows(policy, s.id.IsOwner(r.Context(), wsp.ID, u.ID)) {
		s.writeError(w, http.StatusForbidden, "forbidden", "action not allowed by workspace permissions ("+action+")")
		return false
	}
	return true
}

// patchAbout updates repo About (description/website/topics) + structure settings (default branch, protected branches)
// (route with maintainer gate).
func (s *Server) patchAbout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Description    *string  `json:"description"`
		Website        *string  `json:"website"`
		Topics         []string `json:"topics"`
		DefaultBranch  *string  `json:"default_branch"`
		ProtectDefault *bool    `json:"protect_default"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.DefaultBranch != nil || body.ProtectDefault != nil {
		if err := s.b.UpdateRepoConfig(r.Context(), s.repoID(r), body.DefaultBranch, body.ProtectDefault); err != nil {
			code, status := mapError(err)
			s.writeError(w, status, code, err.Error())
			return
		}
	}
	// about field is true PATCH semantics — missing fields maintain existing values (workspace settings save does not overwrite About body with empty value).
	if body.Description != nil || body.Website != nil || body.Topics != nil {
		cur, gerr := s.b.GetRepo(r.Context(), s.repoID(r))
		if gerr != nil {
			code, status := mapError(gerr)
			s.writeError(w, status, code, gerr.Error())
			return
		}
		desc, web, topics := cur.Description, cur.Website, cur.Topics
		if body.Description != nil {
			desc = *body.Description
		}
		if body.Website != nil {
			web = *body.Website
		}
		if body.Topics != nil {
			topics = body.Topics
		}
		if err := s.b.UpdateAbout(r.Context(), s.repoID(r), desc, web, topics); err != nil {
			code, status := mapError(err)
			s.writeError(w, status, code, err.Error())
			return
		}
	}
	repo, err := s.b.GetRepo(r.Context(), s.repoID(r))
	s.respond(w, repo, err)
}

// getSettings returns team default setting bundles.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !domain.ValidSettingsKind(kind) {
		s.writeError(w, http.StatusNotFound, "not_found", "settings kind not found")
		return
	}
	out, err := s.b.GetSettings(r.Context(), s.repoID(r), kind)
	if errors.Is(err, domain.ErrNotFound) {
		// These optional singleton assets are queried to render their configured/unset
		// status. Absence is a successful state, not a failed HTTP request.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.respond(w, out, err)
}

// putSettings uploads team default setting bundles (claude|agents|codex folder).
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireRepoAction(w, r, "settings") {
		return
	}
	var bundle domain.SettingsBundle
	if !s.decode(w, r, &bundle) {
		return
	}
	bundle.Kind = r.PathValue("kind")
	if u, ok := userFrom(r.Context()); ok {
		bundle.UpdatedBy = u.Username
	}
	if err := s.b.PutSettings(r.Context(), s.repoID(r), bundle); err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	s.respond(w, map[string]any{"kind": bundle.Kind, "files": len(bundle.Files)}, nil)
}

// putSecrets stores secret ciphertext envelopes (server handles transparently — E2E).
func (s *Server) putSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.requireRepoAction(w, r, "secrets") {
		return
	}
	if !isJSONBody(r) { // apply CSRF 2nd defense like decode() for raw body paths
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 512<<10))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// team passphrase consistency: reject if envelope fingerprint differs from server stored version — prevent accidental unlocking with old keys. rotate=true is explicit replacement (upload completed re-encryption).
	newFp := fingerprintOf(raw)
	// fail-closed: inconsistent state on failure other than not-found — passing through allows incorrect passphrase push to overwrite existing envelope in transient outage window. reject without saving.
	existing, gerr := s.b.GetSecrets(r.Context(), s.repoID(r))
	if gerr != nil && !errors.Is(gerr, domain.ErrNotFound) {
		s.writeError(w, http.StatusServiceUnavailable, "consistency_check_failed", "Failed to retrieve existing secret — consistency check failed, storage rejected. Retry later.")
		return
	}
	oldFp := ""
	if gerr == nil {
		oldFp = fingerprintOf(existing)
	}
	if r.URL.Query().Get("rotate") == "true" {
		// rotate is the point of transition = new system adoption, so a fingerprint is required.
		if newFp == "" {
			s.writeError(w, http.StatusBadRequest, "fingerprint_required", "Envelope lacks fingerprint — update cxt/web to the latest version.")
			return
		}
		// CAS: rotation intentionally changes the fingerprint, so expect must identify the envelope the client
		// used as its re-encryption baseline. Rejecting an intervening write prevents a GET-to-PUT race from
		// replacing a teammate's fresh secrets with stale re-encrypted data.
		if oldFp != "" {
			if expect := r.URL.Query().Get("expect"); expect != oldFp {
				s.writeError(w, http.StatusConflict, "rotate_conflict",
					"secrets changed after rotation began (current id "+oldFp+") — fetch the latest envelope and retry")
				return
			}
		}
	} else {
		if newFp == "" {
			// Legacy cxt compatibility (grandfather): Envelopes without fingerprints are accepted only when the team is not using the fingerprint system (legacy contract = transparent storage). If the existing envelope has a fingerprint, consistency check fails, and it is rejected.
			if oldFp != "" {
				s.writeError(w, http.StatusBadRequest, "fingerprint_required", "Team is using passphrase fingerprint system — update cxt and push again.")
				return
			}
		} else if oldFp != "" && oldFp != newFp {
			s.writeError(w, http.StatusConflict, "passphrase_mismatch",
				"secrets already use a different team passphrase (id "+oldFp+") — use the same passphrase or set rotate=true to replace it")
			return
		}
	}
	if err := s.b.PutSecrets(r.Context(), s.repoID(r), raw); err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	s.respond(w, map[string]string{"status": "stored"}, nil)
}

// fingerprintOf extracts the fingerprint field from the envelope raw (server treats the rest as opaque — E2E).
func fingerprintOf(raw []byte) string {
	var e struct {
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Fingerprint
}

// getSecrets returns the encrypted envelope as is (decryption is on the client — E2E).
func (s *Server) getSecrets(w http.ResponseWriter, r *http.Request) {
	raw, err := s.b.GetSecrets(r.Context(), s.repoID(r))
	if errors.Is(err, domain.ErrNotFound) {
		// The web status rail probes this endpoint before a team envelope exists.
		// Keep genuine repository/auth failures in middleware, but represent an
		// unconfigured optional envelope without a noisy 404.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

// getSettingsObject returns the commit attachment settings object.
func (s *Server) getSettingsObject(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetSettingsObjectByHash(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")))
	s.respond(w, out, err)
}

// putSettingsObject stores the commit attachment settings object (idempotent).
func (s *Server) putSettingsObject(w http.ResponseWriter, r *http.Request) {
	var bundle domain.SettingsBundle
	if !s.decode(w, r, &bundle) {
		return
	}
	err := s.b.PutSettingsObject(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")), bundle)
	s.respond(w, map[string]string{"status": "stored"}, err)
}

// listPending returns the entire list of pending context pointers in the repo (for web display).
func (s *Server) listPending(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.ListPendings(r.Context(), s.repoID(r))
	if out == nil {
		out = []domain.Pending{}
	}
	s.respond(w, out, err)
}

// pendingGuard checks if the caller has the right to overwrite or delete the existing pending for sessionID.
// unsync enforces ownership by (user,branch) key but pending uses sessionID key, so there was no ownership boundary — any member or someone else could overwrite or delete a pending pointer (asymmetric, review backlog #6).
// Rule: owner (Author.Email==caller) or maintainer. If no existing pending, new → allowed.
func (s *Server) pendingGuard(w http.ResponseWriter, r *http.Request, sessionID string) (caller domain.User, handled bool) {
	caller, _ = userFrom(r.Context())
	pendings, err := s.b.ListPendings(r.Context(), s.repoID(r))
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return caller, true
	}
	for _, p := range pendings {
		if p.SessionID != sessionID {
			continue
		}
		if p.Author.Email != "" && caller.Email != "" && p.Author.Email == caller.Email {
			return caller, false // owner
		}
		if repo, rerr := s.b.GetRepo(r.Context(), s.repoID(r)); rerr == nil && repo.WorkspaceID != "" {
			if role, ok := s.id.RoleOf(r.Context(), repo.WorkspaceID, caller.ID); ok && role.AtLeast(domain.RoleMaintainer) {
				return caller, false // maintainer or above
			}
		}
		s.writeError(w, http.StatusForbidden, "forbidden", "owned by another session user — only owner or maintainer can change")
		return caller, true
	}
	return caller, false // new
}

// putPending updates the session's pending pointer (CLI hook detached helper path).
func (s *Server) putPending(w http.ResponseWriter, r *http.Request) {
	var p domain.Pending
	if !s.decode(w, r, &p) {
		return
	}
	sessionID := r.PathValue("sessionID")
	caller, handled := s.pendingGuard(w, r, sessionID)
	if handled {
		return
	}
	// Author sets server authority — trusts client body and authenticates user (prevents forgery and misattribution).
	name := caller.Name
	if name == "" {
		name = caller.Username
	}
	p.Author = domain.TeamIdentity{Name: name, Email: caller.Email}
	err := s.b.PutPending(r.Context(), s.repoID(r), sessionID, p)
	s.respond(w, map[string]string{"status": "stored"}, err)
}

// deletePending releases the session's pending pointer after commit incorporation or manual cleanup. It is idempotent.
func (s *Server) deletePending(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if _, handled := s.pendingGuard(w, r, sessionID); handled {
		return
	}
	err := s.b.DeletePending(r.Context(), s.repoID(r), sessionID)
	s.respond(w, map[string]string{"status": "deleted"}, err)
}

// dismissPending hides the in-progress session from the uncommitted list (no data deletion — dismissed flag).
func (s *Server) dismissPending(w http.ResponseWriter, r *http.Request) {
	// POST without body can be cross-site submitted via HTML form (unlike PUT/DELETE) —
	// applies state change common CSRF 2nd defense (application/json enforced) without decode middleware.
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	sessionID := r.PathValue("sessionID")
	if _, handled := s.pendingGuard(w, r, sessionID); handled {
		return
	}
	err := s.b.DismissPending(r.Context(), s.repoID(r), sessionID)
	s.respond(w, map[string]string{"status": "dismissed"}, err)
}

// listUnsync returns the entire push queue pointer for the repo (for web On Hold display).
func (s *Server) listUnsync(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.ListUnsyncs(r.Context(), s.repoID(r))
	if out == nil {
		out = []domain.Unsync{}
	}
	s.respond(w, out, err)
}

// putUnsync upserts the push queue pointer for a branch by an authenticated user (for shadow sync).
func (s *Server) putUnsync(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "Login required")
		return
	}
	var body domain.Unsync
	if !s.decode(w, r, &body) {
		return
	}
	err := s.b.PutUnsync(r.Context(), s.repoID(r), u.Username, r.PathValue("branch"), body)
	s.respond(w, map[string]string{"status": "stored"}, err)
}

// deleteUnsync releases the push queue pointer for a branch by an authenticated user (git push/manual — idempotent).
func (s *Server) deleteUnsync(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "Login required")
		return
	}
	err := s.b.DeleteUnsync(r.Context(), s.repoID(r), u.Username, r.PathValue("branch"))
	s.respond(w, map[string]string{"status": "deleted"}, err)
}

// getMemory returns the compressed memory (MemoryDigest) of a snapshot.
func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetMemoryDigest(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("snapshotID")))
	s.respond(w, out, err)
}

// putMemory attaches compressed memory to a snapshot (idempotent). Target snapshot must exist (404 if not).
func (s *Server) putMemory(w http.ResponseWriter, r *http.Request) {
	var d domain.MemoryDigest
	if !s.decode(w, r, &d) {
		return
	}
	d.SnapshotID = domain.ContentHash(r.PathValue("snapshotID"))
	hash, err := s.b.PutMemoryDigest(r.Context(), s.repoID(r), d)
	s.respond(w, map[string]any{"memory_hash": hash}, err)
}

func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetDoc(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")))
	s.respond(w, out, err)
}

type putRefBody struct {
	Target         domain.ContentHash `json:"target"`
	ExpectedTarget domain.ContentHash `json:"expected_target"`
	Symbolic       string             `json:"symbolic"`
	// Force allows non-fast-forward moves (git push --force).
	Force bool `json:"force,omitempty"`
	// Append is like diverged push, grafting the diverged push onto the current head (cxt push --append).
	Append bool `json:"append,omitempty"`
}

// undismissPending re-adds dismissed uncommitted sessions to the list (undo dismiss).
func (s *Server) undismissPending(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) { // dismiss and same body-less POST CSRF 2nd defense
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	sessionID := r.PathValue("sessionID")
	if _, handled := s.pendingGuard(w, r, sessionID); handled {
		return
	}
	err := s.b.UndismissPending(r.Context(), s.repoID(r), sessionID)
	s.respond(w, map[string]string{"status": "undismissed"}, err)
}

// githubWebhook converts a merged GitHub PR event into context promotion (audit finding #14).
// Signature (X-Hub-Signature-256, HMAC-SHA256) verification — secret is server environment variable
// CXT_GITHUB_WEBHOOK_SECRET (deployment setting). If unset, 404 (feature disabled).
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("CXT_GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		s.writeError(w, http.StatusNotFound, "not_found", "webhook receiver disabled (CXT_GITHUB_WEBHOOK_SECRET unset)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Failed to read body")
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "Webhook signature mismatch")
		return
	}
	if ev := r.Header.Get("X-GitHub-Event"); ev != "pull_request" {
		s.respond(w, map[string]string{"status": "ignored", "event": ev}, nil)
		return
	}
	var payload struct {
		Action      string `json:"action"`
		PullRequest struct {
			Merged bool `json:"merged"`
			Base   struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				Ref  string `json:"ref"`
				Repo struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			HTMLURL  string `json:"html_url"`
			CloneURL string `json:"clone_url"`
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if jerr := json.Unmarshal(body, &payload); jerr != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "payload parsing failed")
		return
	}
	if payload.Action != "closed" || !payload.PullRequest.Merged {
		s.respond(w, map[string]string{"status": "ignored"}, nil)
		return
	}
	// Context branch refs belong to the base cxt repository. A fork PR's
	// same-named branch could otherwise resolve to unrelated base-repo context.
	if payload.Repository.FullName == "" || payload.PullRequest.Head.Repo.FullName == "" ||
		!strings.EqualFold(payload.Repository.FullName, payload.PullRequest.Head.Repo.FullName) {
		s.respond(w, map[string]string{"status": "ignored", "reason": "fork_head"}, nil)
		return
	}
	gitURL := payload.Repository.CloneURL
	if gitURL == "" {
		gitURL = payload.Repository.HTMLURL
	}
	n, perr := s.b.PromoteMergedPR(r.Context(), gitURL, payload.PullRequest.Base.Ref, payload.PullRequest.Head.Ref)
	s.respond(w, map[string]interface{}{"status": "ok", "promoted": n}, perr)
}

func (s *Server) putRef(w http.ResponseWriter, r *http.Request) {
	var body putRefBody
	if !s.decode(w, r, &body) {
		return
	}
	rid := s.repoID(r)
	ref := domain.Ref{
		Kind:     domain.RefKind(r.PathValue("kind")),
		Name:     r.PathValue("name"),
		RepoID:   rid,
		Target:   body.Target,
		Symbolic: body.Symbolic,
	}
	out, err := s.b.UpdateRef(r.Context(), inbound.UpdateRefInput{RepoID: rid, Ref: ref, ExpectedTarget: body.ExpectedTarget, Force: body.Force, Append: body.Append})
	s.respond(w, out, err)
}

type negotiateBody struct {
	SnapshotHaves []domain.ContentHash `json:"snapshot_haves"`
	DocHaves      []domain.ContentHash `json:"doc_haves"`
	ChunkHaves    []domain.ContentHash `json:"chunk_haves"`
}

func (s *Server) pushNegotiate(w http.ResponseWriter, r *http.Request) {
	var body negotiateBody
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.b.Negotiate(r.Context(), inbound.PushNegotiateInput{RepoID: s.repoID(r), SnapshotHaves: body.SnapshotHaves, DocHaves: body.DocHaves, ChunkHaves: body.ChunkHaves})
	s.respond(w, out, err)
}

type chunksBody struct {
	Chunks []inbound.ChunkObject `json:"chunks"`
}

func (s *Server) pushChunks(w http.ResponseWriter, r *http.Request) {
	var body chunksBody
	if !s.decodeLimited(w, r, &body, inbound.MaxChunkWireJSONBody) {
		return
	}
	out, err := s.b.StoreChunks(r.Context(), inbound.StoreChunksInput{RepoID: s.repoID(r), Chunks: body.Chunks})
	s.respond(w, out, err)
}

type objectsBody struct {
	Snapshots    []domain.Snapshot     `json:"snapshots"`
	Docs         []domain.SessionDoc   `json:"docs"`
	ChunkedDocs  []inbound.ChunkedDoc  `json:"chunked_docs"`
	ChunkObjects []inbound.ChunkObject `json:"chunk_objects"`
}

func (s *Server) pushObjects(w http.ResponseWriter, r *http.Request) {
	var body objectsBody
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.b.Commit(r.Context(), inbound.CommitInput{RepoID: s.repoID(r), Snapshots: body.Snapshots, Docs: body.Docs, ChunkedDocs: body.ChunkedDocs, ChunkObjects: body.ChunkObjects})
	s.respond(w, out, err)
}

// promoteSnapshot promotes a hook label snapshot message to a commit message (one-way, idempotent).
func (s *Server) promoteSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	err := s.b.PromoteSnapshotMessage(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("id")), body.Message)
	s.respond(w, map[string]string{"status": "ok"}, err)
}

// graftSnapshot adds an overlay edge by validating the expected_seq CAS and cycle.
func (s *Server) graftSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Parents     []domain.ContentHash `json:"parents"`
		ExpectedSeq *uint64              `json:"expected_seq"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.ExpectedSeq == nil {
		s.respond(w, nil, fmt.Errorf("%w: expected_seq is required", domain.ErrValidation))
		return
	}
	err := s.b.GraftSnapshotParents(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("id")), body.Parents, *body.ExpectedSeq)
	s.respond(w, map[string]string{"status": "ok"}, err)
}

// joinSnapshot repositions session forks of the same git branch behind the branch head.
func (s *Server) joinSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Branch             string             `json:"branch"`
		Snapshot           domain.ContentHash `json:"snapshot"`
		IncludeDescendants bool               `json:"include_descendants"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.b.Join(r.Context(), inbound.JoinInput{
		RepoID: s.repoID(r), TargetBranch: body.Branch, Snapshot: body.Snapshot,
		IncludeDescendants: body.IncludeDescendants,
	})
	s.respond(w, out, err)
}

type pullBody struct {
	SnapshotWants    []domain.ContentHash `json:"snapshot_wants"`
	DocWants         []domain.ContentHash `json:"doc_wants"`
	DocManifestWants []domain.ContentHash `json:"doc_manifest_wants"`
	ChunkWants       []domain.ContentHash `json:"chunk_wants"`
}

type pullChunksBody struct {
	ChunkWants []domain.ContentHash `json:"chunk_wants"`
}

func (s *Server) pullChunks(w http.ResponseWriter, r *http.Request) {
	var body pullChunksBody
	if !s.decodeLimited(w, r, &body, inbound.MaxChunkWantJSONBody) {
		return
	}
	out, err := s.b.PullChunks(r.Context(), inbound.PullChunksInput{RepoID: s.repoID(r), Wants: body.ChunkWants})
	s.respond(w, out, err)
}

func (s *Server) pullObjects(w http.ResponseWriter, r *http.Request) {
	var body pullBody
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.b.Send(r.Context(), inbound.PullSendInput{RepoID: s.repoID(r), SnapshotWants: body.SnapshotWants, DocWants: body.DocWants, DocManifestWants: body.DocManifestWants, ChunkWants: body.ChunkWants})
	s.respond(w, out, err)
}

// search searches commit messages/authors + conversation body (read — viewer level and above).
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.Search(r.Context(), inbound.SearchInput{RepoID: s.repoID(r), Query: r.URL.Query().Get("q")})
	s.respond(w, out, err)
}

type diffBody struct {
	Left  domain.ContentHash `json:"left"`
	Right domain.ContentHash `json:"right"`
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	var body diffBody
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.b.Diff(r.Context(), inbound.DiffInput{RepoID: s.repoID(r), Left: body.Left, Right: body.Right})
	s.respond(w, out, err)
}

type forkBody struct {
	From      domain.ContentHash  `json:"from"`
	NewBranch string              `json:"new_branch"`
	Author    domain.TeamIdentity `json:"author"`
}

func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	var body forkBody
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.b.Fork(r.Context(), inbound.ForkInput{RepoID: s.repoID(r), FromSnapshot: body.From, NewBranch: body.NewBranch, Author: body.Author})
	s.respond(w, out, err)
}

// --- Common Helpers ---

func (s *Server) repoID(r *http.Request) domain.ContentHash {
	return domain.ContentHash(r.PathValue("repoID"))
}

// isJSONBody checks if the request body Content-Type is application/json (allows empty values, rejects otherwise).
// CSRF 2nd defense + format enforcement — cross-site fetch cannot append application/json without a preflight,
// regardless of SameSite setting (Lax/None), limiting state change bodies to first-party JSON.
func isJSONBody(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

func (s *Server) decodeLimited(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "bounded chunk body exceeds transport limit")
			return false
		}
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

func (s *Server) respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	// nil slice is encoded as null in JSON — list responses are always [] to prevent clients (e.g., JS `.length`) from breaking on null.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		v = reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func mapError(err error) (code string, status int) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "not_found", http.StatusNotFound
	case errors.Is(err, domain.ErrIntegrity):
		return "integrity_violation", http.StatusUnprocessableEntity
	case errors.Is(err, domain.ErrNonFastForward):
		return "non_fast_forward", http.StatusConflict
	case errors.Is(err, domain.ErrRefConflict):
		return "ref_conflict", http.StatusConflict
	case errors.Is(err, domain.ErrUnauthorized):
		return "unauthenticated", http.StatusUnauthorized
	case errors.Is(err, domain.ErrForbidden):
		return "forbidden", http.StatusForbidden
	case errors.Is(err, domain.ErrGitOriginMismatch):
		return "git_origin_mismatch", http.StatusConflict
	case errors.Is(err, domain.ErrConflict):
		return "conflict", http.StatusConflict
	case errors.Is(err, domain.ErrValidation):
		return "validation", http.StatusUnprocessableEntity
	default:
		return "internal", http.StatusInternalServerError
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	if code == "internal" {
		msg = "internal server error"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "details": map[string]any{}},
	})
}

func (s *Server) notImplemented(w http.ResponseWriter, _ *http.Request) {
	s.writeError(w, http.StatusNotImplemented, "not_implemented", "endpoint not implemented in this slice")
}

func unsafeMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

// withCSRF applies the same boundary to all state change requests for browser cookie authentication.
// Bearer-only CLI requests and webhook/login entries without cookies are not targets of CSRF.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if !unsafeMethod(r.Method) || err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-Cxt-CSRF") != "1" || !s.trustedBrowserOrigin(r) {
			s.writeError(w, http.StatusForbidden, "csrf_rejected", "trusted Origin and X-Cxt-CSRF header are required for cookie-authenticated state changes")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) trustedBrowserOrigin(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("Referer"))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return false
	}
	origin := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	if s.allowOrigin(origin) == origin {
		return true
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	expectedScheme := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
	if expectedScheme == "" && r.TLS != nil {
		expectedScheme = "https"
	}
	return expectedScheme == "" || strings.EqualFold(u.Scheme, expectedScheme)
}

// withCORS allows cross-origin requests with credentials (cookies).
// Since HttpOnly session cookies are used, `Allow-Origin: *` is not possible — the request Origin is validated and echoed back.
// Allow-Credentials: true is sent with the response (browser rules). CORS is disabled for same-origin (proxy) deployments.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := s.allowOrigin(r.Header.Get("Origin")); origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Cxt-CSRF, X-Cxt-Identity")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowOrigin selects the Origin to reflect. Only matches if a whitelist (CXT_CORS_ORIGINS) is present.
// If the whitelist is empty, only **loopback origins** are reflected (dev convenience) — reflecting an arbitrary Origin with credentials effectively becomes `*`+cookies, which is absolutely forbidden.
func (s *Server) allowOrigin(origin string) string {
	if origin == "" {
		return ""
	}
	if len(s.cors) == 0 {
		if isLoopbackOrigin(origin) {
			return origin
		}
		return ""
	}
	for _, o := range s.cors {
		if o == origin {
			return origin
		}
	}
	return ""
}

// isLoopbackOrigin determines if the Origin is of the form http://localhost[:port] / http://127.0.0.1[:port].
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
