//go:build postgres

package mcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentEventSnapshotPGBackend struct {
	*app.Service
	snapshot domain.Snapshot
	lookups  []context.Context
}

func (backend *documentEventSnapshotPGBackend) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if repo != backend.snapshot.RepoID || hash != backend.snapshot.ID {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return backend.snapshot, ctx.Err()
}

func (backend *documentEventSnapshotPGBackend) QueryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	if repo != backend.snapshot.RepoID {
		return domain.ContextQueryView{}, domain.ErrNotFound
	}
	return domain.ContextQueryView{Position: backend.snapshot.ID, Snapshots: []domain.Snapshot{backend.snapshot}}, ctx.Err()
}

func (backend *documentEventSnapshotPGBackend) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	backend.lookups = append(backend.lookups, ctx)
	return backend.Service.GetRepo(ctx, repo)
}

func TestPGMCPDocumentEventProtocolReauthorizesPinnedReads(test *testing.T) {
	for _, tool := range []string{"context_fetch", "context_search"} {
		test.Run(tool, func(test *testing.T) {
			for _, change := range []string{"current", "legacy-session", "session-revoked", "membership-revoked"} {
				test.Run(change, func(test *testing.T) {
					fixture := newMemorySnapshotPGFixture(test)
					hash := domain.HashContent([]byte(test.Name()))
					backend := &documentEventSnapshotPGBackend{Service: fixture.backend.Service,
						snapshot: domain.Snapshot{ID: hash, DocHash: hash, RepoID: fixture.repo.ID, DocIdentity: domain.DocumentIdentityRootV1}}
					fixture.server.context = backend
					if err := fixture.server.SetContextQuery(backend, inbound.ContextQueryPinnedSnapshots); err != nil {
						test.Fatal(err)
					}
					intercepted := errors.New("document event data read intercepted after snapshot authorization")
					expected := intercepted
					if change == "session-revoked" {
						expected = domain.ErrUnauthorized
					} else if change == "membership-revoked" {
						expected = domain.ErrForbidden
					}
					reads := 0
					queries := &documentEventQueriesProbe{read: func(request context.Context, repo domain.ContentHash) error {
						authorize := inbound.DocumentReadAuthorizer(request)
						if authorize == nil || fixture.storage.InTransaction(request) {
							test.Fatal("event protocol did not supply admission authorization outside the snapshot")
						}
						admitted, err := authorize(request, repo)
						if err != nil {
							test.Fatal("current token was not accepted before snapshot admission", err)
						}
						snapshotAuthorize := inbound.DocumentSnapshotReadAuthorizer(admitted)
						if snapshotAuthorize == nil || fixture.storage.InTransaction(admitted) {
							test.Fatal("event admission did not retain the read-only snapshot authorization branch")
						}
						system := inbound.WithSystemActor(fixture.ctx)
						switch change {
						case "legacy-session":
							fixture.update(test, `UPDATE sessions SET token=$2 WHERE token=$1`, domain.HashToken(fixture.pair.AccessToken), fixture.pair.AccessToken)
						case "session-revoked":
							if err := fixture.revoker.Logout(system, fixture.pair.AccessToken); err != nil {
								test.Fatal(err)
							}
						case "membership-revoked":
							if err := fixture.revoker.RemoveMember(system, fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
								test.Fatal(err)
							}
						}
						before := fixture.state(test)
						var authorizationErr error
						err = fixture.storage.WithinReadSnapshot(admitted, func(transaction context.Context) error {
							authorized, err := snapshotAuthorize(transaction, repo)
							authorizationErr = err
							actor, privileged := inbound.RepositoryActor(authorized)
							if change == "session-revoked" || change == "membership-revoked" {
								if !errors.Is(err, expected) || actor != "" || privileged {
									test.Fatal("pinned event read retained revoked authority", actor, privileged, err)
								}
							} else {
								if err != nil || actor != fixture.reader.ID || privileged || !fixture.storage.InReadOnlyTransaction(authorized) ||
									fixture.peer.InReadOnlyTransaction(authorized) || authorized.Done() != transaction.Done() ||
									authorized.Value(memorySnapshotPGContextKey{}) != test.Name() {
									test.Fatal("pinned event authorization lost its current actor or owned snapshot", err)
								}
								reads++
							}
							if _, err := fixture.storage.GetUser(transaction, fixture.reader.ID); err != nil {
								test.Fatal("event authorization attempted a write and poisoned its read-only snapshot", err)
							}
							return nil
						})
						if err != nil {
							test.Fatal("event authorization could not complete its read-only snapshot", err)
						}
						if fixture.state(test) != before {
							test.Fatal("pinned event authorization wrote session, identity, audit, membership, or repository state")
						}
						if authorizationErr != nil {
							return authorizationErr
						}
						return intercepted
					}}
					if err := fixture.server.SetDocumentEventQueries(queries, inbound.DocumentQueryVerifiedRoots); err != nil {
						test.Fatal(err)
					}
					requestContext := context.WithValue(fixture.ctx, memorySnapshotPGContextKey{}, test.Name())
					request := documentEventRequest(test, requestContext, fixture.pair.AccessToken, tool,
						toolArgs{Repository: string(fixture.repo.ID), Ref: string(hash), Query: "needle"})
					response := httptest.NewRecorder()
					fixture.server.Handler().ServeHTTP(response, request)
					text, failed := memoryAdmissionResult(test, response)
					if !failed || text != "Error: "+expected.Error() || queries.events != 0 ||
						tool == "context_fetch" && (queries.fragments != 1 || queries.searches != 0) ||
						tool == "context_search" && (queries.searches != 1 || queries.fragments != 0) {
						test.Fatal("event tools/call did not use its explicit, reauthorized query port", text, failed, queries)
					}
					if expected == intercepted && reads != 1 || expected != intercepted && reads != 0 {
						test.Fatal("revocation crossed the event read boundary", reads, expected)
					}
					if change != "session-revoked" {
						if len(backend.lookups) != 1 {
							test.Fatal("pinned event authorization did not reread the current repository", len(backend.lookups))
						}
						lookup := backend.lookups[0]
						actor, privileged := inbound.RepositoryActor(lookup)
						if actor != "" || privileged || !fixture.storage.InReadOnlyTransaction(lookup) || inbound.DocumentReadAuthorizer(lookup) != nil {
							test.Fatal("event snapshot lookup reused an actor or a write-capable authentication callback")
						}
					}
				})
			}
		})
	}
}
