//go:build postgres

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var errMemorySnapshotPGRead = errors.New("memory snapshot authorization read intercepted")

type memorySnapshotPGContextKey struct{}

type memorySnapshotPGBackend struct {
	*app.Service
	snapshots  inbound.MemorySnapshotQueries
	test       *testing.T
	storage    *store.PostgresStore
	inspect    func(context.Context, domain.ContentHash, inbound.DocumentReadAuthorization, func(context.Context) error) error
	admissions int
	lookups    []context.Context
	reads      []context.Context
}

func (backend *memorySnapshotPGBackend) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if inbound.DocumentReadAuthorizer(ctx) == nil || backend.storage.InTransaction(ctx) {
		backend.test.Fatal("protocol memory_load did not enter admission outside a transaction with its authorizer")
	}
	return backend.snapshots.WithMemoryReadResources(ctx, repo, func(admitted context.Context) error {
		backend.admissions++
		authorize := inbound.DocumentSnapshotReadAuthorizer(admitted)
		if authorize == nil || backend.storage.InTransaction(admitted) {
			backend.test.Fatal("actual IdentityService did not install a pre-snapshot authorization callback")
		}
		return backend.inspect(admitted, repo, authorize, read)
	})
}

func (backend *memorySnapshotPGBackend) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	backend.lookups = append(backend.lookups, ctx)
	return backend.Service.GetRepo(ctx, repo)
}

func (backend *memorySnapshotPGBackend) GetSnapshot(ctx context.Context, repo, snapshot domain.ContentHash) (domain.Snapshot, error) {
	backend.reads = append(backend.reads, ctx)
	return domain.Snapshot{}, errMemorySnapshotPGRead
}

type memorySnapshotPGFixture struct {
	ctx        context.Context
	storage    *store.PostgresStore
	peer       *store.PostgresStore
	sql        *pgx.Conn
	identity   *app.IdentityService
	revoker    *app.IdentityService
	backend    *memorySnapshotPGBackend
	server     *Server
	owner      domain.User
	reader     domain.User
	repository domain.Repository
	repo       domain.Repo
	pair       domain.OAuthTokenPair
}

func newMemorySnapshotPGFixture(test *testing.T) *memorySnapshotPGFixture {
	test.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	test.Cleanup(cancel)
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = connection.Close(ctx) })
	identity := app.NewIdentityService(nil, storage)
	suffix := fmt.Sprintf("msa-%d", time.Now().UnixNano())
	owner := domain.User{ID: suffix + "-owner", Username: suffix + "-owner", Email: suffix + "-owner@example.test", Name: "Owner"}
	reader := domain.User{ID: suffix + "-reader", Username: suffix + "-reader", Email: suffix + "-reader@example.test", Name: "Reader"}
	for _, user := range []domain.User{owner, reader} {
		if err := storage.UpsertUser(ctx, user); err != nil {
			test.Fatal(err)
		}
	}
	system := inbound.WithSystemActor(ctx)
	repository, err := identity.CreateRepository(system, owner, "Memory-snapshot")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(system, domain.Membership{RepositoryID: repository.ID, UserID: reader.ID, Role: domain.RoleViewer}); err != nil {
		test.Fatal(err)
	}
	repo, err := storage.PutRepo(system, domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main"})
	if err != nil {
		test.Fatal(err)
	}
	backend := &memorySnapshotPGBackend{Service: app.NewService(storage, storage, nil, nil, storage), test: test, storage: storage}
	backend.snapshots = memorySnapshotQueriesForTest(test, backend.Service)
	server, err := NewServer(backend, identity, storage, "https://memory-snapshot.example.test")
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetMemorySnapshotQueries(memorySnapshotScopedTestReader{backend.snapshots, backend}); err != nil {
		test.Fatal(err)
	}
	pair, err := identity.IssueMCPTokenPair(system, reader.ID, "memory-snapshot-client", server.resource)
	if err != nil {
		test.Fatal(err)
	}
	return &memorySnapshotPGFixture{ctx: ctx, storage: storage, peer: peer, sql: connection,
		identity: identity, revoker: app.NewIdentityService(nil, peer), backend: backend, server: server,
		owner: owner, reader: reader, repository: repository, repo: repo, pair: pair}
}

func (fixture *memorySnapshotPGFixture) request(test *testing.T, arguments toolArgs) *http.Request {
	test.Helper()
	arguments.Repository = string(fixture.repo.ID)
	params, err := json.Marshal(map[string]any{"name": "memory_load", "arguments": arguments})
	if err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
	if err != nil {
		test.Fatal(err)
	}
	ctx := context.WithValue(fixture.ctx, memorySnapshotPGContextKey{}, test.Name())
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))).WithContext(ctx)
	for key, value := range remoteMCPHeaders(fixture.pair.AccessToken) {
		request.Header.Set(key, value)
	}
	return request
}

func (fixture *memorySnapshotPGFixture) call(test *testing.T, expected error, reads int) {
	test.Helper()
	request := fixture.request(test, toolArgs{Mode: "stored", Ref: string(domain.HashContent([]byte("intercepted snapshot")))})
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, request)
	text, failed := memoryAdmissionResult(test, response)
	if !failed || text != "Error: "+expected.Error() || fixture.backend.admissions != 1 || len(fixture.backend.reads) != reads {
		test.Fatalf("protocol result=%q failed=%v admissions=%d reads=%d; want %v and %d reads",
			text, failed, fixture.backend.admissions, len(fixture.backend.reads), expected, reads)
	}
}

func (fixture *memorySnapshotPGFixture) update(test *testing.T, query string, arguments ...any) {
	test.Helper()
	result, err := fixture.sql.Exec(fixture.ctx, query, arguments...)
	if err != nil || result.RowsAffected() != 1 {
		test.Fatalf("fixture mutation affected %d rows: %v", result.RowsAffected(), err)
	}
}

func (fixture *memorySnapshotPGFixture) state(test *testing.T) string {
	test.Helper()
	var state string
	err := fixture.sql.QueryRow(fixture.ctx, `
SELECT COALESCE(jsonb_agg(jsonb_build_array(source, version, record) ORDER BY source, record::text), '[]'::jsonb)::text
FROM (
 SELECT 'users' AS source, xmin::text AS version, to_jsonb(users) AS record FROM users WHERE id=ANY($1::text[])
 UNION ALL SELECT 'sessions', xmin::text, to_jsonb(sessions) FROM sessions WHERE user_id=ANY($1::text[])
 UNION ALL SELECT 'oauth_grants', xmin::text, to_jsonb(oauth_grants) FROM oauth_grants WHERE user_id=ANY($1::text[])
 UNION ALL SELECT 'oauth_refresh_references', xmin::text, to_jsonb(oauth_refresh_references) FROM oauth_refresh_references
 WHERE grant_id IN (SELECT id FROM oauth_grants WHERE user_id=ANY($1::text[]))
 UNION ALL SELECT 'account_audit', xmin::text, to_jsonb(account_audit) FROM account_audit WHERE user_id=ANY($1::text[])
 UNION ALL SELECT 'memberships', xmin::text, to_jsonb(memberships) FROM memberships WHERE user_id=ANY($1::text[])
 UNION ALL SELECT 'repositories', xmin::text, to_jsonb(repositories) FROM repositories WHERE owner_id=$2
 UNION ALL SELECT 'repos', xmin::text, to_jsonb(repos) FROM repos WHERE id=$3
) AS state`, []string{fixture.owner.ID, fixture.reader.ID}, fixture.owner.ID, string(fixture.repo.ID)).Scan(&state)
	if err != nil {
		test.Fatal(err)
	}
	return state
}

func (fixture *memorySnapshotPGFixture) assertBound(test *testing.T, transaction, authorized context.Context) {
	test.Helper()
	actor, system := inbound.RepositoryActor(authorized)
	deadline, hasDeadline := transaction.Deadline()
	boundDeadline, boundHasDeadline := authorized.Deadline()
	if actor != fixture.reader.ID || system || !fixture.storage.InReadOnlyTransaction(authorized) || fixture.peer.InReadOnlyTransaction(authorized) ||
		authorized.Value(memorySnapshotPGContextKey{}) != test.Name() || authorized.Done() != transaction.Done() ||
		hasDeadline != boundHasDeadline || !deadline.Equal(boundDeadline) {
		test.Fatal("snapshot authorization lost the current actor, owned read-only transaction, or request context")
	}
	if len(fixture.backend.lookups) != 1 {
		test.Fatalf("snapshot must re-read the repo exactly once: %d", len(fixture.backend.lookups))
	}
	lookup := fixture.backend.lookups[0]
	lookupActor, lookupSystem := inbound.RepositoryActor(lookup)
	if lookupActor != "" || lookupSystem || !fixture.storage.InReadOnlyTransaction(lookup) || inbound.DocumentReadAuthorizer(lookup) != nil {
		test.Fatal("snapshot repo lookup reused stale authority or recursively invoked admission")
	}
	if err := fixture.storage.WithinReadSnapshot(authorized, func(nested context.Context) error {
		if nested != authorized {
			test.Fatal("authorized context opened a different read snapshot")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGMCPMemorySnapshotAuthorizationWiring(test *testing.T) {
	fixture := newMemorySnapshotPGFixture(test)
	fixture.backend.inspect = func(admitted context.Context, repo domain.ContentHash, authorize inbound.DocumentReadAuthorization, read func(context.Context) error) error {
		fixture.update(test, `UPDATE users SET name='Current reader' WHERE id=$1`, fixture.reader.ID)
		fixture.update(test, `UPDATE repos SET default_branch='current-branch' WHERE id=$1`, string(repo))
		var readErr error
		var before string
		err := fixture.storage.WithinReadSnapshot(inbound.WithSystemActor(admitted), func(transaction context.Context) error {
			current, err := fixture.storage.GetUser(transaction, fixture.reader.ID)
			if err != nil || current.Name != "Current reader" {
				test.Fatalf("pin current user in snapshot: %+v %v", current, err)
			}
			fixture.update(test, `UPDATE users SET name='Outside snapshot' WHERE id=$1`, fixture.reader.ID)
			fixture.update(test, `UPDATE repos SET default_branch='outside-snapshot' WHERE id=$1`, string(repo))
			before = fixture.state(test)
			authorized, err := authorize(transaction, repo)
			if err != nil {
				test.Fatalf("protocol snapshot authorization: %v", err)
			}
			fixture.assertBound(test, transaction, authorized)
			user, err := fixture.identity.ResolveMCPUserForSnapshot(authorized, fixture.pair.AccessToken, fixture.server.resource)
			if err != nil || user.ID != current.ID || user.Name != current.Name {
				test.Fatalf("authorized identity escaped the pinned snapshot: %+v %v", user, err)
			}
			boundRepo, ok := authorized.Value(memoryRepositoryKey{}).(domain.Repo)
			if !ok || boundRepo.ID != repo || boundRepo.RepositoryID != fixture.repository.ID || boundRepo.DefaultBranch != "current-branch" {
				test.Fatalf("callback retained admission metadata or escaped the snapshot: %+v", boundRepo)
			}
			readErr = fixture.backend.Service.WithMemoryReadResources(authorized, repo, func(nested context.Context) error {
				if nested != authorized || inbound.DocumentSnapshotReadAuthorizer(nested) == nil {
					test.Fatal("nested same-repository memory scope lost its snapshot context or callback")
				}
				return read(nested)
			})
			if !errors.Is(readErr, errMemorySnapshotPGRead) || len(fixture.backend.reads) != 1 || fixture.backend.reads[0] != authorized {
				test.Fatalf("memory page did not reach the intercepted read with its authorized context: %v", readErr)
			}
			return nil
		})
		if err != nil {
			test.Fatalf("read-only authorization transaction could not commit: %v", err)
		}
		if fixture.state(test) != before {
			test.Fatal("snapshot authorization wrote identity, session, grant, audit, membership, or repository state")
		}
		return readErr
	}
	fixture.call(test, errMemorySnapshotPGRead, 1)
}

func TestPGMCPMemorySnapshotAuthorizationLegacySessionIsReadOnly(test *testing.T) {
	fixture := newMemorySnapshotPGFixture(test)
	fixture.backend.inspect = func(admitted context.Context, repo domain.ContentHash, authorize inbound.DocumentReadAuthorization, read func(context.Context) error) error {
		fixture.update(test, `UPDATE sessions SET token=$2 WHERE token=$1`, domain.HashToken(fixture.pair.AccessToken), fixture.pair.AccessToken)
		before := fixture.state(test)
		var readErr error
		err := fixture.storage.WithinReadSnapshot(admitted, func(transaction context.Context) error {
			authorized, err := authorize(transaction, repo)
			if err != nil {
				test.Fatalf("read-only legacy MCP session lookup: %v", err)
			}
			fixture.assertBound(test, transaction, authorized)
			readErr = read(authorized)
			if !errors.Is(readErr, errMemorySnapshotPGRead) {
				test.Fatalf("legacy session did not reach memory read: %v", readErr)
			}
			return nil
		})
		if err != nil {
			test.Fatalf("legacy lookup attempted a write in its read-only transaction: %v", err)
		}
		if fixture.state(test) != before {
			test.Fatal("snapshot authentication promoted or changed a legacy session")
		}
		return readErr
	}
	fixture.call(test, errMemorySnapshotPGRead, 1)
}

func TestPGMCPMemorySnapshotAuthorizationRejectsChangedAuthority(test *testing.T) {
	for _, change := range []string{"session_deleted", "session_expired", "grant_revoked", "membership_revoked", "wrong_audience", "different_identity", "repository_remapped", "wrong_repository", "no_snapshot", "foreign_snapshot", "write_transaction"} {
		test.Run(change, func(test *testing.T) {
			fixture := newMemorySnapshotPGFixture(test)
			var expected error = domain.ErrUnauthorized
			if change == "membership_revoked" || change == "repository_remapped" || change == "wrong_repository" {
				expected = domain.ErrForbidden
			} else if change == "no_snapshot" || change == "foreign_snapshot" || change == "write_transaction" {
				expected = domain.ErrConflict
			}
			fixture.backend.inspect = func(admitted context.Context, repo domain.ContentHash, authorize inbound.DocumentReadAuthorization, read func(context.Context) error) error {
				system := inbound.WithSystemActor(fixture.ctx)
				requested := repo
				var err error
				switch change {
				case "session_deleted":
					err = fixture.revoker.Logout(system, fixture.pair.AccessToken)
				case "session_expired":
					fixture.update(test, `UPDATE sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE token=$1`, domain.HashToken(fixture.pair.AccessToken))
				case "grant_revoked":
					err = fixture.revoker.RevokeMCPToken(system, fixture.pair.RefreshToken, "memory-snapshot-client")
					if _, sessionErr := fixture.peer.GetSession(system, domain.HashToken(fixture.pair.AccessToken)); sessionErr != nil {
						test.Fatalf("grant revocation must leave the access session to exercise the grant check: %v", sessionErr)
					}
				case "membership_revoked":
					err = fixture.revoker.RemoveMember(system, fixture.owner.ID, fixture.repository.ID, fixture.reader.ID)
				case "wrong_audience", "different_identity":
					user, resource := fixture.reader.ID, "https://other-memory.example.test/mcp"
					if change == "different_identity" {
						user, resource = fixture.owner.ID, fixture.server.resource
					}
					pair, pairErr := fixture.revoker.IssueMCPTokenPair(system, user, "memory-snapshot-client", resource)
					if pairErr != nil {
						test.Fatal(pairErr)
					}
					fixture.update(test, `UPDATE sessions SET user_id=$2, grant_id=$3 WHERE token=$1`, domain.HashToken(fixture.pair.AccessToken), user, pair.GrantID)
				case "repository_remapped":
					other, createErr := fixture.revoker.CreateRepository(system, fixture.owner, "Other-memory")
					if createErr != nil {
						test.Fatal(createErr)
					}
					if err := fixture.peer.AddMember(system, domain.Membership{RepositoryID: other.ID, UserID: fixture.reader.ID, Role: domain.RoleViewer}); err != nil {
						test.Fatal(err)
					}
					fixture.update(test, `UPDATE repos SET repository_id=$2 WHERE id=$1`, string(repo), other.ID)
				case "wrong_repository":
					requested = domain.HashContent([]byte("different memory repository"))
				}
				if err != nil {
					test.Fatal(err)
				}
				before := fixture.state(test)
				var denied error
				check := func(transaction context.Context) error {
					cleared, authErr := authorize(inbound.WithSystemActor(transaction), requested)
					denied = authErr
					if !errors.Is(denied, expected) {
						test.Fatalf("snapshot callback accepted changed authority: got %v, want %v", denied, expected)
					}
					actor, privileged := inbound.RepositoryActor(cleared)
					if actor != "" || privileged || len(fixture.backend.reads) != 0 {
						test.Fatal("denied snapshot retained an actor or reached memory data")
					}
					if fixture.storage.InReadOnlyTransaction(transaction) {
						if _, err := fixture.storage.GetUser(transaction, fixture.reader.ID); err != nil {
							test.Fatalf("denial attempted a write and poisoned the read-only transaction: %v", err)
						}
					}
					return nil
				}
				switch change {
				case "no_snapshot":
					err = check(admitted)
				case "foreign_snapshot":
					err = fixture.peer.WithinReadSnapshot(admitted, check)
				case "write_transaction":
					err = fixture.storage.WithinRepository(admitted, repo, check)
				default:
					err = fixture.storage.WithinReadSnapshot(admitted, check)
				}
				if err != nil {
					test.Fatalf("authorization check could not finish its transaction: %v", err)
				}
				if fixture.state(test) != before {
					test.Fatal("denied snapshot performed session cleanup, promotion, or another authorization write")
				}
				return denied
			}
			fixture.call(test, expected, 0)
		})
	}
}

type qualifiedMemoryPGBackend struct {
	*app.Service
	fixture       *memorySnapshotPGFixture
	test          *testing.T
	qualification domain.ContentHash
	before        func() error
	admissions    int
	queries       int
	checks        int
	result        domain.EffectiveMemoryPage
	failure       error
}

func (backend *qualifiedMemoryPGBackend) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return backend.Service.WithMemoryReadResources(ctx, repo, func(admitted context.Context) error {
		backend.admissions++
		authorize := inbound.DocumentSnapshotReadAuthorizer(admitted)
		if authorize == nil || backend.fixture.storage.InTransaction(admitted) {
			backend.test.Fatal("protocol did not install snapshot authorization before the qualified read")
		}
		return read(inbound.WithDocumentSnapshotReadAuthorizer(admitted, func(transaction context.Context, requested domain.ContentHash) (context.Context, error) {
			backend.checks++
			if requested != repo || !backend.fixture.storage.InReadOnlyTransaction(transaction) || backend.fixture.peer.InReadOnlyTransaction(transaction) {
				backend.test.Fatal("qualified reader invoked authorization outside its owned read-only snapshot")
			}
			authorized, err := authorize(transaction, requested)
			if err == nil {
				actor, system := inbound.RepositoryActor(authorized)
				if actor != backend.fixture.reader.ID || system || !backend.fixture.storage.InReadOnlyTransaction(authorized) || authorized.Done() != transaction.Done() {
					backend.test.Fatal("qualified reader lost its authorized actor or snapshot context")
				}
			}
			return authorized, err
		}))
	})
}

func (backend *qualifiedMemoryPGBackend) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	backend.queries++
	if backend.before != nil {
		if err := backend.before(); err != nil {
			return domain.EffectiveMemoryPage{}, err
		}
	}
	before := backend.fixture.state(backend.test)
	backend.result, backend.failure = backend.Service.QueryQualifiedMemory(ctx, repo, backend.qualification, request)
	if backend.fixture.state(backend.test) != before {
		backend.test.Fatal("qualified MCP read changed authorization state")
	}
	return backend.result, backend.failure
}

func TestPGMCPMemoryQualifiedResultUsesTransportAuthorization(test *testing.T) {
	for _, scenario := range []string{"allowed", "session_revoked", "grant_revoked", "membership_revoked"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newMemorySnapshotPGFixture(test)
			system := inbound.WithSystemActor(fixture.ctx)
			service := fixture.backend.Service
			document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fixture.reader.ID}}}}}}
			raw, err := domain.CanonicalBytes(document.CIR)
			if err != nil {
				test.Fatal(err)
			}
			document.Hash = domain.HashContent(raw)
			if _, err := fixture.storage.PutDoc(system, fixture.repo.ID, document); err != nil {
				test.Fatal(err)
			}
			snapshot := domain.Snapshot{ID: document.Hash, RepoID: fixture.repo.ID, DocHash: document.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}
			if err := fixture.storage.PutSnapshot(system, snapshot); err != nil {
				test.Fatal(err)
			}
			const secret = "Private qualified MCP memory"
			memory, err := service.PutMemoryDigestCAS(system, fixture.repo.ID, domain.MemoryDigest{SnapshotID: snapshot.ID, ClaimsVersion: domain.MemoryClaimsVersion,
				Fragments: []domain.MemoryFragment{{SourceSnapshot: snapshot.ID, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: secret}}}}})
			if err != nil {
				test.Fatal(err)
			}
			selection := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot.ID, MemoryHash: memory, CodeCommit: strings.Repeat("1", 40)}, Limit: 1}
			prepared, err := service.PrepareMemoryRead(system, fixture.repo.ID, selection, nil)
			if err != nil {
				test.Fatal("prepare qualified MCP memory", err)
			}
			qualification, err := service.PublishPreparedMemoryRead(system, prepared, time.Now().Add(time.Hour))
			if err != nil {
				test.Fatal("publish qualified MCP memory", err)
			}
			backend := &qualifiedMemoryPGBackend{Service: service, fixture: fixture, test: test, qualification: qualification}
			server, err := NewServer(backend, fixture.identity, fixture.storage, "https://memory-snapshot.example.test")
			if err != nil {
				test.Fatal(err)
			}
			server.SetEffectiveMemory(backend)
			var expected error
			switch scenario {
			case "session_revoked":
				expected = domain.ErrUnauthorized
				backend.before = func() error { return fixture.revoker.Logout(system, fixture.pair.AccessToken) }
			case "grant_revoked":
				expected = domain.ErrUnauthorized
				backend.before = func() error {
					return fixture.revoker.RevokeMCPToken(system, fixture.pair.RefreshToken, "memory-snapshot-client")
				}
			case "membership_revoked":
				expected = domain.ErrForbidden
				backend.before = func() error {
					return fixture.revoker.RemoveMember(system, fixture.owner.ID, fixture.repository.ID, fixture.reader.ID)
				}
			}
			request := fixture.request(test, toolArgs{Mode: "effective", Ref: string(snapshot.ID), MemoryHash: string(memory), CodeCommit: selection.Selection.CodeCommit})
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			text, failed := memoryAdmissionResult(test, response)
			if backend.admissions != 1 || backend.queries != 1 || backend.checks != 1 {
				test.Fatalf("protocol bypassed qualified snapshot authorization: admissions=%d queries=%d checks=%d body=%s", backend.admissions, backend.queries, backend.checks, text)
			}
			if expected != nil {
				if !failed || !errors.Is(backend.failure, expected) || text != "Error: "+expected.Error() || strings.Contains(text, secret) || !reflect.DeepEqual(backend.result, domain.EffectiveMemoryPage{}) {
					test.Fatalf("qualified MCP read leaked revoked memory: failed=%v error=%v body=%s", failed, backend.failure, text)
				}
				return
			}
			var page effectiveMemorySourceTransportPage
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				test.Fatal(err)
			}
			if failed || backend.failure != nil || page.Mode != "effective" || page.Selection != selection.Selection || page.Total != 1 ||
				!page.ItemComplete || page.Next != "" || !strings.Contains(page.Fragment, secret) || len(backend.result.Items) != 1 {
				test.Fatalf("qualified MCP result changed selection or memory: failed=%v error=%v body=%s", failed, backend.failure, text)
			}
			var item domain.EffectiveMemoryItem
			if err := json.Unmarshal([]byte(page.Fragment), &item); err != nil || !reflect.DeepEqual(item, backend.result.Items[0]) || page.ItemID != item.ID || page.StateHash != backend.result.StateHash {
				test.Fatalf("MCP fragment differs from qualified application result: %v", err)
			}
		})
	}
}

type memorySnapshotPGAuditFailureIdentity struct {
	*app.IdentityService
	cause  error
	admits int
}

func (identity *memorySnapshotPGAuditFailureIdentity) AdmitRepositoryRead(context.Context, string, string) (inbound.RepositoryReadPermit, error) {
	identity.admits++
	return nil, errors.Join(domain.ErrBreakGlassAuditUnavailable, identity.cause)
}

func TestPGMCPMemorySnapshotAuthorizationSanitizesAuditFailure(test *testing.T) {
	fixture := newMemorySnapshotPGFixture(test)
	const privateCause = "private audit database detail: internal-audit.example.test"
	identity := &memorySnapshotPGAuditFailureIdentity{IdentityService: fixture.identity, cause: errors.New(privateCause)}
	server, err := NewServer(fixture.backend, identity, fixture.storage, "https://memory-snapshot.example.test")
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetMemorySnapshotQueries(memorySnapshotScopedTestReader{fixture.backend.snapshots, fixture.backend}); err != nil {
		test.Fatal(err)
	}
	before := fixture.state(test)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, fixture.request(test, toolArgs{Mode: "stored", Ref: string(domain.HashContent([]byte("audit denied snapshot")))}))
	text, failed := memoryAdmissionResult(test, response)
	if !failed || text != "Error: "+domain.ErrBreakGlassAuditUnavailable.Error() || strings.Contains(response.Body.String(), privateCause) ||
		identity.admits != 1 || fixture.backend.admissions != 0 || len(fixture.backend.reads) != 0 {
		test.Fatalf("MCP audit failure was not sanitized before reading memory: failed=%v admits=%d reads=%d body=%s", failed, identity.admits, len(fixture.backend.reads), response.Body)
	}
	if fixture.state(test) != before {
		test.Fatal("failed admission changed authorization state")
	}
}
