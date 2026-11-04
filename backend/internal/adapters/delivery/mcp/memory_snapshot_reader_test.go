package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func memorySnapshotQueriesForTest(test *testing.T, service *app.Service) inbound.MemorySnapshotQueries {
	test.Helper()
	reader, err := service.MemorySnapshotQueries()
	if err != nil {
		test.Fatal(err)
	}
	return reader
}

type memorySnapshotScopedTestReader struct {
	inbound.MemorySnapshotQueries
	scope inbound.MemoryReadScope
}

func (reader memorySnapshotScopedTestReader) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return reader.scope.WithMemoryReadResources(ctx, repo, read)
}

type memorySnapshotMCPReader struct {
	inbound.MemorySnapshotQueries
	object     func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
	projection func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryProjection, error)
}

func (reader *memorySnapshotMCPReader) GetMemoryObject(ctx context.Context, repo, memory domain.ContentHash) (domain.MemoryDigest, error) {
	return reader.object(ctx, repo, memory)
}

func (reader *memorySnapshotMCPReader) GetMemoryProjection(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
	return reader.projection(ctx, repo, snapshot)
}

type memorySnapshotMCPBackendTrap struct {
	*preparedMemoryContextOnly
	scopes int
	reads  int
}

func (backend *memorySnapshotMCPBackendTrap) WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error {
	backend.scopes++
	return domain.ErrIntegrity
}

func (backend *memorySnapshotMCPBackendTrap) GetMemoryObject(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	backend.reads++
	return domain.MemoryDigest{}, domain.ErrIntegrity
}

func (backend *memorySnapshotMCPBackendTrap) GetMemoryProjection(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryProjection, error) {
	backend.reads++
	return domain.MemoryProjection{}, domain.ErrIntegrity
}

func TestSetMemorySnapshotQueriesRejectsNilWithoutReplacingReader(test *testing.T) {
	var typedNil *memorySnapshotMCPReader
	var typedNilOwner *memoryquery.SnapshotQueries
	for _, installed := range []bool{false, true} {
		test.Run(map[bool]string{false: "empty", true: "installed"}[installed], func(test *testing.T) {
			server := &Server{}
			reader := &memorySnapshotMCPReader{}
			if installed {
				if err := server.SetMemorySnapshotQueries(reader); err != nil {
					test.Fatal(err)
				}
			}
			for _, rejected := range []inbound.MemorySnapshotQueries{nil, typedNil, typedNilOwner} {
				if err := server.SetMemorySnapshotQueries(rejected); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("nil snapshot reader accepted", err)
				}
				if installed && server.memorySnapshots != reader || !installed && server.memorySnapshots != nil {
					test.Fatal("rejected reader replaced the previous state")
				}
			}
			replacement := &memorySnapshotMCPReader{}
			if err := server.SetMemorySnapshotQueries(replacement); err != nil || server.memorySnapshots != replacement {
				test.Fatal("valid replacement rejected", err)
			}
		})
	}
}

func TestMemorySnapshotMCPInjectedReaderOwnsScopeAndSerializedPage(test *testing.T) {
	for _, selection := range []struct {
		name string
		mode string
		hash bool
	}{{"stored", "stored", false}, {"exact_object", "", true}, {"snapshot", "", false}, {"snapshot_project", "project", false}} {
		for _, broadScope := range []bool{false, true} {
			for _, exit := range []struct {
				name string
				err  error
			}{{"ready", nil}, {"forbidden", domain.ErrForbidden}, {"canceled", context.Canceled}} {
				test.Run(selection.name+"/"+map[bool]string{false: "no_broad_scope", true: "poisoned_broad_scope"}[broadScope]+"/"+exit.name, func(test *testing.T) {
					fixture := newMemoryAdmissionFixture(test)
					contextOnly := &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}
					broad := &memorySnapshotMCPBackendTrap{preparedMemoryContextOnly: contextOnly}
					fixture.server.context = contextOnly
					if broadScope {
						fixture.server.context = broad
					}
					args := toolArgs{Mode: selection.mode, Ref: string(fixture.snapshot.ID)}
					if selection.hash {
						args.MemoryHash = string(fixture.memory)
					}
					want, err := fixture.server.memoryPage(systemTestContext(), fixture.repo, args)
					if err != nil {
						test.Fatal(err)
					}
					digest, err := fixture.backend.snapshots.GetMemoryObject(systemTestContext(), fixture.repo.ID, fixture.memory)
					if err != nil {
						test.Fatal(err)
					}
					projection, err := fixture.backend.snapshots.GetMemoryProjection(systemTestContext(), fixture.repo.ID, fixture.snapshot.ID)
					if err != nil {
						test.Fatal(err)
					}
					scopes, calls := 0, 0
					active, completed := false, false
					var readContext context.Context
					scope := preparedMemoryScopeFunc(func(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
						scopes++
						return fixture.backend.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
							active = true
							defer func() { active = false }()
							if err := read(bound); err != nil {
								return err
							}
							completed = true
							digest.KeyFacts[0] = "Mutation after serialization must not leak"
							if len(projection.Digest.KeyFacts) > 0 {
								projection.Digest.KeyFacts[0] = "Mutation after projection serialization must not leak"
							}
							return exit.err
						})
					})
					check := func(ctx context.Context, repo, selected, expected domain.ContentHash) {
						calls++
						readContext = ctx
						actor, system := inbound.RepositoryActor(ctx)
						if _, limited := ctx.Deadline(); !limited || !active || actor != fixture.reader.ID || system || repo != fixture.repo.ID || selected != expected || inbound.DocumentReadAuthorizer(ctx) == nil {
							test.Fatal("snapshot query bypassed injected scoped admission")
						}
					}
					probe := &memorySnapshotMCPReader{MemorySnapshotQueries: fixture.backend.snapshots,
						object: func(ctx context.Context, repo, memory domain.ContentHash) (domain.MemoryDigest, error) {
							check(ctx, repo, memory, fixture.memory)
							return digest, nil
						}, projection: func(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
							check(ctx, repo, snapshot, fixture.snapshot.ID)
							return projection, nil
						}}
					if err := fixture.server.SetMemorySnapshotQueries(memorySnapshotScopedTestReader{probe, scope}); err != nil {
						test.Fatal(err)
					}
					response := httptest.NewRecorder()
					fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
					text, failed := memoryAdmissionResult(test, response)
					if scopes != 1 || calls != 1 || !completed || active || broad.scopes != 0 || broad.reads != 0 || readContext == nil || !errors.Is(readContext.Err(), context.Canceled) {
						test.Fatal("snapshot MCP discovered broad capabilities or escaped its scope", scopes, calls, text)
					}
					if exit.err != nil {
						if !failed || text != "Error: "+exit.err.Error() || strings.Contains(text, "json_fragment") {
							test.Fatal("failed scope returned serialized snapshot bytes", text)
						}
					} else if failed || text != want {
						test.Fatal("snapshot serialization changed or escaped the callback", text, want)
					}
				})
			}
		}
	}
}

func TestMemorySnapshotMCPMissingReaderDoesNotDiscoverBackend(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	broad := &memorySnapshotMCPBackendTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
	server, err := NewServer(broad, fixture.identity, fixture.storage, "https://memory.example.test")
	if err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"stored", ""} {
		args := toolArgs{Repository: string(fixture.repo.ID), Mode: mode, Ref: string(fixture.snapshot.ID), MemoryHash: string(fixture.memory)}
		raw, err := json.Marshal(args)
		if err != nil {
			test.Fatal(err)
		}
		if result, err := server.runTool(context.Background(), fixture.reader, "memory_load", raw); !errors.Is(err, errMemorySnapshotsUnavailable) || result != "" {
			test.Fatal("missing snapshot reader discovered broad capabilities", result, err)
		}
	}
	if _, _, err := server.nearestDigest(systemTestContext(), fixture.repo.ID, fixture.snapshot); !errors.Is(err, errMemorySnapshotsUnavailable) {
		test.Fatal("nearest digest rediscovered broad memory getter", err)
	}
	if _, err := server.toolMemoryLoad(systemTestContext(), fixture.repo, string(fixture.snapshot.ID)); !errors.Is(err, errMemorySnapshotsUnavailable) {
		test.Fatal("legacy tool rediscovered broad projection getter", err)
	}
	if _, err := server.projectMemory(systemTestContext(), fixture.repo, toolArgs{}, fixture.snapshot.ID); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("branch fallback rediscovered broad projection getter", err)
	}
	if _, err := server.memoryPage(systemTestContext(), fixture.repo, toolArgs{Ref: string(fixture.snapshot.ID), MemoryHash: string(fixture.memory)}); !errors.Is(err, errMemorySnapshotsUnavailable) {
		test.Fatal("exact page rediscovered broad object getter", err)
	}
	if broad.scopes != 0 || broad.reads != 0 {
		test.Fatal("missing injection invoked broad memory capabilities", broad.scopes, broad.reads)
	}
}

func TestMemorySnapshotMCPReaderDoesNotReplaceBranchOrEffectiveScope(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	broad := &memorySnapshotMCPBackendTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
	fixture.server.context = broad
	setLiveMemoryFixture(test, fixture.server, fixture.backend.Service, broad)
	scope := preparedMemoryScopeFunc(func(context.Context, domain.ContentHash, func(context.Context) error) error {
		test.Fatal("branch/effective mode entered snapshot-only scope")
		return nil
	})
	if err := fixture.server.SetMemorySnapshotQueries(memorySnapshotScopedTestReader{fixture.backend.snapshots, scope}); err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"", "project", "effective"} {
		args := toolArgs{Repository: string(fixture.repo.ID), Mode: mode, Ref: "main"}
		raw, err := json.Marshal(args)
		if err != nil {
			test.Fatal(err)
		}
		before := broad.scopes
		if result, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", raw); !errors.Is(err, domain.ErrIntegrity) || result != "" || broad.scopes != before+1 || broad.reads != 0 {
			test.Fatal("branch/effective mode changed its legacy outer admission", result, err)
		}
	}
}

func TestMemorySnapshotMCPQueryErrorsRemainUnchanged(test *testing.T) {
	for _, failure := range []error{domain.ErrNotFound, domain.ErrIntegrity, domain.ErrForbidden, domain.ErrUnauthorized, context.Canceled} {
		test.Run(failure.Error(), func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			reader := &memorySnapshotMCPReader{MemorySnapshotQueries: fixture.backend.snapshots,
				object: func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
					return domain.MemoryDigest{Summary: "must not leak"}, failure
				}, projection: func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryProjection, error) {
					return domain.MemoryProjection{Found: true, Digest: domain.MemoryDigest{Summary: "must not leak"}}, failure
				}}
			if err := fixture.server.SetMemorySnapshotQueries(reader); err != nil {
				test.Fatal(err)
			}
			for _, args := range []toolArgs{{Mode: "stored", Ref: string(fixture.snapshot.ID)}, {Ref: string(fixture.snapshot.ID), MemoryHash: string(fixture.memory)}, {Mode: "project", Ref: string(fixture.snapshot.ID)}} {
				if result, err := fixture.server.memoryPage(systemTestContext(), fixture.repo, args); !errors.Is(err, failure) || result != "" {
					test.Fatal("snapshot query error changed or returned payload", result, err)
				}
			}
		})
	}
}

func TestMemorySnapshotMCPInjectedScopeFailurePreventsRead(test *testing.T) {
	for _, failure := range []error{domain.ErrForbidden, domain.ErrIntegrity, domain.ErrResourceOverloaded, context.DeadlineExceeded} {
		test.Run(failure.Error(), func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			broad := &memorySnapshotMCPBackendTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
			fixture.server.context = broad
			scopes := 0
			scope := preparedMemoryScopeFunc(func(ctx context.Context, repo domain.ContentHash, _ func(context.Context) error) error {
				scopes++
				if _, limited := ctx.Deadline(); !limited || repo != fixture.repo.ID || inbound.DocumentReadAuthorizer(ctx) == nil {
					test.Fatal("snapshot scope omitted deadline or authorization")
				}
				return failure
			})
			if err := fixture.server.SetMemorySnapshotQueries(memorySnapshotScopedTestReader{fixture.backend.snapshots, scope}); err != nil {
				test.Fatal(err)
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), toolArgs{Mode: "stored", Ref: string(fixture.snapshot.ID)}))
			text, failed := memoryAdmissionResult(test, response)
			if !failed || text != "Error: "+failure.Error() || scopes != 1 || broad.scopes != 0 || broad.reads != 0 || fixture.backend.reads.Load() != 0 {
				test.Fatal("rejected snapshot scope reached payload or broad admission", text, scopes)
			}
		})
	}
}

func TestMemorySnapshotMCPLegacyGetterPathsUseInjectedReader(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	broad := &memorySnapshotMCPBackendTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
	fixture.server.context = broad
	objects, projections, branches := 0, 0, 0
	branch := branchMemoryQueryFunc(func(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
		branches++
		return fixture.backend.Service.QueryBranchMemory(ctx, repo, branch, snapshot, code)
	})
	if err := fixture.server.SetBranchMemory(liveMemoryFixtureReader{MemoryReadScope: fixture.backend, BranchMemoryQuery: branch}); err != nil {
		test.Fatal(err)
	}
	reader := &memorySnapshotMCPReader{MemorySnapshotQueries: fixture.backend.snapshots,
		object: func(ctx context.Context, repo, memory domain.ContentHash) (domain.MemoryDigest, error) {
			objects++
			return fixture.backend.snapshots.GetMemoryObject(ctx, repo, memory)
		}, projection: func(ctx context.Context, repo, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
			projections++
			return fixture.backend.snapshots.GetMemoryProjection(ctx, repo, snapshot)
		}}
	if err := fixture.server.SetMemorySnapshotQueries(reader); err != nil {
		test.Fatal(err)
	}
	if result, err := fixture.server.toolContextFetch(systemTestContext(), fixture.repo, string(fixture.snapshot.ID), 12); err != nil || !strings.Contains(result, "Memory is unchanged") {
		test.Fatal("context tool did not use injected object reader", result, err)
	}
	if result, err := fixture.server.toolMemoryLoad(systemTestContext(), fixture.repo, string(fixture.snapshot.ID)); err != nil || !strings.Contains(result, "Memory is unchanged") {
		test.Fatal("legacy memory tool did not use injected projection reader", result, err)
	}
	if result, err := fixture.server.projectMemory(systemTestContext(), fixture.repo, toolArgs{}, fixture.snapshot.ID); err != nil || !result.Found {
		test.Fatal("branch fallback did not use injected projection reader", result, err)
	}
	if objects != 1 || projections != 1 || branches != 1 || broad.scopes != 0 || broad.reads != 0 {
		test.Fatal("injected snapshot and live readers lost their distinct paths", objects, projections, branches)
	}
}

func TestMemorySnapshotOnlyDispatchPreservesBranchAndEffectiveModes(test *testing.T) {
	for _, scenario := range []struct {
		name string
		args toolArgs
		want bool
	}{
		{"stored_branch", toolArgs{Mode: "stored", Ref: "main"}, true},
		{"exact_object", toolArgs{MemoryHash: string(pageHash(3))}, true},
		{"snapshot", toolArgs{Ref: string(pageHash(2))}, true},
		{"snapshot_project", toolArgs{Mode: "project", Ref: " " + string(pageHash(2)) + " "}, true},
		{"default_branch", toolArgs{}, false},
		{"named_branch", toolArgs{Ref: "main"}, false},
		{"project_branch", toolArgs{Mode: "project", Ref: "main"}, false},
		{"tag", toolArgs{Mode: "project", Ref: "release"}, false},
		{"short_hash", toolArgs{Ref: "abcdef"}, false},
		{"effective", toolArgs{Mode: "effective", Ref: string(pageHash(2)), MemoryHash: string(pageHash(3))}, false},
		{"invalid", toolArgs{Mode: "invalid", Ref: string(pageHash(2))}, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if actual := memorySnapshotOnly(scenario.args); actual != scenario.want {
				test.Fatal("snapshot dispatch changed branch/effective semantics", actual, scenario.want)
			}
		})
	}
}
