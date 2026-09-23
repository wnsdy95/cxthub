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
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Backend is a set of server actions required by REST handlers (app.Service implements).
type Backend interface {
	inbound.ContextQuery
	inbound.GraphStateQuery
	GetRepositoryView(context.Context, domain.ContentHash) (domain.RepositoryView, error)
	GetPendingView(context.Context, domain.ContentHash) (domain.PendingView, error)
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
	Negotiate(ctx context.Context, in inbound.PushNegotiateInput) (inbound.PushNegotiateOutput, error)
	StoreChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error)
	PullChunks(ctx context.Context, in inbound.PullChunksInput) (inbound.PullChunksOutput, error)
	Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error)
	Send(ctx context.Context, in inbound.PullSendInput) (inbound.PullSendOutput, error)
	UpdateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error)
	UpdateRefs(ctx context.Context, in inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error)
	List(ctx context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error)
	Diff(ctx context.Context, in inbound.DiffInput) (inbound.DiffOutput, error)
	Search(ctx context.Context, in inbound.SearchInput) (inbound.SearchOutput, error)
	Fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error)
	PromoteSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error
	GraftSnapshotParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error
	inbound.JoinPreview
	GetManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error)
	EnsureRepo(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error)
	ListRepos(ctx context.Context, team string) ([]domain.Repo, error)
	Contributions(ctx context.Context, repositoryIDs []string) (map[string]int, error)
	Activity(ctx context.Context, repositories []domain.Repository) ([]domain.ActivityMonth, error)
	GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error)
	Fsck(ctx context.Context, repoID domain.ContentHash) (inbound.FsckReport, error)
	Reflog(ctx context.Context, repoID domain.ContentHash) ([]domain.RefLogEntry, error)
	ListHistory(ctx context.Context, repoID domain.ContentHash) ([]domain.HistoryEvent, error)
	RecordHistory(ctx context.Context, event domain.HistoryEvent) error
	EnableContextProtocol(context.Context, domain.ContentHash) error
	GetSnapshot(ctx context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error)
	GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error)
	ListRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error)
	// MemoryDigest: snapshot derivative carried with the raw document (compatibility rules).
	PutMemoryDigest(ctx context.Context, repoID domain.ContentHash, d domain.MemoryDigest) (domain.ContentHash, error)
	PutMemoryDigestCAS(ctx context.Context, repoID domain.ContentHash, d domain.MemoryDigest) (domain.ContentHash, error)
	GetMemoryDigest(ctx context.Context, repoID, snapshotID domain.ContentHash) (domain.MemoryDigest, error)
	GetMemoryObject(ctx context.Context, repoID, hash domain.ContentHash) (domain.MemoryDigest, error)
	// About + team default settings bundle (web editing).
	UpdateAbout(ctx context.Context, repoID domain.ContentHash, description, website string, topics []string) error
	PatchRepoProfile(context.Context, domain.ContentHash, inbound.RepoProfilePatch) (domain.Repo, error)
	PutSettings(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettings(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error)
	inbound.SaveSecrets
	GetSecrets(ctx context.Context, repoID domain.ContentHash) ([]byte, error)
	PutSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash, bundle domain.SettingsBundle) error
	GetSettingsObjectByHash(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error)
	// In-progress context pointer (CLI hook capture mirror — resolved by deletion on commit, for web display).
	PutPending(ctx context.Context, repoID domain.ContentHash, sessionID string, p domain.Pending) error
	ListPendings(ctx context.Context, repoID domain.ContentHash) ([]domain.Pending, error)
	DeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	CompareAndDeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string, expected domain.ContentHash) (bool, error)
	DismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	UndismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error
	// GitHub PR merged webhook → append base context from head branch to base.
	PromoteMergedPR(ctx context.Context, gitURL string, pr domain.PullRequestMerge) (int, error)
	PromoteRepositoryPR(ctx context.Context, repoID domain.ContentHash, pr domain.PullRequestMerge) (inbound.UpdateRefOutput, error)
	// Push pending (unsync) pointer ((user, branch) key — resolved by deletion on git push, for On Hold display).
	PutUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string, u domain.Unsync) error
	ListUnsyncs(ctx context.Context, repoID domain.ContentHash) ([]domain.Unsync, error)
	DeleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error
	// GitHub public state sync (GHVisibilitySync enabled repository — GitHub → cxthub unidirectional).
	SyncRepositoryVisibility(ctx context.Context, repositoryID string) (domain.Repository, error)
	// Repository structure setup (default branch, protected branch) — for maintainers and above.
	UpdateRepoConfig(ctx context.Context, repoID domain.ContentHash, defaultBranch *string, protectDefault *bool) error
}

// Server binds REST handlers to Backend (session synchronization) + IdentityBackend (authentication/repository).
type Server struct {
	gitSyncAudit      inbound.GitSyncAudit
	docFinalization   inbound.DocFinalization
	effectiveMemory   inbound.EffectiveMemoryQuery
	memoryPositions   inbound.MemoryPositionQuery
	codeApplicability inbound.CodeApplicabilityQuery
	gitChanges        inbound.GitChanges
	gitScans          inbound.GitScans
	changes           repositoryChangeHub
	// syncInflight is a guard against duplicate execution of GitHub sync lazy TTL (repository ID set).
	syncInflight sync.Map
	runtime      outbound.RuntimeStore
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
	return &Server{b: b, id: id, docFinalization: docFinalization, runtime: runtime, cookie: loadCookieCfg(), cors: splitCSV(os.Getenv("CXT_CORS_ORIGINS"))}
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
	mux.HandleFunc("GET /api/v1/repos/{repoID}/fsck", s.guard(domain.RoleViewer, s.fsck))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/reflog", s.guard(domain.RoleViewer, s.reflog))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/github-sync-check", s.guard(domain.RoleMaintainer, s.checkGitHubSync))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/context-protocol", s.guard(domain.RoleMaintainer, s.enableContextProtocol))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/history", s.guard(domain.RoleViewer, s.listHistory))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/prs/promotions", s.guard(domain.RoleMember, s.submitPRPromotion))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/prs/promotions", s.guard(domain.RoleViewer, s.listPRPromotions))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/prs/promotions/{jobID}/retry", s.guard(domain.RoleMember, s.retryPRPromotion))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/prs/promote", s.guard(domain.RoleMember, s.promoteRepositoryPR))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/history", s.guard(domain.RoleMember, s.recordHistory))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/manifest", s.guard(domain.RoleViewer, s.getManifest))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/branches", s.guard(domain.RoleViewer, s.listRefs))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/refs", s.guard(domain.RoleViewer, s.listRefs))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/refs/batch", s.guard(domain.RoleMember, s.putRefs))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/context-query", s.guard(domain.RoleViewer, s.contextQuery))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/git-changes", s.guard(domain.RoleMember, s.submitGitChange))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/effective-memory", s.guard(domain.RoleViewer, s.queryEffectiveMemory))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/effective-memory/positions", s.guard(domain.RoleViewer, s.queryMemoryPositions))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/code-applicability", s.guard(domain.RoleViewer, s.queryCodeApplicability))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/git-scans", s.guard(domain.RoleViewer, s.listGitScans))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/git-scans/{scanID}/retry", s.guard(domain.RoleMember, s.retryGitScan))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/git-changes", s.guard(domain.RoleViewer, s.listGitChanges))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/git-changes/{changeID}", s.guard(domain.RoleViewer, s.getGitChange))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/git-changes/{changeID}/retry", s.guard(domain.RoleMember, s.retryGitChange))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/view", s.guard(domain.RoleViewer, compressedGraphRead(s.repositoryView)))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/pending-view", s.guard(domain.RoleViewer, compressedGraphRead(s.pendingView)))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/graph-state", s.guard(domain.RoleViewer, compressedGraphRead(s.graphState)))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/revision", s.guard(domain.RoleViewer, s.repositoryRevision))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/changes", s.guard(domain.RoleViewer, s.repositoryChanges))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/snapshots", s.guard(domain.RoleViewer, s.listSnapshots))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/snapshots/{id}", s.guard(domain.RoleViewer, s.getSnapshot))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/snapshots/{id}/promote", s.guard(domain.RoleMember, s.promoteSnapshot))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/snapshots/{id}/graft", s.guard(domain.RoleMember, s.graftSnapshot))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/join/preview", s.guard(domain.RoleMember, s.previewJoin))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/join", s.guard(domain.RoleMember, s.joinSnapshot))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/search", s.guard(domain.RoleViewer, s.search))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/docs/{hash}", s.guard(domain.RoleViewer, s.getDoc))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/docs/{hash}/events", s.guard(domain.RoleViewer, s.getDocEvents))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/memories/{snapshotID}", s.guard(domain.RoleViewer, s.getMemory))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/memory-objects/{hash}", s.guard(domain.RoleViewer, s.getMemoryObject))
	// Team-asset reads require puller. Settings writes use the action gate;
	// secrets writes authorize inside the application transaction.
	mux.HandleFunc("PATCH /api/v1/repos/{repoID}/about", s.guard(domain.RoleMaintainer, s.patchAbout))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/settings/{kind}", s.guard(domain.RolePuller, s.getSettings))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/settings/{kind}", s.requireUser(s.putSettings))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/secrets", s.requireUser(s.putSecrets))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/secrets", s.guard(domain.RolePuller, s.getSecrets))
	// Commit attachment settings object: Write = context push part (member), Read = pull layer (puller).
	mux.HandleFunc("GET /api/v1/repos/{repoID}/settings-objects/{hash}", s.guard(domain.RolePuller, s.getSettingsObject))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/settings-objects/{hash}", s.guard(domain.RoleMember, s.putSettingsObject))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/memories/{snapshotID}", s.guard(domain.RoleMember, s.putMemory))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/memory-attachments/{snapshotID}", s.guard(domain.RoleMember, s.putMemoryAttachment))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/memory-publications", s.guard(domain.RoleMember, s.publishMemoryArchive))
	mux.HandleFunc("PUT /api/v1/repos/{repoID}/typed-memory-attachments/{snapshotID}", s.guard(domain.RoleMember, s.putTypedMemoryAttachment))
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
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/doc-jobs", s.guard(domain.RoleMember, s.submitDocFinalization))
	mux.HandleFunc("GET /api/v1/repos/{repoID}/push/doc-jobs/{jobID}", s.guard(domain.RoleMember, s.getDocFinalization))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/push/objects", s.guard(domain.RoleMember, s.pushObjects))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/pull/chunks", s.guard(domain.RolePuller, s.pullChunks))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/pull/objects", s.guard(domain.RolePuller, s.pullObjects))

	// Actions exposed by the server. Provider-session restoration remains a local
	// CLI operation and is deliberately absent from the HTTP surface.
	mux.HandleFunc("POST /api/v1/repos/{repoID}/diff", s.guard(domain.RoleViewer, s.diff))
	mux.HandleFunc("POST /api/v1/repos/{repoID}/fork", s.guard(domain.RoleMember, s.fork))

	// Authentication · Repository · Invite (all Firebase/dev tokens required — requireUser middleware).
	s.registerIdentity(mux)

	return s.withSecurityHeaders(s.withCORS(s.withCSRF(s.withCorrelation(mux))))
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
	repo, err := s.b.GetRepo(r.Context(), s.repoID(r))
	if err != nil {
		code, status := mapError(err)
		s.writeError(w, status, code, err.Error())
		return false
	}
	u, authed := userFrom(r.Context())
	if repo.RepositoryID == "" {
		s.writeError(w, http.StatusForbidden, "repo_unbound",
			"repository is not bound to a repository — verify that <namespace>/<repository>/<repository> in the remote URL matches an existing repository, then reconnect with cxt setup <url>")
		return false
	}
	repositoryRecord, werr := s.id.GetRepository(r.Context(), repo.RepositoryID)
	if werr != nil {
		code, status := mapError(werr)
		s.writeError(w, status, code, werr.Error())
		return false
	}
	// Public repository: non-members (including anonymous) receive the default role set by the repository (viewer by default, owner can go up to puller). Members are determined by their actual role below.
	if repositoryRecord.IsPublic() {
		if repositoryRecord.PublicBaseRole().AtLeast(min) && (min == domain.RoleViewer || !repositoryRecord.Archived) {
			return true // Satisfies the default role for non-members (viewer exceeds storage is blocked)
		}
	}
	if !authed {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "Login required — cxt login <token> (generate from Account Settings ⚙)")
		return false
	}
	// The archived repository is read-only — deny actions beyond viewer (check after authentication —
	// does not leak repository status to anonymous users).
	if min != domain.RoleViewer && repositoryRecord.Archived {
		s.writeError(w, http.StatusForbidden, "forbidden", "archived repository (read-only) — owner can disable in settings")
		return false
	}
	role, ok := s.id.RoleOf(r.Context(), repo.RepositoryID, u.ID)
	if !ok || !role.AtLeast(min) {
		// Break-glass is a narrow read-only exception. It never satisfies pull,
		// push, settings, secrets, or any other write-capable role gate.
		if min == domain.RoleViewer {
			allowed, accessErr := s.id.HasBreakGlassAccess(r.Context(), repo.RepositoryID, u.ID)
			if accessErr != nil {
				s.writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "break-glass audit unavailable")
				return false
			}
			if allowed {
				return true
			}
		}
		s.writeError(w, http.StatusForbidden, "forbidden", "insufficient role permissions — at least "+string(min)+" required")
		return false
	}
	return true
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
	repo, err := s.b.PatchRepoProfile(r.Context(), s.repoID(r), body)
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
	out, err := s.b.SaveSecrets(r.Context(), inbound.SaveSecretsInput{
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
		if repo, rerr := s.b.GetRepo(r.Context(), s.repoID(r)); rerr == nil && repo.RepositoryID != "" {
			if role, ok := s.id.RoleOf(r.Context(), repo.RepositoryID, caller.ID); ok && role.AtLeast(domain.RoleMaintainer) {
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
	if expected := domain.ContentHash(r.URL.Query().Get("expect")); expected != "" {
		deleted, err := s.b.CompareAndDeletePending(r.Context(), s.repoID(r), sessionID, expected)
		status := "kept"
		if deleted {
			status = "deleted"
		}
		s.respond(w, map[string]string{"status": status}, err)
		return
	}
	err := s.b.DeletePending(r.Context(), s.repoID(r), sessionID)
	s.respond(w, map[string]string{"status": "deleted"}, err)
}

// dismissPending hides the capture from the uncommitted list (no data deletion — dismissed flag).
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

func (s *Server) putMemoryAttachment(w http.ResponseWriter, r *http.Request) {
	s.putMemoryAttachmentVersion(w, r, false)
}

func (s *Server) putTypedMemoryAttachment(w http.ResponseWriter, r *http.Request) {
	s.putMemoryAttachmentVersion(w, r, true)
}

func (s *Server) putMemoryAttachmentVersion(w http.ResponseWriter, r *http.Request, typed bool) {
	var d domain.MemoryDigest
	if !s.decode(w, r, &d) {
		return
	}
	if (typed && d.ClaimsVersion != domain.MemoryClaimsVersion) || (!typed && (d.ClaimsVersion != 0 || d.HasMemoryClaims())) {
		s.respond(w, nil, fmt.Errorf("%w: memory claims version does not match attachment endpoint", domain.ErrValidation))
		return
	}
	d.SnapshotID = domain.ContentHash(r.PathValue("snapshotID"))
	hash, err := s.b.PutMemoryDigestCAS(r.Context(), s.repoID(r), d)
	s.respond(w, map[string]any{"memory_hash": hash}, err)
}

func (s *Server) getMemoryObject(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetMemoryObject(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")))
	s.respond(w, out, err)
}

func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) {
	out, err := s.b.GetDoc(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")))
	s.respond(w, out, err)
}

type putRefBody struct {
	BranchID       string             `json:"branch_id,omitempty"`
	Target         domain.ContentHash `json:"target"`
	ExpectedTarget domain.ContentHash `json:"expected_target"`
	Symbolic       string             `json:"symbolic"`
	// Force allows non-fast-forward moves (git push --force).
	Force bool `json:"force,omitempty"`
	// Append is like diverged push, grafting the diverged push onto the current head (cxt push --append).
	Append bool `json:"append,omitempty"`
}

type putRefsBody struct {
	Updates []struct {
		Ref    domain.Ref `json:"ref"`
		Force  bool       `json:"force,omitempty"`
		Append bool       `json:"append,omitempty"`
	} `json:"updates"`
}

func (s *Server) putRefs(w http.ResponseWriter, r *http.Request) {
	var body putRefsBody
	if !s.decodeLimited(w, r, &body, inbound.MaxRefBatchJSONBody) {
		return
	}
	if len(body.Updates) > inbound.MaxRefBatchUpdates {
		s.respond(w, nil, fmt.Errorf("%w: at most %d ref updates per batch", domain.ErrValidation, inbound.MaxRefBatchUpdates))
		return
	}
	rid := s.repoID(r)
	updates := make([]inbound.UpdateRefInput, 0, len(body.Updates))
	for _, update := range body.Updates {
		update.Ref.RepoID = rid
		updates = append(updates, inbound.UpdateRefInput{
			RepoID: rid, Ref: update.Ref, Force: update.Force, Append: update.Append,
		})
	}
	out, err := s.b.UpdateRefs(r.Context(), inbound.UpdateRefsInput{RepoID: rid, Updates: updates})
	s.respond(w, out, err)
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
	if r.Header.Get("X-GitHub-Event") == "push" {
		s.githubPush(w, r, body)
		return
	}
	if ev := r.Header.Get("X-GitHub-Event"); ev != "pull_request" {
		s.respond(w, map[string]string{"status": "ignored", "event": ev}, nil)
		return
	}
	var payload struct {
		Number      int    `json:"number"`
		Action      string `json:"action"`
		PullRequest struct {
			MergeSHA string `json:"merge_commit_sha"`
			Merged   bool   `json:"merged"`
			Base     struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				SHA  string `json:"sha"`
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
	promote := s.b.PromoteMergedPR
	if durable, ok := s.b.(interface {
		SubmitMergedPR(context.Context, string, domain.PullRequestMerge) (int, error)
	}); ok {
		promote = durable.SubmitMergedPR
	}
	n, perr := promote(inbound.WithSystemActor(r.Context()), gitURL, domain.PullRequestMerge{Number: payload.Number, BaseBranch: payload.PullRequest.Base.Ref, HeadBranch: payload.PullRequest.Head.Ref, HeadSHA: payload.PullRequest.Head.SHA, MergeSHA: payload.PullRequest.MergeSHA})
	s.respond(w, map[string]interface{}{"status": "accepted", "queued": n}, perr)
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
		BranchID: body.BranchID,
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
func (s *Server) previewJoin(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	out, err := s.b.PreviewJoin(r.Context(), inbound.JoinPreviewInput{ActorID: u.ID, RepoID: s.repoID(r), Snapshot: domain.ContentHash(r.URL.Query().Get("snapshot")), Branch: r.URL.Query().Get("branch")})
	s.respond(w, out, err)
}

func (s *Server) joinSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedHead       domain.ContentHash `json:"expected_head"`
		PlanRevision       domain.ContentHash `json:"plan_revision"`
		BranchID           string             `json:"branch_id,omitempty"`
		Branch             string             `json:"branch"`
		Snapshot           domain.ContentHash `json:"snapshot"`
		IncludeDescendants bool               `json:"include_descendants"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.b.ConfirmJoin(r.Context(), inbound.ConfirmJoinInput{ActorID: u.ID, JoinInput: inbound.JoinInput{
		ExpectedHead: body.ExpectedHead, PlanRevision: body.PlanRevision,
		RepoID: s.repoID(r), TargetBranch: body.Branch, BranchID: body.BranchID, Snapshot: body.Snapshot,
		IncludeDescendants: body.IncludeDescendants,
	}})
	s.respond(w, out, err)
}

type pullBody struct {
	SnapshotWants         []domain.ContentHash `json:"snapshot_wants"`
	DocWants              []domain.ContentHash `json:"doc_wants"`
	DocManifestWants      []domain.ContentHash `json:"doc_manifest_wants"`
	ChunkWants            []domain.ContentHash `json:"chunk_wants"`
	ChunkFormatsSupported []string             `json:"chunk_formats_supported"`
	CIRVersionsSupported  []string             `json:"cir_versions_supported"`
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
	out, err := s.b.Send(r.Context(), inbound.PullSendInput{RepoID: s.repoID(r), SnapshotWants: body.SnapshotWants, DocWants: body.DocWants, DocManifestWants: body.DocManifestWants, ChunkWants: body.ChunkWants, ChunkFormatsSupported: body.ChunkFormatsSupported, CIRVersionsSupported: body.CIRVersionsSupported})
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
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds transport limit")
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
	case errors.Is(err, domain.ErrNonFastForward):
		return "non_fast_forward", http.StatusConflict
	case errors.Is(err, domain.ErrRefConflict):
		return "ref_conflict", http.StatusConflict
	case errors.Is(err, domain.ErrBranchArchived):
		return "branch_archived", http.StatusConflict
	case errors.Is(err, domain.ErrUnauthorized):
		return "unauthenticated", http.StatusUnauthorized
	case errors.Is(err, domain.ErrForbidden):
		return "forbidden", http.StatusForbidden
	case errors.Is(err, domain.ErrGitOriginMismatch):
		return "git_origin_mismatch", http.StatusConflict
	case errors.Is(err, domain.ErrUnsupportedCIRVersion):
		return "unsupported_cir_version", http.StatusConflict
	case errors.Is(err, domain.ErrJoinPreviewChanged):
		return "join_preview_changed", http.StatusConflict
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
	if code == "consistency_check_failed" {
		msg = domain.ErrSecretsConsistency.Error()
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
