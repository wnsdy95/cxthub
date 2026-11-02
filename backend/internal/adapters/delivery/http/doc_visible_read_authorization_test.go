package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestDocVisibleHTTPPostAdmissionAuthorization(t *testing.T) {
	for _, mode := range []string{"initial", "cursor", "expand"} {
		for _, change := range []string{"unchanged", "membership", "revoked token", "expired cookie", "public expired cookie", "public privatized", "identity policy unavailable"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				ctx := systemTestContext()
				storage := store.NewFSStore(t.TempDir())
				identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
				const ownerToken = "dev:visible-admission-owner@example.test:Owner"
				owner, err := identity.Authenticate(ctx, ownerToken)
				if err != nil {
					t.Fatal(err)
				}
				reader, session, err := identity.Login(ctx, "dev:visible-admission-reader@example.test:Reader", "visible admission")
				if err != nil {
					t.Fatal(err)
				}
				record, err := identity.CreateRepository(ctx, owner, "visible-admission")
				if err != nil {
					t.Fatal(err)
				}
				if err := storage.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: reader.ID, Role: domain.RoleViewer}); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(change, "public ") {
					visibility := domain.VisibilityPublic
					if _, err := identity.UpdateRepositorySettings(ctx, owner.ID, record.ID, app.RepositoryPatch{Visibility: &visibility}); err != nil {
						t.Fatal(err)
					}
				}
				repo, hash := domain.HashContent([]byte(record.ID)), domain.HashContent([]byte("doc"))
				if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
					t.Fatal(err)
				}
				backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{Backend: app.NewService(storage, storage, nil, nil, storage)}}
				server := newDocumentViewTestServer(backend, identity)
				entered, done := make(chan struct{}), make(chan struct{})
				var current context.Context
				var authorizationErr error
				readCalls := 0
				backend.read = func(request context.Context) (domain.DocVisiblePage, error) {
					close(entered)
					admitted, release, err := app.AcquireResources(request, app.ResourceInteractive)
					if err != nil {
						return domain.DocVisiblePage{}, err
					}
					defer release()
					authorize := inbound.DocumentReadAuthorizer(admitted)
					if authorize == nil {
						return domain.DocVisiblePage{}, errors.New("missing document read authorizer")
					}
					current, authorizationErr = authorize(admitted, repo)
					if authorizationErr != nil {
						return domain.DocVisiblePage{}, authorizationErr
					}
					readCalls++
					return domain.DocVisiblePage{Projection: domain.DocVisibleProjection}, nil
				}
				var releases []func()
				for range 2 {
					_, release, err := app.AcquireResources(context.Background(), app.ResourceInteractive)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(release)
					releases = append(releases, release)
				}
				query := "projection=visible-v1&view=chat"
				if mode != "initial" {
					purpose := "units"
					if mode == "expand" {
						purpose = "fold"
					}
					query = "projection=visible-v1&" + mode + "=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, purpose))
				}
				parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				t.Cleanup(cancel)
				request := docVisibleTestRequest(repo, hash, query).WithContext(parent)
				if change != "public privatized" {
					request.Header.Set("Authorization", "Bearer "+session.Token)
					if strings.Contains(change, "cookie") {
						request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
						request.Header.Set("Authorization", "Bearer "+ownerToken)
					}
				}
				writer := &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}
				t.Cleanup(func() {
					for _, release := range releases {
						release()
					}
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("visible handler did not finish")
					}
				})
				go func() { defer close(done); server.Handler().ServeHTTP(writer, request) }()
				select {
				case <-entered:
				case <-done:
					t.Fatalf("initial guard blocked viewer: %d %s", writer.Code, writer.Body)
				case <-parent.Done():
					t.Fatal("viewer did not reach admission")
				}
				want, code := http.StatusOK, ""
				switch change {
				case "membership":
					err = identity.RemoveMember(ctx, owner.ID, record.ID, reader.ID)
					want, code = http.StatusForbidden, "forbidden"
				case "revoked token":
					err = identity.Logout(ctx, session.Token)
					want, code = http.StatusUnauthorized, "unauthenticated"
				case "expired cookie", "public expired cookie":
					var stored domain.Session
					stored, err = storage.GetSession(ctx, domain.HashToken(session.Token))
					if err == nil {
						stored.CreatedAt, stored.ExpiresAt = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
						err = storage.CreateSession(ctx, stored)
					}
					if change == "expired cookie" {
						want, code = http.StatusUnauthorized, "unauthenticated"
					}
				case "public privatized":
					visibility := domain.VisibilityPrivate
					_, err = identity.UpdateRepositorySettings(ctx, owner.ID, record.ID, app.RepositoryPatch{Visibility: &visibility})
					want, code = http.StatusUnauthorized, "unauthenticated"
				case "identity policy unavailable":
					server.id = policyDownIdentity{IdentityService: identity, user: reader}
					want, code = http.StatusNotFound, "not_found"
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, release := range releases {
					release()
				}
				select {
				case <-done:
				case <-parent.Done():
					t.Fatal("handler did not finish after admission")
				}
				if writer.Code != want || backend.calls != 1 || want != http.StatusOK && (!strings.Contains(writer.Body.String(), `"code":"`+code+`"`) || readCalls != 0) || want == http.StatusOK && readCalls != 1 {
					t.Fatalf("post-admission access: status=%d want=%d reads=%d body=%s", writer.Code, want, readCalls, writer.Body)
				}
				if current == nil {
					t.Fatal("authorizer was not invoked after admission")
				}
				deadline, present := current.Deadline()
				if expected, _ := parent.Deadline(); !present || !deadline.Equal(expected) || !writer.read.Equal(deadline) || !writer.write.Equal(deadline) {
					t.Fatal("authorization lost original deadline")
				}
				actor, system := inbound.RepositoryActor(current)
				user, authed := userFrom(current)
				anonymous := change == "revoked token" || strings.Contains(change, "cookie") || change == "public privatized"
				if system || anonymous && (actor != "" || authed) || !anonymous && (!authed || actor != reader.ID || user.ID != reader.ID) {
					t.Fatalf("stale authorization context retained: actor=%q system=%v user=%+v authed=%v", actor, system, user, authed)
				}
			})
		}
	}
}

type docVisiblePolicyBackend struct {
	Backend
	repo  domain.Repo
	err   error
	calls int
}

func (backend *docVisiblePolicyBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	backend.calls++
	return backend.repo, backend.err
}

type docVisiblePolicyIdentity struct {
	IdentityBackend
	repository    domain.Repository
	repositoryErr error
	user          domain.User
	credentialErr error
	role          domain.MemberRole
	breakGlass    bool
	auditErr      error
	auditCalls    int
	resolveCalls  int
	checkAudit    func(context.Context)
}

func (identity *docVisiblePolicyIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	identity.resolveCalls++
	return identity.user, identity.credentialErr
}

func (identity *docVisiblePolicyIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return identity.repository, identity.repositoryErr
}

func (identity *docVisiblePolicyIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return identity.role, identity.role != ""
}

func (identity *docVisiblePolicyIdentity) HasBreakGlassAccess(ctx context.Context, _, _ string) (bool, error) {
	identity.auditCalls++
	if identity.checkAudit != nil {
		identity.checkAudit(ctx)
	}
	return identity.breakGlass, identity.auditErr
}

func TestDocVisibleHTTPAuthorizerRefreshesEveryAdmissionAndRejectsForeignRepo(t *testing.T) {
	repo := domain.HashContent([]byte("repo"))
	backend := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
	identity := &docVisiblePolicyIdentity{repository: domain.Repository{ID: "repository"}, user: domain.User{ID: "current"}, role: domain.RoleViewer}
	server := configureDocumentViewTestServer(&Server{b: backend, id: identity})
	request := docVisibleTestRequest(repo, domain.HashContent([]byte("doc")), "projection=visible-v1&view=chat")
	request.Header.Set("Authorization", "Bearer session")
	authorize := server.docVisibleReadAuthorizer(request, repo)
	stale := inbound.WithSystemActor(context.WithValue(context.Background(), userCtxKey, domain.User{ID: "stale"}))
	for attempt := range 2 {
		current, err := authorize(stale, repo)
		if attempt == 0 {
			actor, system := inbound.RepositoryActor(current)
			user, authed := userFrom(current)
			if err != nil || system || actor != "current" || !authed || user.ID != "current" {
				t.Fatalf("credential not refreshed: actor=%q system=%v user=%+v err=%v", actor, system, user, err)
			}
			stale, identity.credentialErr = current, domain.ErrUnauthorized
		} else {
			actor, system := inbound.RepositoryActor(current)
			_, authed := userFrom(current)
			if code, status := mapError(err); status != 401 || code != "unauthenticated" || authed || system || actor != "" {
				t.Fatalf("second admission reused cached authority: status=%d code=%s actor=%q system=%v authed=%v", status, code, actor, system, authed)
			}
		}
	}
	if identity.resolveCalls != 2 || backend.calls != 2 {
		t.Fatal("authorization was cached", identity.resolveCalls, backend.calls)
	}
	_, err := authorize(stale, domain.HashContent([]byte("foreign repo")))
	if code, status := mapError(err); status != 403 || code != "forbidden" || identity.resolveCalls != 2 || backend.calls != 2 {
		t.Fatalf("foreign route reached policy lookup: status=%d code=%s resolve=%d repo=%d", status, code, identity.resolveCalls, backend.calls)
	}
	canceled, cancel := context.WithCancel(stale)
	cancel()
	if _, err := authorize(canceled, repo); !errors.Is(err, context.Canceled) || identity.resolveCalls != 2 || backend.calls != 2 {
		t.Fatal("canceled authorization performed policy IO", err)
	}
}

func TestDocVisibleHTTPPostAdmissionBreakGlass(t *testing.T) {
	for _, change := range []string{"unchanged", "revoked", "audit unavailable"} {
		t.Run(change, func(t *testing.T) {
			repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
			policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
			identity := &docVisiblePolicyIdentity{repository: domain.Repository{ID: "repository"}, user: domain.User{ID: "reader"}, breakGlass: true}
			reading, reads := false, 0
			identity.checkAudit = func(context.Context) {
				if reading {
					t.Error("write-backed break-glass audit ran inside read transaction")
				}
			}
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{Backend: policy}}
			backend.read = func(ctx context.Context) (domain.DocVisiblePage, error) {
				if identity.auditCalls != 1 {
					return domain.DocVisiblePage{}, errors.New("initial guard did not audit break-glass")
				}
				if change == "revoked" {
					identity.breakGlass = false
				}
				if change == "audit unavailable" {
					identity.auditErr = errors.New("private audit failure")
				}
				admitted, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
				if err != nil {
					return domain.DocVisiblePage{}, err
				}
				defer release()
				authorize := inbound.DocumentReadAuthorizer(admitted)
				if authorize == nil {
					return domain.DocVisiblePage{}, errors.New("missing authorizer")
				}
				if _, err = authorize(admitted, repo); err != nil {
					return domain.DocVisiblePage{}, err
				}
				reading = true
				reads++
				return domain.DocVisiblePage{}, nil
			}
			request := docVisibleTestRequest(repo, hash, "projection=visible-v1&view=chat")
			request.Header.Set("Authorization", "Bearer session")
			writer := httptest.NewRecorder()
			newDocumentViewTestServer(backend, identity).Handler().ServeHTTP(writer, request)
			want, code, message := 200, "", ""
			if change == "revoked" {
				want, code, message = 403, "forbidden", "insufficient role permissions — at least viewer required"
			}
			if change == "audit unavailable" {
				want, code, message = 503, "audit_unavailable", "break-glass audit unavailable"
			}
			if writer.Code != want || identity.auditCalls != 2 || identity.resolveCalls != 2 || want != 200 && reads != 0 || want == 200 && reads != 1 {
				t.Fatalf("break-glass changed: status=%d audits=%d resolves=%d reads=%d body=%s", writer.Code, identity.auditCalls, identity.resolveCalls, reads, writer.Body)
			}
			if want != 200 {
				assertDocVisibleAccessError(t, writer, want, code, message)
			}
		})
	}
}

func assertDocVisibleAccessError(t *testing.T, writer *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	var response struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if writer.Code != status || response.Error.Code != code || response.Error.Message != message {
		t.Fatalf("access envelope changed: status=%d want=%d code=%q want=%q message=%q want=%q", writer.Code, status, response.Error.Code, code, response.Error.Message, message)
	}
}

func TestRepositoryRolePolicyExtractionPreservesHTTPContract(t *testing.T) {
	for _, test := range []struct {
		name, code, message                                         string
		min, role                                                   domain.MemberRole
		public, publicPuller, archived, authed, unbound, breakGlass bool
		repoErr, identityErr, auditErr                              error
		status, audits                                              int
	}{
		{name: "unbound", min: domain.RoleViewer, unbound: true, status: 403, code: "repo_unbound", message: "repository is not bound to a repository — verify that <namespace>/<repository>/<repository> in the remote URL matches an existing repository, then reconnect with cxt setup <url>"},
		{name: "repository missing", min: domain.RoleViewer, repoErr: domain.ErrNotFound, status: 404, code: "not_found", message: domain.ErrNotFound.Error()},
		{name: "identity policy missing", min: domain.RoleViewer, identityErr: domain.ErrNotFound, status: 404, code: "not_found", message: domain.ErrNotFound.Error()},
		{name: "repository internal error", min: domain.RoleViewer, repoErr: errors.New("private lookup failure"), status: 500, code: "internal", message: "internal server error"},
		{name: "anonymous private", min: domain.RoleViewer, status: 401, code: "unauthenticated", message: "Login required — cxt login <token> (generate from Account Settings ⚙)"},
		{name: "viewer", min: domain.RoleViewer, role: domain.RoleViewer, authed: true, status: 200},
		{name: "archived viewer", min: domain.RoleViewer, role: domain.RoleViewer, authed: true, archived: true, status: 200},
		{name: "archived member", min: domain.RoleMember, role: domain.RoleOwner, authed: true, archived: true, status: 403, code: "forbidden", message: "archived repository (read-only) — owner can disable in settings"},
		{name: "anonymous public viewer", min: domain.RoleViewer, public: true, status: 200},
		{name: "anonymous archived public viewer", min: domain.RoleViewer, public: true, archived: true, status: 200},
		{name: "anonymous public puller", min: domain.RolePuller, public: true, publicPuller: true, status: 200},
		{name: "public viewer cannot pull", min: domain.RolePuller, public: true, status: 401, code: "unauthenticated", message: "Login required — cxt login <token> (generate from Account Settings ⚙)"},
		{name: "insufficient role", min: domain.RoleMember, role: domain.RoleViewer, authed: true, status: 403, code: "forbidden", message: "insufficient role permissions — at least member required"},
		{name: "break glass viewer", min: domain.RoleViewer, authed: true, breakGlass: true, status: 200, audits: 1},
		{name: "break glass cannot pull", min: domain.RolePuller, authed: true, breakGlass: true, status: 403, code: "forbidden", message: "insufficient role permissions — at least puller required"},
		{name: "missing break glass", min: domain.RoleViewer, authed: true, status: 403, code: "forbidden", message: "insufficient role permissions — at least viewer required", audits: 1},
		{name: "failed break glass audit", min: domain.RoleViewer, authed: true, auditErr: domain.ErrForbidden, status: 503, code: "audit_unavailable", message: "break-glass audit unavailable", audits: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := domain.HashContent([]byte("repo"))
			backend := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}, err: test.repoErr}
			if test.unbound {
				backend.repo.RepositoryID = ""
			}
			identity := &docVisiblePolicyIdentity{repository: domain.Repository{ID: "repository", Archived: test.archived}, repositoryErr: test.identityErr,
				role: test.role, breakGlass: test.breakGlass, auditErr: test.auditErr}
			if test.public {
				identity.repository.Visibility = domain.VisibilityPublic
			}
			if test.publicPuller {
				identity.repository.PublicRole = string(domain.RolePuller)
			}
			server := configureDocumentViewTestServer(&Server{b: backend, id: identity})
			request := docVisibleTestRequest(repo, domain.HashContent([]byte("doc")), "")
			if test.authed {
				request = request.WithContext(context.WithValue(request.Context(), userCtxKey, domain.User{ID: "reader"}))
			}
			writer := httptest.NewRecorder()
			allowed := server.requireRepoRole(writer, request, test.min)
			if allowed != (test.status == http.StatusOK) || identity.auditCalls != test.audits {
				t.Fatalf("policy changed: allowed=%v audits=%d body=%s", allowed, identity.auditCalls, writer.Body)
			}
			if allowed {
				return
			}
			assertDocVisibleAccessError(t, writer, test.status, test.code, test.message)
			err := server.checkRepoRole(request.Context(), repo, test.min)
			var access *httpAccessError
			if !errors.As(err, &access) {
				t.Fatalf("policy failure is not a typed HTTP error: %v", err)
			}
			if code, status := mapError(fmt.Errorf("document read: %w", err)); status != test.status || code != test.code {
				t.Fatalf("wrapped access error lost original status/code: %d %s", status, code)
			}
		})
	}
}
