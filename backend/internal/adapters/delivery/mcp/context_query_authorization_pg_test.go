//go:build postgres

package mcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGMCPContextQueryProtocolReauthorizesPinnedReads(test *testing.T) {
	for _, tool := range []struct {
		name string
		mode string
	}{{name: "context_list"}, {name: "context_list", mode: "segments"}, {name: "context_history"}, {name: "context_search"}} {
		test.Run(tool.name+"/"+tool.mode, func(test *testing.T) {
			for _, change := range []string{"current", "legacy-session", "session-revoked", "membership-revoked"} {
				test.Run(change, func(test *testing.T) {
					fixture := newMemorySnapshotPGFixture(test)
					fixture.server.context = &contextMCPAdmissionBackend{
						contextMCPPoisonedBackend: contextMCPPoisonedBackend{ContextBackend: fixture.backend}, metadata: fixture.backend.GetRepo,
					}
					if tool.name == "context_search" {
						if err := fixture.server.SetDocumentEventQueries(&documentEventQueriesProbe{}, inbound.DocumentQueryCompatibility); err != nil {
							test.Fatal(err)
						}
					}
					intercepted := errors.New("context data read intercepted after snapshot authorization")
					expected := intercepted
					if change == "session-revoked" {
						expected = domain.ErrUnauthorized
					} else if change == "membership-revoked" {
						expected = domain.ErrForbidden
					}
					calls, reads := 0, 0
					query := contextMCPQueryFunc(func(request context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
						calls++
						authorize := inbound.DocumentReadAuthorizer(request)
						if authorize == nil || fixture.storage.InTransaction(request) || repo != fixture.repo.ID {
							test.Fatal("context protocol lost its admission authorizer or entered a snapshot too early")
						}
						admitted, err := authorize(request, repo)
						if err != nil {
							test.Fatal("current token failed context admission", err)
						}
						snapshotAuthorize := inbound.DocumentSnapshotReadAuthorizer(admitted)
						if snapshotAuthorize == nil || fixture.storage.InTransaction(admitted) {
							test.Fatal("context profile did not retain its read-only snapshot authentication branch")
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
									test.Fatal("pinned context query retained revoked authority", actor, privileged, err)
								}
							} else {
								if err != nil || actor != fixture.reader.ID || privileged || !fixture.storage.InReadOnlyTransaction(authorized) ||
									fixture.peer.InReadOnlyTransaction(authorized) || authorized.Done() != transaction.Done() ||
									authorized.Value(memorySnapshotPGContextKey{}) != test.Name() {
									test.Fatal("context snapshot authorization lost its actor or owned read-only transaction", err)
								}
								reads++
							}
							if _, err := fixture.storage.GetUser(transaction, fixture.reader.ID); err != nil {
								test.Fatal("context authentication attempted a write inside the read-only transaction", err)
							}
							return nil
						})
						if err != nil || fixture.state(test) != before {
							test.Fatal("context authentication poisoned its read-only snapshot or wrote authority state", err)
						}
						if authorizationErr != nil {
							return domain.ContextQueryView{}, authorizationErr
						}
						return domain.ContextQueryView{}, intercepted
					})
					if err := fixture.server.SetContextQuery(query, inbound.ContextQueryPinnedSnapshots); err != nil {
						test.Fatal(err)
					}
					requestContext := context.WithValue(fixture.ctx, memorySnapshotPGContextKey{}, test.Name())
					request := documentEventRequest(test, requestContext, fixture.pair.AccessToken, tool.name,
						toolArgs{Repository: string(fixture.repo.ID), Mode: tool.mode, Query: "needle"})
					response := httptest.NewRecorder()
					fixture.server.Handler().ServeHTTP(response, request)
					text, failed := memoryAdmissionResult(test, response)
					if !failed || text != "Error: "+expected.Error() || calls != 1 ||
						expected == intercepted && reads != 1 || expected != intercepted && reads != 0 {
						test.Fatal("context protocol lost explicit current-authority dispatch", text, failed, calls, reads)
					}
					if change != "session-revoked" {
						if len(fixture.backend.lookups) != 1 {
							test.Fatal("pinned context authorization did not reread the current repository", len(fixture.backend.lookups))
						}
						lookup := fixture.backend.lookups[0]
						actor, privileged := inbound.RepositoryActor(lookup)
						if actor != "" || privileged || !fixture.storage.InReadOnlyTransaction(lookup) || inbound.DocumentReadAuthorizer(lookup) != nil {
							test.Fatal("context snapshot lookup reused an actor or a general authentication callback")
						}
					}
				})
			}
		})
	}
}
