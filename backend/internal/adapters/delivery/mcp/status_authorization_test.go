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

type statusMCPQueryProbe struct {
	read func(context.Context, domain.ContentHash) error
}

func (probe statusMCPQueryProbe) List(ctx context.Context, repo domain.ContentHash, _ string, _ int) (domain.GitChangePage, error) {
	return domain.GitChangePage{}, probe.read(ctx, repo)
}

func (probe statusMCPQueryProbe) Get(ctx context.Context, repo domain.ContentHash, _ string) (domain.GitChangeJob, error) {
	return domain.GitChangeJob{}, probe.read(ctx, repo)
}

func (probe statusMCPQueryProbe) ListScans(ctx context.Context, repo domain.ContentHash, _ string, _ int) (domain.GitScanPage, error) {
	return domain.GitScanPage{}, probe.read(ctx, repo)
}

func (probe statusMCPQueryProbe) QueryCodeApplicability(ctx context.Context, repo domain.ContentHash, _ domain.CodeSelection) (domain.CodeApplicability, error) {
	return domain.CodeApplicability{}, probe.read(ctx, repo)
}

func TestStatusMCPProtocolReauthorizesAfterAdmissionAndInsideSnapshot(test *testing.T) {
	for _, tool := range []string{"git_changes", "git_observations", "code_applicability"} {
		for _, profile := range []inbound.ContextQueryProfile{inbound.ContextQueryCompatibility, inbound.ContextQueryPinnedSnapshots} {
			for _, documentProfile := range []inbound.DocumentQueryProfile{inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
				for _, revoked := range []bool{false, true} {
					test.Run(fmt.Sprintf("%s/context=%d/document=%d/revoked=%t", tool, profile, documentProfile, revoked), func(test *testing.T) {
						fixture := newMemoryAdmissionFixture(test)
						identity := &contextMCPSnapshotIdentity{IdentityBackend: fixture.observed, test: test, user: fixture.reader,
							repository: fixture.repository.ID, token: fixture.token, resource: fixture.server.resource}
						fixture.server.identity = identity
						fixture.server.context = &contextMCPAdmissionBackend{contextMCPPoisonedBackend: contextMCPPoisonedBackend{ContextBackend: fixture.backend}, metadata: fixture.backend.GetRepo}
						fixture.server.contextQueryProfile, fixture.server.documentQueryProfile = profile, documentProfile
						calls, reads := 0, 0
						intercepted := errors.New("status payload intercepted after current authorization")
						probe := statusMCPQueryProbe{read: func(ctx context.Context, repo domain.ContentHash) error {
							calls++
							if repo != fixture.repo.ID || identity.resolves != 1 {
								test.Fatal("status query lost initial authentication or repository", repo, identity.resolves)
							}
							authorize := inbound.DocumentReadAuthorizer(ctx)
							if authorize == nil {
								test.Fatal("status protocol omitted post-admission authorization")
							}
							pinned := profile == inbound.ContextQueryPinnedSnapshots
							if revoked && !pinned {
								if err := fixture.identity.Logout(systemTestContext(), fixture.token); err != nil {
									test.Fatal(err)
								}
							}
							bound, err := authorize(ctx, repo)
							if err != nil {
								return err
							}
							snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
							if pinned {
								if snapshot == nil || identity.admissions != 1 || identity.resolves != 2 {
									test.Fatal("status admission did not retain pinned authority")
								}
								if revoked {
									identity.snapshotErr = domain.ErrUnauthorized
								}
								bound, err = snapshot(context.WithValue(bound, contextMCPSnapshotKey{}, true), repo)
							} else if snapshot != nil || identity.admissions != 0 {
								test.Fatal("status compatibility followed the unrelated document profile")
							}
							if err != nil {
								return err
							}
							if actor, system := inbound.RepositoryActor(bound); actor != fixture.reader.ID || system {
								test.Fatal("status query retained stale authority", actor, system)
							}
							reads++
							return intercepted
						}}
						fixture.server.SetGitChanges(probe)
						fixture.server.SetGitScans(probe)
						fixture.server.SetCodeApplicability(probe)
						request := documentEventRequest(test, context.Background(), fixture.token, tool, toolArgs{
							Repository: string(fixture.repo.ID), CodeCommit: strings.Repeat("a", 40), SourceCommit: strings.Repeat("b", 40), Paths: []string{"file.go"},
						})
						response := httptest.NewRecorder()
						fixture.server.Handler().ServeHTTP(response, request)
						text, failed := memoryAdmissionResult(test, response)
						expected, expectedReads := intercepted, 1
						if revoked {
							expected, expectedReads = domain.ErrUnauthorized, 0
						}
						expectedSnapshots := 0
						if profile == inbound.ContextQueryPinnedSnapshots {
							expectedSnapshots = 1
						}
						if !failed || text != "Error: "+expected.Error() || calls != 1 || reads != expectedReads || identity.resolves != 2 ||
							identity.admissions != expectedSnapshots || identity.snapshotResolves != expectedSnapshots || identity.snapshotPermits != expectedSnapshots {
							test.Fatal("status protocol skipped current authority or followed an unrelated profile", text, failed, calls, reads,
								identity.resolves, identity.admissions, identity.snapshotResolves, identity.snapshotPermits)
						}
					})
				}
			}
		}
	}
}
