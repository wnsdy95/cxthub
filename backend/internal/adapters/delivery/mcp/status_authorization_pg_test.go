//go:build postgres

package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGMCPStatusProtocolReauthorizesPinnedReads(test *testing.T) {
	for _, tool := range []string{"git_changes", "git_observations", "code_applicability", "context_link_status"} {
		for _, change := range []string{"current", "session-revoked", "membership-revoked"} {
			test.Run(fmt.Sprintf("%s/%s", tool, change), func(test *testing.T) {
				fixture := newMemorySnapshotPGFixture(test)
				fixture.server.contextQueryProfile = inbound.ContextQueryPinnedSnapshots
				fixture.server.documentQueryProfile = inbound.DocumentQueryCompatibility
				intercepted := errors.New("status payload intercepted in its current read snapshot")
				expected := intercepted
				if change == "session-revoked" {
					expected = domain.ErrUnauthorized
				} else if change == "membership-revoked" {
					expected = domain.ErrForbidden
				}
				calls, reads := 0, 0
				probe := statusMCPQueryProbe{read: func(request context.Context, repo domain.ContentHash) error {
					calls++
					authorize := inbound.DocumentReadAuthorizer(request)
					if authorize == nil || fixture.storage.InTransaction(request) {
						test.Fatal("status protocol omitted current authorization outside the snapshot")
					}
					admitted, err := authorize(request, repo)
					if err != nil {
						test.Fatal(err)
					}
					snapshot := inbound.DocumentSnapshotReadAuthorizer(admitted)
					if snapshot == nil || fixture.storage.InTransaction(admitted) {
						test.Fatal("status admission omitted its read-only snapshot branch")
					}
					system := inbound.WithSystemActor(fixture.ctx)
					if change == "session-revoked" {
						if err := fixture.revoker.Logout(system, fixture.pair.AccessToken); err != nil {
							test.Fatal(err)
						}
					} else if change == "membership-revoked" {
						if err := fixture.revoker.RemoveMember(system, fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
							test.Fatal(err)
						}
					}
					before := fixture.state(test)
					var authorizationError error
					err = fixture.storage.WithinReadSnapshot(admitted, func(transaction context.Context) error {
						bound, err := snapshot(transaction, repo)
						authorizationError = err
						actor, system := inbound.RepositoryActor(bound)
						if change == "current" {
							if err != nil || actor != fixture.reader.ID || system || !fixture.storage.InReadOnlyTransaction(bound) || fixture.peer.InReadOnlyTransaction(bound) || bound.Done() != transaction.Done() {
								test.Fatal("status authentication lost its owned current snapshot", actor, system, err)
							}
							reads++
						} else if !errors.Is(err, expected) || actor != "" || system {
							test.Fatal("status authentication retained revoked authority", actor, system, err)
						}
						if _, err := fixture.storage.GetUser(transaction, fixture.reader.ID); err != nil {
							test.Fatal("status authentication poisoned its read-only transaction", err)
						}
						return nil
					})
					if err != nil || fixture.state(test) != before {
						test.Fatal("status authentication changed state inside its read-only snapshot", err)
					}
					if authorizationError != nil {
						return authorizationError
					}
					return intercepted
				}}
				fixture.server.SetGitChanges(probe)
				fixture.server.SetGitScans(probe)
				fixture.server.SetCodeApplicability(probe)
				if err := fixture.server.SetGitAssociationStatus(probe); err != nil {
					test.Fatal(err)
				}
				request := documentEventRequest(test, fixture.ctx, fixture.pair.AccessToken, tool, toolArgs{
					Repository: string(fixture.repo.ID), CodeCommit: strings.Repeat("a", 40), SourceCommit: strings.Repeat("b", 40), Paths: []string{"file.go"},
				})
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, request)
				text, failed := memoryAdmissionResult(test, response)
				if !failed || text != "Error: "+expected.Error() || calls != 1 || change == "current" && reads != 1 || change != "current" && reads != 0 {
					test.Fatal("status protocol bypassed current PostgreSQL authority", text, failed, calls, reads)
				}
			})
		}
	}
}
