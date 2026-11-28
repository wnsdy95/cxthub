// Package http is the REST delivery adapter for the sync API.
//
// base = /api/v1/... , body application/json. frontend uses a separate CDN, so CORS is allowed.
// Handlers call a single Backend interface (consumer-defined) — app.Service implements this.
package http

import (
	"context"
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

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Backend is a set of server actions required by REST handlers (app.Service implements).
type Backend interface {
	Negotiate(ctx context.Context, in inbound.PushNegotiateInput) (inbound.PushNegotiateOutput, error)
	StoreChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error)
	Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error)
	EnsureRepo(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error)
	ListRepos(ctx context.Context, team string) ([]domain.Repo, error)
	GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error)
	PatchRepoProfile(context.Context, domain.ContentHash, inbound.RepoProfilePatch) (domain.Repo, error)
	PutSettings(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettings(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error)
	inbound.SaveSecrets
	GetSecrets(ctx context.Context, repoID domain.ContentHash) ([]byte, error)
	// GitHub public state sync (GHVisibilitySync enabled repository — GitHub → cxthub unidirectional).
	SyncRepositoryVisibility(ctx context.Context, repositoryID string) (domain.Repository, error)
}

// Server binds REST handlers to Backend (session synchronization) + IdentityBackend (authentication/repository).
type Server struct {
	deviceVerificationOrigin string
	githubConnections        GitHubConnections
	githubAppSecret          string
	githubReturnURL          string
	docFinalization          inbound.DocFinalization
	documentViews            inbound.DocumentViewQueries
	documentQueryProfile     inbound.DocumentQueryProfile
	repositoryAssets         inbound.RepositoryAssetCommands
	documentPublications     inbound.DocumentPublicationCommands
	// syncInflight is a guard against duplicate execution of GitHub sync lazy TTL (repository ID set).
	syncInflight sync.Map
	runtime      outbound.RuntimeStore
	allowance    localAllowance // per-process GCRA checked before, and used instead of, runtime allowances
	b            Backend
	id           IdentityBackend
	cookie       cookieCfg // session cookie attributes (env injected; tuned per deployment topology)
	cors         []string  // allowed Origin whitelist (empty reflects requested Origin — dev convenience)
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
	var runtime outbound.RuntimeStore
	if provider, ok := id.(interface{ RuntimeStore() outbound.RuntimeStore }); ok {
		runtime = provider.RuntimeStore()
	}
	docFinalization, _ := b.(inbound.DocFinalization)
	return &Server{b: b, id: id, repositoryAssets: b, documentPublications: b,
		docFinalization: docFinalization, runtime: runtime, cookie: loadCookieCfg(), cors: splitCSV(os.Getenv("CXT_CORS_ORIGINS"))}
}

func (server *Server) SetRepositoryAssetCommands(commands inbound.RepositoryAssetCommands) error {
	if commands == nil {
		return fmt.Errorf("%w: repository asset commands are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(commands); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: repository asset commands are required", domain.ErrValidation)
		}
	}
	server.repositoryAssets = commands
	return nil
}

func (server *Server) SetDocumentQueries(queries inbound.DocumentQueries, profile inbound.DocumentQueryProfile) error {
	if err := server.SetDocumentEventQueries(queries, profile); err != nil {
		return err
	}
	server.documentViews = queries
	return nil
}

func (server *Server) SetDocumentEventQueries(queries inbound.DocumentEventQueries, profile inbound.DocumentQueryProfile) error {
	if profile != inbound.DocumentQueryCompatibility && profile != inbound.DocumentQueryVerifiedRoots {
		return fmt.Errorf("%w: explicit document query profile is required", domain.ErrValidation)
	}
	if queries == nil {
		return fmt.Errorf("%w: document event queries are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(queries); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: document event queries are required", domain.ErrValidation)
		}
	}
	server.documentQueryProfile = profile
	return nil
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
	// GET at viewer level is allowed for public repositories. Detailed rules are in requireRepoRole.
	mux.HandleFunc("GET /api/v1/repos", s.listRepos)
	mux.HandleFunc("POST /api/v1/repos", s.requireUser(s.createRepo)) // distillation (empirically verified usage of bound repositoryRecord is checked by subsequent gate)
	mux.HandleFunc("GET /api/v1/repos/{repoID}", s.guard(domain.RoleViewer, s.getRepo))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-sessions", s.guard(domain.RoleViewer, s.listAnchoredSessions))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}", s.guard(domain.RoleViewer, s.getAnchoredSession))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}", s.guard(domain.RoleMember, s.putAnchoredSession))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-commits/{sha}", s.guard(domain.RoleViewer, s.anchoredCommitSegments))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-commits/{sha}/summaries", s.guard(domain.RoleViewer, s.listAnchoredCommitSummaries))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/summaries/{sha}", s.guard(domain.RoleMember, s.putAnchoredSummary))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/branch-ids", s.guard(domain.RoleMember, s.putBranchIDs))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/branch-lifetimes", s.guard(domain.RoleViewer, s.listBranchLifetimes))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/calls", s.guard(domain.RoleViewer, s.anchoredSessionCalls))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/events", s.guard(domain.RoleViewer, s.getAnchoredSessionEvents))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/event-fragments", s.guard(domain.RoleViewer, s.getAnchoredSessionEventFragment))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-search", s.guard(domain.RoleViewer, s.bulkRead(s.searchAnchored)))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-memory", s.guard(domain.RoleViewer, s.bulkRead(s.readAnchoredMemory)))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/archive", s.guard(domain.RoleMember, s.setAnchoredSessionArchive))
	mux.HandleFunc("DELETE /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}", s.guard(domain.RoleMember, s.removeAnchoredSession))
	mux.HandleFunc("DELETE /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/removal", s.guard(domain.RoleMember, s.allowAnchoredSession))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-removals", s.guard(domain.RoleMember, s.listAnchoredSessionRemovals))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-sessions/{provider}/{sessionID}/content", s.guard(domain.RoleViewer, s.bulkRead(s.readAnchoredSessionContent)))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/anchored-notes", s.guard(domain.RoleViewer, s.listAnchoredNotes))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/anchored-notes", s.guard(domain.RoleMember, s.createAnchoredNote))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/anchored-notes/{noteID}", s.guard(domain.RoleMember, s.updateAnchoredNote))
	mux.HandleFunc("DELETE /api/v1/repos/{repoID}/anchored-notes/{noteID}", s.guard(domain.RoleMember, s.deleteAnchoredNote))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/worktrees", s.guard(domain.RoleViewer, s.listWorktrees))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/worktrees", s.guard(domain.RoleMember, s.putWorktrees))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/collector", s.guard(domain.RoleMember, s.putCollector))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/push-origins", s.guard(domain.RoleViewer, s.getPushOrigins))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/github-connection", s.guard(domain.RoleViewer, s.getGitHubConnection))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/revision", s.guard(domain.RoleViewer, s.getViewRevision))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/remote-git", s.guard(domain.RoleViewer, s.getRemoteGit))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/remote-git/refresh", s.guard(domain.RoleMember, s.refreshRemoteGit))
	// Team-asset reads require puller. Settings writes use the action gate;
	// secrets writes authorize inside the application transaction.
	mux.HandleFunc("PATCH /api/v1/repos/{repoID}/about", s.guard(domain.RoleMaintainer, s.patchAbout))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/settings/{kind}", s.guard(domain.RolePuller, s.getSettings))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/settings/{kind}", s.requireUser(s.putSettings))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/secrets", s.requireUser(s.putSecrets))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/secrets", s.guard(domain.RolePuller, s.getSecrets))
	// Commit attachment settings object: Write = context push part (member), Read = pull layer (puller).
	// In-progress context pointer: Write/Delete = context push layer (member), Read = pull/web layer (puller).
	// Push pending (unsync) pointer: Key = (authenticated user, branch) — write/delete only by owner.
	// {name...}: Branch names can include slashes like in git (feature/login, etc.) — full matching of the rest.

	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/negotiate", s.guard(domain.RoleMember, s.pushNegotiate))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/chunks", s.guard(domain.RoleMember, s.pushChunks))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/doc-jobs", s.guard(domain.RoleMember, s.submitDocFinalization))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/push/doc-jobs/{jobID}", s.guard(domain.RoleMember, s.getDocFinalization))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/objects", s.guard(domain.RoleMember, s.pushObjects))

	// Actions exposed by the server. Provider-session restoration remains a local
	// CLI operation and is deliberately absent from the HTTP surface.

	// Authentication · Repository · Invite (all Firebase/dev tokens required — requireUser middleware).
	s.registerIdentity(mux)

	return s.withSecurityHeaders(s.withCORS(s.withCSRF(s.withCorrelation(s.withDocumentIdentityPeer(mux)))))
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

// listRepos: if ?repository=<id> is present, requires login + corresponding repository membership and
// returns only repos within that repository (visibility boundary). Without parameters, returns accessible repos only.
func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	repositoryID := r.URL.Query().Get("repository")
	team := r.URL.Query().Get("team")
	if team == "" {
		team = "default"
	}
	repos, err := s.b.ListRepos(r.Context(), team)
	if err != nil {
		s.respond(w, repos, err)
		return
	}
	if repositoryID == "" {
		// Repository filter-less full list exposes enumeration info (review found #4) —
		// Anonymous gets empty list (server survival check 200), authenticated user gets only accessible ones:
		// My member repositories · Public repositories. Membership or policy lookup failures are hidden.
		token := s.requestToken(r)
		u, uerr := s.id.ResolveUser(r.Context(), token)
		if token == "" || uerr != nil {
			s.respond(w, []domain.Repo{}, nil)
			return
		}
		visible := make([]domain.Repo, 0, len(repos))
		for _, rp := range repos {
			if rp.RepositoryID == "" {
				continue
			}
			repositoryRecord, werr := s.id.GetRepository(r.Context(), rp.RepositoryID)
			if werr != nil {
				continue
			}
			if repositoryRecord.IsPublic() {
				visible = append(visible, rp)
				continue
			}
			if _, ok := s.id.RoleOf(r.Context(), rp.RepositoryID, u.ID); ok {
				visible = append(visible, rp)
			}
		}
		s.respond(w, visible, nil)
		return
	}
	// Repository filter — members only. However, public (public) repositories are accessible by anyone.
	repositoryRecord, werr := s.id.GetRepository(r.Context(), repositoryID)
	if werr != nil {
		code, status := mapError(werr)
		s.writeError(w, status, code, werr.Error())
		return
	}
	if !repositoryRecord.IsPublic() {
		token := s.requestToken(r)
		u, uerr := s.id.ResolveUser(r.Context(), token)
		if token == "" || uerr != nil {
			s.writeError(w, http.StatusUnauthorized, "unauthenticated", "repository filter requires login")
			return
		}
		if _, merr := s.id.ListMembers(r.Context(), u.ID, repositoryID); merr != nil {
			allowed, accessErr := s.id.HasBreakGlassAccess(r.Context(), repositoryID, u.ID)
			if accessErr != nil {
				s.writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "break-glass audit unavailable")
				return
			}
			if !allowed {
				code, status := mapError(merr)
				s.writeError(w, status, code, "not a repository member")
				return
			}
		}
	}
	filtered := make([]domain.Repo, 0, len(repos))
	for _, rp := range repos {
		if rp.RepositoryID == repositoryID {
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

// getRepo answers the repository together with the document identities this
// server reads, which a CLI needs to know before it uploads a conversation.
func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	ids, enabled := s.documentIdentityCapabilities()
	out, err := s.b.GetRepo(r.Context(), s.repoID(r))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	s.respond(w, repoPullView{Repo: out, DocIdentitiesSupported: ids, RootPublicationEnabled: enabled}, nil)
}

// requireRepoMember checks if the caller is a member of the repo if it belongs to the repository.
// guard gates the route as a 5-step role ladder (serial AND front — roles are layer boundaries,
// policy specifics are narrowed by requireRepoAction behind).
//
//   - If a token is present, it is interpreted and injected into the context (anonymous is initially allowed — determination is by requireRepoRole).
//   - Reading at the viewer level in a public repository is allowed for anonymous users (GitHub public repo compatibility).
//   - Repos not belonging to the repository have no policy boundary and are all rejected.
func (s *Server) guard(min domain.MemberRole, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := s.requestToken(r); token != "" {
			if u, err := s.id.ResolveUser(r.Context(), token); err == nil {
				r = r.WithContext(inbound.WithRepositoryActor(context.WithValue(r.Context(), userCtxKey, u), u.ID))
			}
		}
		if !s.requireRepoRole(w, r, min) {
			return
		}
		fn(w, r)
	}
}

// kickVisibilitySync runs GitHub public sync in the background once (preventing duplicates).
func (s *Server) kickVisibilitySync(repositoryID string) {
	if _, running := s.syncInflight.LoadOrStore(repositoryID, true); running {
		return
	}
	go func() {
		defer s.syncInflight.Delete(repositoryID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.b.SyncRepositoryVisibility(ctx, repositoryID)
	}()
}

// requireRepoRole checks if the user's role in the repo's repository is at least min.
//
//   - Reading at the viewer level in a public repository is allowed for anonymous users (GitHub public repo compatibility).
//   - Repos not belonging to the repository have no policy boundary and are all rejected.
func (s *Server) requireRepoRole(w http.ResponseWriter, r *http.Request, min domain.MemberRole) bool {
	if err := s.checkRepoRole(r.Context(), s.repoID(r), min); err != nil {
		s.respond(w, nil, err)
		return false
	}
	return true
}

type httpAccessError struct {
	status  int
	code    string
	message string
	cause   error
}

func (err *httpAccessError) Error() string { return err.message }
func (err *httpAccessError) Unwrap() error { return err.cause }

func (s *Server) checkRepoRole(ctx context.Context, repoID domain.ContentHash, min domain.MemberRole) error {
	repo, err := s.b.GetRepo(ctx, repoID)
	if err != nil {
		code, status := mapError(err)
		return &httpAccessError{status: status, code: code, message: err.Error(), cause: err}
	}
	u, authed := userFrom(ctx)
	if repo.RepositoryID == "" {
		return &httpAccessError{status: http.StatusForbidden, code: "repo_unbound", cause: domain.ErrForbidden,
			message: "repository is not bound to a repository — verify that <namespace>/<repository>/<repository> in the remote URL matches an existing repository, then reconnect with cxt setup <url>"}
	}
	repositoryRecord, werr := s.id.GetRepository(ctx, repo.RepositoryID)
	if werr != nil {
		code, status := mapError(werr)
		return &httpAccessError{status: status, code: code, message: werr.Error(), cause: werr}
	}
	// Public repository: non-members (including anonymous) receive the default role set by the repository (viewer by default, owner can go up to puller). Members are determined by their actual role below.
	if repositoryRecord.IsPublic() {
		if repositoryRecord.PublicBaseRole().AtLeast(min) && (min == domain.RoleViewer || !repositoryRecord.Archived) {
			return nil
		}
	}
	if !authed {
		return &httpAccessError{status: http.StatusUnauthorized, code: "unauthenticated", cause: domain.ErrUnauthorized,
			message: "Login required — cxt login <token> (generate from Account Settings ⚙)"}
	}
	// The archived repository is read-only — deny actions beyond viewer (check after authentication —
	// does not leak repository status to anonymous users).
	if min != domain.RoleViewer && repositoryRecord.Archived {
		return &httpAccessError{status: http.StatusForbidden, code: "forbidden", cause: domain.ErrForbidden,
			message: "archived repository (read-only) — owner can disable in settings"}
	}
	role, ok := s.id.RoleOf(ctx, repo.RepositoryID, u.ID)
	if !ok || !role.AtLeast(min) {
		// Break-glass is a narrow read-only exception. It never satisfies pull,
		// push, settings, secrets, or any other write-capable role gate.
		if min == domain.RoleViewer {
			allowed, accessErr := s.id.HasBreakGlassAccess(ctx, repo.RepositoryID, u.ID)
			if accessErr != nil {
				return &httpAccessError{status: http.StatusServiceUnavailable, code: "audit_unavailable", cause: accessErr,
					message: "break-glass audit unavailable"}
			}
			if allowed {
				return nil
			}
		}
		return &httpAccessError{status: http.StatusForbidden, code: "forbidden", cause: domain.ErrForbidden,
			message: "insufficient role permissions — at least " + string(min) + " required"}
	}
	return nil
}

// requireRepoAction narrows down policy by action after role gate (maintainer and above).
// Settings policy may narrow maintainer access to owners. Unknown policy values
// are denied except for owners. Secrets use the transactional application command.
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
	if repo.RepositoryID == "" {
		s.writeError(w, http.StatusForbidden, "repo_unbound", "repo not bound to repository, policy determination impossible")
		return false
	}
	repositoryRecord, werr := s.id.GetRepository(r.Context(), repo.RepositoryID)
	if werr != nil {
		code, status := mapError(werr)
		s.writeError(w, status, code, werr.Error())
		return false
	}
	if action != "settings" {
		s.writeError(w, http.StatusForbidden, "forbidden", "unsupported settings action")
		return false
	}
	policy := repositoryRecord.SettingsPolicy
	if policy == "" || policy == "members" {
		return true // role-based only (no additional narrowing)
	}
	u, _ := userFrom(r.Context())
	if !domain.PolicyAllows(policy, s.id.IsOwner(r.Context(), repositoryRecord.ID, u.ID)) {
		s.writeError(w, http.StatusForbidden, "forbidden", "action not allowed by repository permissions ("+action+")")
		return false
	}
	return true
}

// patchAbout updates repo About (description/website/topics) + structure settings (default branch, protected branches)
// (route with maintainer gate).
func (s *Server) patchAbout(w http.ResponseWriter, r *http.Request) {
	var body inbound.RepoProfilePatch
	if !s.decode(w, r, &body) {
		return
	}
	repo, err := s.repositoryAssets.PatchRepoProfile(r.Context(), s.repoID(r), body)
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
	if err := s.repositoryAssets.PutSettings(r.Context(), s.repoID(r), bundle); err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	s.respond(w, map[string]any{"kind": bundle.Kind, "files": len(bundle.Files)}, nil)
}

// putSecrets decodes transport input; application owns edit and rotation policy.
func (s *Server) putSecrets(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (512<<10)+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Cannot read encrypted envelope")
		return
	}
	if len(raw) > 512<<10 {
		s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Encrypted envelope too large")
		return
	}
	actor, _ := userFrom(r.Context())
	out, err := s.repositoryAssets.SaveSecrets(r.Context(), inbound.SaveSecretsInput{
		RepoID: s.repoID(r), ActorID: actor.ID, Envelope: raw,
		Edit: domain.SecretsEdit{ExpectedRevision: r.URL.Query().Get("expected_revision"), Rotate: r.URL.Query().Get("rotate") == "true", ExpectedFingerprint: r.URL.Query().Get("expect")},
	})
	s.respond(w, out, err)
}

// getSecrets returns the encrypted envelope as is (decryption is on the client — E2E).
func (s *Server) getSecrets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	raw, err = domain.WithSecretsRevision(raw, domain.SecretsRevision(raw))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
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
	r, release, ok := s.admitResourceRequest(w, r, app.ResourceBulk)
	if !ok {
		return
	}
	defer release()
	var body chunksBody
	if !s.decodeLimited(w, r, &body, inbound.MaxChunkWireJSONBody) {
		return
	}
	out, err := s.documentPublications.StoreChunks(r.Context(), inbound.StoreChunksInput{RepoID: s.repoID(r), Chunks: body.Chunks})
	s.respond(w, out, err)
}

type objectsBody struct {
	Snapshots    []domain.Snapshot     `json:"snapshots"`
	Docs         []domain.SessionDoc   `json:"docs"`
	ChunkedDocs  []inbound.ChunkedDoc  `json:"chunked_docs"`
	ChunkObjects []inbound.ChunkObject `json:"chunk_objects"`
}

func (s *Server) pushObjects(w http.ResponseWriter, r *http.Request) {
	r, release, ok := s.admitResourceRequest(w, r, app.ResourceBulk)
	if !ok {
		return
	}
	defer release()
	var body objectsBody
	if !s.decode(w, r, &body) {
		return
	}
	out, err := s.documentPublications.Commit(r.Context(), inbound.CommitInput{RepoID: s.repoID(r), Snapshots: body.Snapshots, Docs: body.Docs, ChunkedDocs: body.ChunkedDocs, ChunkObjects: body.ChunkObjects})
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
		s.writeDecodeError(w, err)
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
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds transport limit")
			return false
		}
		s.writeDecodeError(w, err)
		return false
	}
	return true
}

func (s *Server) writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return
	}
	s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
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
	var accessErr *httpAccessError
	if errors.As(err, &accessErr) {
		return accessErr.code, accessErr.status
	}
	switch {
	case errors.Is(err, domain.ErrResourceOverloaded):
		return "resource_overloaded", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrDocRangeIndexUnavailable):
		return "doc_range_index_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrFederationUnavailable):
		return "identity_connection_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrVerifiedDomainRequired):
		return "verified_domain_required", http.StatusConflict
	case errors.Is(err, domain.ErrRecentIdentityLogin):
		return "recent_identity_login_required", http.StatusUnauthorized
	case errors.Is(err, domain.ErrDomainVerificationUnavailable):
		return "domain_verification_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrSecretsRevisionRequired):
		return "revision_required", http.StatusPreconditionRequired
	case errors.Is(err, domain.ErrSecretsConflict):
		return "secrets_conflict", http.StatusConflict
	case errors.Is(err, domain.ErrSecretsFingerprintRequired):
		return "fingerprint_required", http.StatusBadRequest
	case errors.Is(err, domain.ErrSecretsRotateConflict):
		return "rotate_conflict", http.StatusConflict
	case errors.Is(err, domain.ErrSecretsPassphraseMismatch):
		return "passphrase_mismatch", http.StatusConflict
	case errors.Is(err, domain.ErrSecretsConsistency):
		return "consistency_check_failed", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrSecretsMalformedEnvelope):
		return "bad_request", http.StatusBadRequest
	case errors.Is(err, domain.ErrStorageLimit):
		return "storage_limit", http.StatusConflict
	case errors.Is(err, domain.ErrUsageUnavailable):
		return "usage_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrNotFound):
		return "not_found", http.StatusNotFound
	case errors.Is(err, domain.ErrIntegrity):
		return "integrity_violation", http.StatusUnprocessableEntity
	case errors.Is(err, domain.ErrRefConflict):
		return "ref_conflict", http.StatusConflict
	case errors.Is(err, domain.ErrUnauthorized):
		return "unauthenticated", http.StatusUnauthorized
	case errors.Is(err, domain.ErrBreakGlassAuditUnavailable):
		return "audit_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrForbidden):
		return "forbidden", http.StatusForbidden
	case errors.Is(err, domain.ErrGitOriginMismatch):
		return "git_origin_mismatch", http.StatusConflict
	case errors.Is(err, domain.ErrUnsupportedCIRVersion):
		return "unsupported_cir_version", http.StatusConflict
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		return "document_identity_upgrade_required", http.StatusConflict
	case errors.Is(err, domain.ErrRootPublicationDisabled):
		return "root_publication_disabled", http.StatusConflict
	case errors.Is(err, domain.ErrUnsupportedDocumentIdentity):
		return "unsupported_document_identity", http.StatusConflict
	case errors.Is(err, domain.ErrConflict):
		return "conflict", http.StatusConflict
	case errors.Is(err, domain.ErrValidation):
		return "validation", http.StatusUnprocessableEntity
	default:
		return "internal", http.StatusInternalServerError
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	if code == "resource_overloaded" || code == "doc_range_index_unavailable" {
		// A handler that knows a quick retry repeats expensive work sets a longer delay first.
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "1")
		}
	}
	if code == "internal" {
		logInternalError(w, msg)
		msg = "internal server error"
	}
	if code == "consistency_check_failed" {
		msg = domain.ErrSecretsConsistency.Error()
	}
	if code == "audit_unavailable" {
		msg = domain.ErrBreakGlassAuditUnavailable.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "details": map[string]any{}},
	})
}

func unsafeMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

// withCSRF applies the same boundary to all state change requests for browser cookie authentication.
// Bearer-only CLI requests and webhook/login entries without cookies are not targets of CSRF.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if !unsafeMethod(r.Method) || err != nil || cookie.Value == "" || isSAMLACS(r) {
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
			h.Set("Access-Control-Expose-Headers", "Retry-After")
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Cxt-CSRF, X-Cxt-Identity, X-Cxt-Doc-Identities")
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
