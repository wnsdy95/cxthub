package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type contextMCPQueryFunc func(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error)

func (query contextMCPQueryFunc) QueryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	return query(ctx, repo, selection)
}

func setContextQueryTestPort(test *testing.T, server *Server, query inbound.ContextQuery) {
	test.Helper()
	if err := server.SetContextQuery(query, inbound.ContextQueryCompatibility); err != nil {
		test.Fatal(err)
	}
}

type contextMCPPoisonedBackend struct{ ContextBackend }

func (*contextMCPPoisonedBackend) QueryContext(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error) {
	panic("context query reached the broad MCP backend")
}

func TestSetContextQueryMCPRejectsInvalidConfiguration(test *testing.T) {
	var nilPointer *contextMCPPoisonedBackend
	var nilFunction contextMCPQueryFunc
	valid := contextMCPQueryFunc(func(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error) {
		return domain.ContextQueryView{}, nil
	})
	for _, installed := range []bool{false, true} {
		for _, rejected := range []struct {
			name    string
			query   inbound.ContextQuery
			profile inbound.ContextQueryProfile
		}{
			{name: "nil", profile: inbound.ContextQueryCompatibility},
			{name: "typed pointer", query: nilPointer, profile: inbound.ContextQueryCompatibility},
			{name: "typed function", query: nilFunction, profile: inbound.ContextQueryCompatibility},
			{name: "missing profile", query: valid},
			{name: "unknown profile", query: valid, profile: 255},
		} {
			test.Run(fmt.Sprintf("installed=%t/%s", installed, rejected.name), func(test *testing.T) {
				server := &Server{}
				owner := &contextMCPPoisonedBackend{}
				if installed {
					if err := server.SetContextQuery(owner, inbound.ContextQueryPinnedSnapshots); err != nil {
						test.Fatal(err)
					}
				}
				if err := server.SetContextQuery(rejected.query, rejected.profile); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("invalid query configuration accepted", err)
				}
				if installed && (server.contextQueries != owner || server.contextQueryProfile != inbound.ContextQueryPinnedSnapshots) ||
					!installed && (server.contextQueries != nil || server.contextQueryProfile != 0) {
					test.Fatal("rejected configuration replaced the previous owner or profile")
				}
			})
		}
	}
}

func TestContextQueryMCPMissingPortAndErrorsDoNotDiscoverBackend(test *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	for _, tool := range []string{"list", "segments", "history", "search"} {
		test.Run(tool, func(test *testing.T) {
			server := &Server{context: &contextMCPPoisonedBackend{}}
			if err := server.SetDocumentEventQueries(&documentEventQueriesProbe{}, inbound.DocumentQueryVerifiedRoots); err != nil {
				test.Fatal(err)
			}
			read := func() (string, error) {
				switch tool {
				case "segments":
					return server.contextSegmentPage(context.Background(), repo, toolArgs{Mode: "segments"})
				case "history":
					return server.historyPage(context.Background(), repo, toolArgs{})
				case "search":
					return server.searchPage(context.Background(), repo, toolArgs{Query: "needle"})
				default:
					return server.contextPage(context.Background(), repo, toolArgs{})
				}
			}
			body, err := read()
			if body != "" || !errors.Is(err, domain.ErrAgentHistoryUnavailable) || !strings.Contains(err.Error(), "context query is not configured") {
				test.Fatal("absent query silently discovered the backend or changed its tool error", body, err)
			}
			for _, expected := range []error{domain.ErrValidation, domain.ErrConflict, domain.ErrIntegrity, domain.ErrNotFound,
				domain.ErrUnauthorized, domain.ErrForbidden, domain.ErrCatalogUnsupported, domain.ErrResourceOverloaded, context.Canceled} {
				calls := 0
				setContextQueryTestPort(test, server, contextMCPQueryFunc(func(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error) {
					calls++
					return domain.ContextQueryView{}, expected
				}))
				body, err := read()
				if body != "" || !errors.Is(err, expected) || calls != 1 {
					test.Fatal("explicit query error fell back or changed", body, err, calls)
				}
			}
		})
	}
}

func TestContextQueryMCPProtocolRequiresExplicitPort(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	fixture.server.context = &contextMCPPoisonedBackend{ContextBackend: fixture.backend}
	if err := fixture.server.SetDocumentEventQueries(&documentEventQueriesProbe{}, inbound.DocumentQueryCompatibility); err != nil {
		test.Fatal(err)
	}
	for _, tool := range []struct {
		name string
		mode string
	}{{name: "context_list"}, {name: "context_list", mode: "segments"}, {name: "context_history"}, {name: "context_search"}} {
		request := documentEventRequest(test, context.Background(), fixture.token, tool.name,
			toolArgs{Repository: string(fixture.repo.ID), Mode: tool.mode, Query: "needle"})
		response := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(response, request)
		text, failed := memoryAdmissionResult(test, response)
		if !failed || !strings.Contains(text, "context query is not configured") || fixture.server.contextQueries != nil {
			test.Fatal("protocol discovered an implicit query owner or lost its tool error", tool.name, tool.mode, text, failed)
		}
	}
}

func TestContextQueryMCPInjectedPortRetainsSelectionAndCursor(test *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	for _, tool := range []string{"list", "segments", "history", "search"} {
		test.Run(tool, func(test *testing.T) {
			server := &Server{context: &contextMCPPoisonedBackend{}}
			if err := server.SetDocumentEventQueries(&documentEventQueriesProbe{}, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			requestKey := struct{ name string }{"context MCP request"}
			parent, cancel := context.WithTimeout(context.WithValue(context.Background(), requestKey, "retained"), 30*time.Second)
			defer cancel()
			parent = inbound.WithRepositoryActor(inbound.WithDocumentIdentities(parent, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), "request-actor")
			args := toolArgs{Repository: string(repo.ID), Branch: "logical", Position: "main", Scope: "current", CodeCommit: strings.Repeat("b", 40), Query: "needle", Limit: 1}
			if tool == "segments" {
				args.Mode = "segments"
			}
			state, position := pageHash(3), pageHash(4)
			calls := []domain.ContextSelection{}
			setContextQueryTestPort(test, server, contextMCPQueryFunc(func(ctx context.Context, requested domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
				if requested != repo.ID || ctx.Value(requestKey) != "retained" || ctx.Err() != nil {
					test.Fatal("query lost repository or request context", requested, ctx.Err())
				}
				if actor, system := inbound.RepositoryActor(ctx); actor != "request-actor" || system {
					test.Fatal("query changed inherited request authority", actor, system)
				}
				if identities := inbound.DocumentIdentities(ctx); len(identities) != 1 || identities[0] != domain.DocumentIdentityRootV1 {
					test.Fatal("query lost negotiated document identities", identities)
				}
				deadline, present := ctx.Deadline()
				if original, _ := parent.Deadline(); !present || !deadline.Equal(original) {
					test.Fatal("query changed the original deadline", deadline, original)
				}
				calls = append(calls, selection)
				return domain.ContextQueryView{Version: 1, Branch: "logical", Position: position, StateHash: state,
					Snapshots: []domain.Snapshot{{ID: pageHash(5), Message: "needle one"}, {ID: pageHash(6), Message: "needle two"}},
					History:   []domain.HistoryEvent{{ID: "new", CreatedAt: time.Unix(2, 0)}, {ID: "old", CreatedAt: time.Unix(1, 0)}},
					Segments:  &domain.ContextSegmentPage{Version: domain.ContextSegmentVersion, StateHash: state, PageHash: pageHash(7), Next: 1}}, nil
			}))
			read := func() (string, error) {
				switch tool {
				case "segments":
					return server.contextSegmentPage(parent, repo, args)
				case "history":
					return server.historyPage(parent, repo, args)
				case "search":
					return server.searchPage(parent, repo, args)
				default:
					return server.contextPage(parent, repo, args)
				}
			}
			for page := 0; page < 2; page++ {
				body, err := read()
				if err != nil {
					test.Fatal(err)
				}
				var envelope struct {
					Next string `json:"next_cursor"`
				}
				if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Next == "" && page == 0 {
					test.Fatal("explicit query lost its continuation", body, err)
				}
				args.Cursor = envelope.Next
			}
			first := domain.ContextSelection{Branch: args.Branch, Position: args.Position, Scope: args.Scope, CodeCommit: args.CodeCommit}
			second := first
			second.Position = string(position)
			if tool == "segments" {
				first.SegmentLimit, second.SegmentLimit, second.SegmentOffset, second.SegmentStateHash = 1, 1, 1, state
			}
			if tool == "history" {
				first, second = domain.ContextSelection{Branch: args.Branch}, domain.ContextSelection{Branch: args.Branch}
			}
			if !reflect.DeepEqual(calls, []domain.ContextSelection{first, second}) {
				test.Fatal("cursor or branch/code selection changed across the injected query", calls)
			}
		})
	}
}

type contextMCPSnapshotKey struct{}

type contextMCPSnapshotIdentity struct {
	IdentityBackend
	test             *testing.T
	user             domain.User
	repository       string
	token            string
	resource         string
	resolves         int
	admissions       int
	snapshotResolves int
	snapshotPermits  int
	snapshotErr      error
}

type contextMCPPermit func(context.Context, string, string) error

func (permit contextMCPPermit) AuthorizeSnapshot(ctx context.Context, repository, actor string) error {
	return permit(ctx, repository, actor)
}

func (identity *contextMCPSnapshotIdentity) ResolveMCPUser(ctx context.Context, token string, resources ...string) (domain.User, error) {
	if ctx.Value(contextMCPSnapshotKey{}) == true {
		identity.test.Fatal("general MCP authentication ran inside a pinned context snapshot")
	}
	identity.resolves++
	return identity.IdentityBackend.ResolveMCPUser(ctx, token, resources...)
}

func (identity *contextMCPSnapshotIdentity) AdmitRepositoryRead(ctx context.Context, repository, actor string) (inbound.RepositoryReadPermit, error) {
	if ctx.Value(contextMCPSnapshotKey{}) == true || repository != identity.repository || actor != identity.user.ID {
		identity.test.Fatal("context admission did not precede the snapshot with current authority", repository, actor)
	}
	identity.admissions++
	return contextMCPPermit(func(bound context.Context, requested, current string) error {
		if bound.Value(contextMCPSnapshotKey{}) != true || requested != repository || current != actor {
			identity.test.Fatal("context snapshot permit lost its bound context or identity", requested, current)
		}
		identity.snapshotPermits++
		return identity.snapshotErr
	}), nil
}

func (*contextMCPSnapshotIdentity) ResolveUserForSnapshot(context.Context, string) (domain.User, error) {
	panic("MCP context queries must not resolve HTTP snapshot identities")
}

func (identity *contextMCPSnapshotIdentity) ResolveMCPUserForSnapshot(ctx context.Context, token string, resources ...string) (domain.User, error) {
	if ctx.Value(contextMCPSnapshotKey{}) != true || token != identity.token || len(resources) != 1 || resources[0] != identity.resource {
		identity.test.Fatal("context snapshot authentication lost its original credential, resource or snapshot")
	}
	identity.snapshotResolves++
	return identity.user, nil
}

type contextMCPAdmissionBackend struct {
	contextMCPPoisonedBackend
	metadata func(context.Context, domain.ContentHash) (domain.Repo, error)
}

func (backend *contextMCPAdmissionBackend) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return backend.metadata(ctx, repo)
}

func TestContextQueryMCPProtocolAuthorizationUsesOwnProfile(test *testing.T) {
	for _, tool := range []struct {
		name string
		mode string
	}{{name: "context_list"}, {name: "context_list", mode: "segments"}, {name: "context_history"}, {name: "context_search"}} {
		for _, profile := range []inbound.ContextQueryProfile{inbound.ContextQueryCompatibility, inbound.ContextQueryPinnedSnapshots} {
			for _, documentProfile := range []inbound.DocumentQueryProfile{0, inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
				if tool.name == "context_search" && documentProfile == 0 {
					continue
				}
				for _, revoked := range []bool{false, true} {
					test.Run(fmt.Sprintf("%s/%s/context=%d/document=%d/revoked=%t", tool.name, tool.mode, profile, documentProfile, revoked), func(test *testing.T) {
						fixture := newMemoryAdmissionFixture(test)
						identity := &contextMCPSnapshotIdentity{IdentityBackend: fixture.observed, test: test, user: fixture.reader,
							repository: fixture.repository.ID, token: fixture.token, resource: fixture.server.resource}
						fixture.server.identity = identity
						fixture.server.context = &contextMCPAdmissionBackend{contextMCPPoisonedBackend: contextMCPPoisonedBackend{ContextBackend: fixture.backend},
							metadata: fixture.backend.GetRepo}
						if documentProfile != 0 {
							if err := fixture.server.SetDocumentEventQueries(&documentEventQueriesProbe{}, documentProfile); err != nil {
								test.Fatal(err)
							}
						}
						calls, reads := 0, 0
						intercepted := errors.New("explicit context query read intercepted")
						query := contextMCPQueryFunc(func(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
							calls++
							if repo != fixture.repo.ID || identity.resolves != 1 {
								test.Fatal("context authority ran before query admission or lost initial authentication", repo, identity.resolves)
							}
							authorize := inbound.DocumentReadAuthorizer(ctx)
							if authorize == nil {
								test.Fatal("actual protocol query path has no current authorizer")
							}
							pinned := profile == inbound.ContextQueryPinnedSnapshots
							if revoked && !pinned {
								if err := fixture.identity.Logout(systemTestContext(), fixture.token); err != nil {
									test.Fatal(err)
								}
							}
							bound, err := authorize(ctx, repo)
							if err != nil {
								return domain.ContextQueryView{}, err
							}
							snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
							if pinned {
								if snapshot == nil || identity.admissions != 1 || identity.resolves != 2 {
									test.Fatal("context profile did not split admission and snapshot authority")
								}
								if revoked {
									identity.snapshotErr = domain.ErrUnauthorized
								}
								bound, err = snapshot(context.WithValue(bound, contextMCPSnapshotKey{}, true), repo)
							} else if snapshot != nil || identity.admissions != 0 {
								test.Fatal("compatibility context profile adopted document snapshot policy")
							}
							if err != nil {
								return domain.ContextQueryView{}, err
							}
							if actor, system := inbound.RepositoryActor(bound); actor != fixture.reader.ID || system {
								test.Fatal("context query retained a stale actor", actor, system)
							}
							reads++
							return domain.ContextQueryView{}, intercepted
						})
						if err := fixture.server.SetContextQuery(query, profile); err != nil {
							test.Fatal(err)
						}
						request := documentEventRequest(test, context.Background(), fixture.token, tool.name,
							toolArgs{Repository: string(fixture.repo.ID), Mode: tool.mode, Query: "needle"})
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
							test.Fatal("context protocol followed unrelated policy or stale authority", text, failed, calls, reads,
								identity.resolves, identity.admissions, identity.snapshotResolves, identity.snapshotPermits)
						}
					})
				}
			}
		}
	}
}
