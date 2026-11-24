package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// readAuthBackend answers which repository a document store is bound to.
type readAuthBackend struct {
	Backend
	bound map[domain.ContentHash]string
	err   error
	reads int
}

func (backend *readAuthBackend) GetRepo(_ context.Context, id domain.ContentHash) (domain.Repo, error) {
	backend.reads++
	if backend.err != nil {
		return domain.Repo{}, backend.err
	}
	repository, ok := backend.bound[id]
	if !ok {
		return domain.Repo{}, domain.ErrNotFound
	}
	return domain.Repo{ID: id, RepositoryID: repository}, nil
}

// readAuthIdentity is an identity service that knows logins, one repository
// and its members. It cannot admit a read for a snapshot.
type readAuthIdentity struct {
	IdentityBackend
	logins        map[string]domain.User
	visibility    domain.Visibility
	roles         map[string]domain.MemberRole
	breakGlass    bool
	breakGlassErr error
	repositories  []string
}

func (identity *readAuthIdentity) ResolveUser(_ context.Context, token string) (domain.User, error) {
	user, ok := identity.logins[token]
	if !ok {
		return domain.User{}, domain.ErrUnauthorized
	}
	return user, nil
}

func (identity *readAuthIdentity) GetRepository(_ context.Context, id string) (domain.Repository, error) {
	identity.repositories = append(identity.repositories, id)
	return domain.Repository{ID: id, Visibility: identity.visibility}, nil
}

func (identity *readAuthIdentity) RoleOf(_ context.Context, _ string, user string) (domain.MemberRole, bool) {
	role, ok := identity.roles[user]
	return role, ok
}

func (identity *readAuthIdentity) HasBreakGlassAccess(context.Context, string, string) (bool, error) {
	return identity.breakGlass, identity.breakGlassErr
}

// readAuthPermit records what a snapshot was authorized for.
type readAuthPermit struct {
	asked  [][2]string
	answer error
}

func (permit *readAuthPermit) AuthorizeSnapshot(_ context.Context, repository, user string) error {
	permit.asked = append(permit.asked, [2]string{repository, user})
	return permit.answer
}

// readAuthSnapshotIdentity can also admit a read and answer again who the
// caller is at the moment the store's snapshot is taken.
type readAuthSnapshotIdentity struct {
	*readAuthIdentity
	admitted    [][2]string
	admitErr    error
	permit      *readAuthPermit
	snapshot    map[string]domain.User
	snapshotErr error
	resolved    int
}

func (identity *readAuthSnapshotIdentity) AdmitRepositoryRead(_ context.Context, repository, user string) (inbound.RepositoryReadPermit, error) {
	identity.admitted = append(identity.admitted, [2]string{repository, user})
	if identity.admitErr != nil {
		return nil, identity.admitErr
	}
	return identity.permit, nil
}

func (identity *readAuthSnapshotIdentity) ResolveUserForSnapshot(_ context.Context, token string) (domain.User, error) {
	identity.resolved++
	if identity.snapshotErr != nil {
		return domain.User{}, identity.snapshotErr
	}
	user, ok := identity.snapshot[token]
	if !ok {
		return domain.User{}, domain.ErrUnauthorized
	}
	return user, nil
}

func (identity *readAuthSnapshotIdentity) ResolveMCPUserForSnapshot(context.Context, string, ...string) (domain.User, error) {
	return domain.User{}, domain.ErrUnauthorized
}

const (
	readAuthStore      = domain.ContentHash("1111111111111111111111111111111111111111111111111111111111111111")
	readAuthOtherStore = domain.ContentHash("2222222222222222222222222222222222222222222222222222222222222222")
	readAuthUnbound    = domain.ContentHash("3333333333333333333333333333333333333333333333333333333333333333")
	// readAuthSibling is a second store of the repository readAuthStore belongs to.
	readAuthSibling = domain.ContentHash("5555555555555555555555555555555555555555555555555555555555555555")
)

func readAuthRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

// readAuthStale is a context that already carries somebody: an authorizer
// must not let that somebody through.
func readAuthStale() context.Context {
	stale := domain.User{ID: "stale"}
	return inbound.WithRepositoryActor(context.WithValue(context.Background(), userCtxKey, stale), stale.ID)
}

func readAuthActsAs(t *testing.T, name string, ctx context.Context, want string) {
	t.Helper()
	user, signedIn := userFrom(ctx)
	actor, system := inbound.RepositoryActor(ctx)
	if system || actor != want || signedIn != (want != "") || user.ID != want {
		t.Errorf("%s: the read goes on as user %q (signed in %t), actor %q, system %t; want %q", name, user.ID, signedIn, actor, system, want)
	}
}

func readAuthSnapshotFixture() (*Server, *readAuthBackend, *readAuthSnapshotIdentity) {
	backend := &readAuthBackend{bound: map[domain.ContentHash]string{readAuthStore: "repository-1", readAuthSibling: "repository-1", readAuthOtherStore: "repository-2", readAuthUnbound: ""}}
	alice := domain.User{ID: "alice"}
	identity := &readAuthSnapshotIdentity{
		readAuthIdentity: &readAuthIdentity{logins: map[string]domain.User{"alice-token": alice}},
		permit:           &readAuthPermit{},
		snapshot:         map[string]domain.User{"alice-token": alice},
	}
	server := NewServer(backend, identity)
	server.documentQueryProfile = inbound.DocumentQueryVerifiedRoots
	return server, backend, identity
}

// On a server whose stores take snapshots, a read is admitted once before any
// work, for the repository the request named and the user the request's login
// resolves to.
func TestSnapshotReadIsAdmittedForTheRequestsRepositoryAndUser(t *testing.T) {
	refusal := errors.New("synthetic: the identity service refused the read")
	lost := errors.New("synthetic: the store cannot say which repository this is")
	cancelled, cancel := context.WithCancel(readAuthStale())
	cancel()
	for _, s := range []struct {
		name     string
		token    string
		store    domain.ContentHash
		ask      domain.ContentHash
		ctx      context.Context
		admitErr error
		storeErr error
		want     error
		admitted [][2]string
		reads    int
		actsAs   string
	}{
		{name: "nobody signed in", store: readAuthStore, ask: readAuthStore, admitted: [][2]string{{"repository-1", ""}}, reads: 1},
		{name: "a signed-in user", token: "alice-token", store: readAuthStore, ask: readAuthStore, admitted: [][2]string{{"repository-1", "alice"}}, reads: 1, actsAs: "alice"},
		{name: "a login that is no longer good reads as nobody", token: "expired", store: readAuthStore, ask: readAuthStore, admitted: [][2]string{{"repository-1", ""}}, reads: 1},
		{name: "another store than the request named", token: "alice-token", store: readAuthStore, ask: readAuthOtherStore, want: domain.ErrForbidden},
		{name: "another store of the same repository", token: "alice-token", store: readAuthStore, ask: readAuthSibling, want: domain.ErrForbidden},
		{name: "a request that was given up", token: "alice-token", store: readAuthStore, ask: readAuthStore, ctx: cancelled, want: context.Canceled},
		{name: "a store that is not there", token: "alice-token", store: "4444444444444444444444444444444444444444444444444444444444444444", ask: "4444444444444444444444444444444444444444444444444444444444444444", want: domain.ErrNotFound, reads: 1},
		{name: "a store that cannot be read", token: "alice-token", store: readAuthStore, ask: readAuthStore, storeErr: lost, want: lost, reads: 1},
		{name: "a store that belongs to no repository", token: "alice-token", store: readAuthUnbound, ask: readAuthUnbound, want: domain.ErrForbidden, reads: 1},
		{name: "the identity service refuses", token: "alice-token", store: readAuthStore, ask: readAuthStore, admitErr: refusal, want: refusal, admitted: [][2]string{{"repository-1", "alice"}}, reads: 1},
	} {
		server, backend, identity := readAuthSnapshotFixture()
		identity.admitErr, backend.err = s.admitErr, s.storeErr
		ctx := s.ctx
		if ctx == nil {
			ctx = readAuthStale()
		}
		admitted, err := server.documentReadAuthorizer(readAuthRequest(s.token), s.store)(ctx, s.ask)
		if s.want == nil && err != nil || s.want != nil && !errors.Is(err, s.want) {
			t.Errorf("%s: %v", s.name, err)
		}
		if len(identity.admitted) != len(s.admitted) || len(s.admitted) == 1 && identity.admitted[0] != s.admitted[0] {
			t.Errorf("%s: admission was asked for %v, want %v", s.name, identity.admitted, s.admitted)
		}
		if backend.reads != s.reads {
			t.Errorf("%s: the store was asked for its repository %d times, want %d", s.name, backend.reads, s.reads)
		}
		// Whoever the context carried before is not who the read goes on as.
		readAuthActsAs(t, s.name, admitted, s.actsAs)
		// Only an admitted read is given the second check.
		if (inbound.DocumentSnapshotReadAuthorizer(admitted) != nil) != (s.want == nil) {
			t.Errorf("%s: a snapshot check is attached: %t", s.name, inbound.DocumentSnapshotReadAuthorizer(admitted) != nil)
		}
		if len(identity.permit.asked) != 0 || identity.resolved != 0 || len(identity.repositories) != 0 {
			t.Errorf("%s: admission did the snapshot's work: permit %v, logins resolved for a snapshot %d, role lookups %v", s.name, identity.permit.asked, identity.resolved, identity.repositories)
		}
	}
}

// When the store takes its snapshot, the admitted read is checked again
// against what is true inside that snapshot: the same repository, the same
// user or nobody, and the permit the read was admitted with.
func TestSnapshotReadIsCheckedAgainInsideTheSnapshot(t *testing.T) {
	refusal := errors.New("synthetic: the permit no longer holds")
	down := errors.New("synthetic: the logins cannot be read")
	lost := errors.New("synthetic: the store cannot say which repository this is")
	for _, s := range []struct {
		name     string
		token    string
		ask      domain.ContentHash
		change   func(*readAuthBackend, *readAuthSnapshotIdentity)
		want     error
		asked    [][2]string
		resolved int
		actsAs   string
	}{
		{name: "nobody signed in", ask: readAuthStore, asked: [][2]string{{"repository-1", ""}}},
		{name: "the same user", token: "alice-token", ask: readAuthStore, asked: [][2]string{{"repository-1", "alice"}}, resolved: 1, actsAs: "alice"},
		{name: "another store than the request named", token: "alice-token", ask: readAuthOtherStore, want: domain.ErrForbidden},
		{name: "another store of the same repository", token: "alice-token", ask: readAuthSibling, want: domain.ErrForbidden},
		{name: "the login ended in between: the read goes on as nobody", token: "alice-token", ask: readAuthStore,
			change: func(_ *readAuthBackend, identity *readAuthSnapshotIdentity) { identity.snapshot = nil },
			asked:  [][2]string{{"repository-1", ""}}, resolved: 1},
		{name: "the login now belongs to somebody else", token: "alice-token", ask: readAuthStore,
			change: func(_ *readAuthBackend, identity *readAuthSnapshotIdentity) {
				identity.snapshot = map[string]domain.User{"alice-token": {ID: "mallory"}}
			},
			want: domain.ErrUnauthorized, resolved: 1},
		{name: "the logins cannot be read", token: "alice-token", ask: readAuthStore,
			change: func(_ *readAuthBackend, identity *readAuthSnapshotIdentity) { identity.snapshotErr = down },
			want:   down, resolved: 1},
		{name: "the store was bound to another repository in between", token: "alice-token", ask: readAuthStore,
			change: func(backend *readAuthBackend, _ *readAuthSnapshotIdentity) {
				backend.bound[readAuthStore] = "repository-2"
			},
			want: domain.ErrForbidden, resolved: 1},
		{name: "the store cannot be read any more", token: "alice-token", ask: readAuthStore,
			change: func(backend *readAuthBackend, _ *readAuthSnapshotIdentity) { backend.err = lost },
			want:   lost, resolved: 1},
		{name: "the permit refuses", token: "alice-token", ask: readAuthStore,
			change: func(_ *readAuthBackend, identity *readAuthSnapshotIdentity) { identity.permit.answer = refusal },
			want:   refusal, asked: [][2]string{{"repository-1", "alice"}}, resolved: 1},
	} {
		server, backend, identity := readAuthSnapshotFixture()
		admitted, err := server.documentReadAuthorizer(readAuthRequest(s.token), readAuthStore)(context.Background(), readAuthStore)
		if err != nil {
			t.Fatalf("%s: admission: %v", s.name, err)
		}
		if s.change != nil {
			s.change(backend, identity)
		}
		bound, err := inbound.DocumentSnapshotReadAuthorizer(admitted)(readAuthStale(), s.ask)
		if s.want == nil && err != nil || s.want != nil && !errors.Is(err, s.want) {
			t.Errorf("%s: %v", s.name, err)
		}
		if len(identity.permit.asked) != len(s.asked) || len(s.asked) == 1 && identity.permit.asked[0] != s.asked[0] {
			t.Errorf("%s: the permit was asked for %v, want %v", s.name, identity.permit.asked, s.asked)
		}
		if identity.resolved != s.resolved {
			t.Errorf("%s: the login was resolved for the snapshot %d times, want %d", s.name, identity.resolved, s.resolved)
		}
		if len(identity.admitted) != 1 {
			t.Errorf("%s: the read was admitted %d times", s.name, len(identity.admitted))
		}
		readAuthActsAs(t, s.name, bound, s.actsAs)
	}
}

// A server whose stores take no snapshots, and a server whose identity service
// cannot admit a read for one, decide by the caller's role in the repository.
func TestReadWithoutSnapshotsIsDecidedByTheCallersRole(t *testing.T) {
	alice := domain.User{ID: "alice"}
	audit := errors.New("synthetic: the break-glass audit cannot be written")
	cancelled, cancel := context.WithCancel(readAuthStale())
	cancel()
	for _, s := range []struct {
		name       string
		token      string
		store      domain.ContentHash
		ask        domain.ContentHash
		ctx        context.Context
		visibility domain.Visibility
		role       domain.MemberRole
		breakGlass bool
		auditErr   error
		want       error
		code       string
		status     int
		lookups    int
		actsAs     string
	}{
		{name: "anyone reads a public repository", store: readAuthStore, ask: readAuthStore, visibility: domain.VisibilityPublic, lookups: 1},
		{name: "nobody signed in and a private repository", store: readAuthStore, ask: readAuthStore, want: domain.ErrUnauthorized, code: "unauthenticated", status: http.StatusUnauthorized, lookups: 1},
		{name: "a login that is no longer good and a private repository", token: "expired", store: readAuthStore, ask: readAuthStore, want: domain.ErrUnauthorized, code: "unauthenticated", status: http.StatusUnauthorized, lookups: 1},
		{name: "a viewer of a private repository", token: "alice-token", store: readAuthStore, ask: readAuthStore, role: domain.RoleViewer, lookups: 1, actsAs: "alice"},
		{name: "a signed-in user who is not in the repository", token: "alice-token", store: readAuthStore, ask: readAuthStore, want: domain.ErrForbidden, code: "forbidden", status: http.StatusForbidden, lookups: 1, actsAs: "alice"},
		{name: "a signed-in user with break-glass access", token: "alice-token", store: readAuthStore, ask: readAuthStore, breakGlass: true, lookups: 1, actsAs: "alice"},
		{name: "break-glass access that cannot be audited", token: "alice-token", store: readAuthStore, ask: readAuthStore, auditErr: audit, want: audit, code: "audit_unavailable", status: http.StatusServiceUnavailable, lookups: 1, actsAs: "alice"},
		{name: "another store than the request named", token: "alice-token", store: readAuthStore, ask: readAuthOtherStore, visibility: domain.VisibilityPublic, want: domain.ErrForbidden, code: "forbidden", status: http.StatusForbidden},
		{name: "a request that was given up", token: "alice-token", store: readAuthStore, ask: readAuthStore, ctx: cancelled, visibility: domain.VisibilityPublic, want: context.Canceled},
		{name: "a store that belongs to no repository", token: "alice-token", store: readAuthUnbound, ask: readAuthUnbound, visibility: domain.VisibilityPublic, want: domain.ErrForbidden, code: "repo_unbound", status: http.StatusForbidden, actsAs: "alice"},
	} {
		for kind, snapshots := range map[string]bool{"stores without snapshots": false, "an identity service that cannot admit": true} {
			name := s.name + " (" + kind + ")"
			backend := &readAuthBackend{bound: map[domain.ContentHash]string{readAuthStore: "repository-1", readAuthSibling: "repository-1", readAuthOtherStore: "repository-2", readAuthUnbound: ""}}
			plain := &readAuthIdentity{logins: map[string]domain.User{"alice-token": alice}, visibility: s.visibility, roles: map[string]domain.MemberRole{}, breakGlass: s.breakGlass, breakGlassErr: s.auditErr}
			if s.role != "" {
				plain.roles[alice.ID] = s.role
			}
			admitting := &readAuthSnapshotIdentity{readAuthIdentity: plain, permit: &readAuthPermit{}}
			var server *Server
			if snapshots {
				// The stores take snapshots, but this identity service cannot admit for one.
				server = NewServer(backend, plain)
				server.documentQueryProfile = inbound.DocumentQueryVerifiedRoots
			} else {
				// This identity service could admit, but the stores take no snapshots.
				server = NewServer(backend, admitting)
				server.documentQueryProfile = inbound.DocumentQueryCompatibility
			}
			ctx := s.ctx
			if ctx == nil {
				ctx = readAuthStale()
			}
			decided, err := server.documentReadAuthorizer(readAuthRequest(s.token), s.store)(ctx, s.ask)
			if s.want == nil && err != nil || s.want != nil && !errors.Is(err, s.want) {
				t.Errorf("%s: %v", name, err)
			}
			if s.code != "" {
				var refused *httpAccessError
				if !errors.As(err, &refused) || refused.code != s.code || refused.status != s.status {
					t.Errorf("%s: refused as %+v, want %s %d", name, refused, s.code, s.status)
				}
			}
			if len(plain.repositories) != s.lookups || s.lookups == 1 && plain.repositories[0] != "repository-1" {
				t.Errorf("%s: roles were looked up in %v", name, plain.repositories)
			}
			if len(admitting.admitted) != 0 || len(admitting.permit.asked) != 0 || inbound.DocumentSnapshotReadAuthorizer(decided) != nil {
				t.Errorf("%s: the read was admitted for a snapshot nobody takes: %v", name, admitting.admitted)
			}
			readAuthActsAs(t, name, decided, s.actsAs)
		}
	}
}
