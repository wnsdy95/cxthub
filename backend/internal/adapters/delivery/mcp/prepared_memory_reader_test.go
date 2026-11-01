package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type preparedMemoryContextOnly struct {
	ContextBackend
	getRepo func(context.Context, domain.ContentHash) (domain.Repo, error)
}

func (backend *preparedMemoryContextOnly) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return backend.getRepo(ctx, repo)
}

type preparedMemoryContextTrap struct {
	*preparedMemoryContextOnly
	scopes  int
	queries int
}

func (backend *preparedMemoryContextTrap) WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error {
	backend.scopes++
	return domain.ErrIntegrity
}

func (backend *preparedMemoryContextTrap) QueryPreparedMemory(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
	backend.queries++
	return domain.MemoryReadPage{}, domain.ErrIntegrity
}

func TestSetPreparedMemoryRejectsNilWithoutReplacingReader(test *testing.T) {
	var typedNil *scopedPreparedMemoryQuery
	var typedNilOwner *memoryquery.PreparedQueries
	for _, installed := range []bool{false, true} {
		test.Run(map[bool]string{false: "empty", true: "installed"}[installed], func(test *testing.T) {
			server := &Server{}
			reader := &scopedPreparedMemoryQuery{
				MemoryReadScope: preparedMemoryScopeFunc(func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
					return read(ctx)
				}),
				preparedMemoryQueryFunc: func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
					return domain.MemoryReadPage{}, nil
				},
			}
			if installed {
				if err := server.SetPreparedMemory(reader); err != nil {
					test.Fatal(err)
				}
			}
			for _, rejected := range []inbound.PreparedMemoryReader{nil, typedNil, typedNilOwner} {
				if err := server.SetPreparedMemory(rejected); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("nil prepared reader accepted", err)
				}
				if installed && server.preparedMemory != reader || !installed && server.preparedMemory != nil {
					test.Fatal("rejected prepared reader replaced the previous state")
				}
			}
			replacement := &scopedPreparedMemoryQuery{reader.MemoryReadScope, reader.preparedMemoryQueryFunc}
			if err := server.SetPreparedMemory(replacement); err != nil || server.preparedMemory != replacement {
				test.Fatal("valid prepared reader could not be installed after rejection", err)
			}
		})
	}
}

func TestPreparedMemoryMCPInjectedReaderOwnsScopeAndSerializedPage(test *testing.T) {
	for _, broadScope := range []bool{false, true} {
		for _, exit := range []struct {
			name string
			err  error
		}{
			{name: "ready"},
			{name: "forbidden", err: domain.ErrForbidden},
			{name: "canceled", err: context.Canceled},
		} {
			test.Run(map[bool]string{false: "no_broad_scope", true: "poisoned_broad_scope"}[broadScope]+"/"+exit.name, func(test *testing.T) {
				fixture := newMemoryAdmissionFixture(test)
				contextOnly := &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}
				broad := &preparedMemoryContextTrap{preparedMemoryContextOnly: contextOnly}
				fixture.server.context = contextOnly
				if broadScope {
					fixture.server.context = broad
				}
				version := 1
				args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), MemoryHash: string(fixture.memory), CodeCommit: strings.Repeat("a", 40)}
				items := []domain.EffectiveMemoryItem{{ID: pageHash(3), Text: "Serialized inside the injected scope"}}
				scopes, calls := 0, 0
				active, completed := false, false
				var readContext context.Context
				scope := preparedMemoryScopeFunc(func(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
					scopes++
					if repo != fixture.repo.ID {
						return domain.ErrForbidden
					}
					return fixture.backend.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
						active = true
						defer func() { active = false }()
						if err := read(bound); err != nil {
							return err
						}
						completed = true
						items[0].Text = "Mutation after the read callback must not leak"
						probe, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
						defer cancel()
						_, release, err := app.AcquireResources(probe, app.ResourceBulk)
						if release != nil {
							release()
						}
						if !errors.Is(err, context.DeadlineExceeded) {
							test.Fatal("injected permit released before serialized page callback completed", err)
						}
						return exit.err
					})
				})
				reader := preparedMemoryReader(scope, func(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
					calls++
					readContext = ctx
					actor, system := inbound.RepositoryActor(ctx)
					if _, limited := ctx.Deadline(); !limited || !active || actor != fixture.reader.ID || system || repo != fixture.repo.ID ||
						request.Selection.SnapshotID != fixture.snapshot.ID || inbound.DocumentReadAuthorizer(ctx) == nil {
						test.Fatal("injected prepared reader bypassed scoped admission")
					}
					return domain.MemoryReadPage{Cursor: "narrow-replay", Status: preparedMemoryTestStatus(repo), Page: domain.EffectiveMemoryPage{
						Selection: request.Selection, StateHash: pageHash(5), LineageHash: pageHash(6), Total: 1, Items: items}}, nil
				})
				if err := fixture.server.SetPreparedMemory(reader); err != nil {
					test.Fatal(err)
				}
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
				text, failed := memoryAdmissionResult(test, response)
				if scopes != 1 || calls != 1 || !completed || active || broad.scopes != 0 || broad.queries != 0 {
					test.Fatal("prepared MCP discovered broad capabilities or skipped the injected scope", scopes, calls, completed)
				}
				if readContext == nil || !errors.Is(readContext.Err(), context.Canceled) {
					test.Fatal("prepared MCP request deadline context leaked")
				}
				if exit.err != nil {
					var failure struct {
						Error preparedMemoryToolError `json:"error"`
					}
					if err := json.Unmarshal([]byte(text), &failure); err != nil || !failed || failure.Error.Code != exit.name ||
						strings.Contains(text, "json_fragment") || strings.Contains(text, "Serialized inside") {
						test.Fatal("failed scope returned a serialized prepared page", text, err)
					}
				} else {
					var page preparedMemoryTestPage
					if err := json.Unmarshal([]byte(text), &page); err != nil || failed || !page.Complete {
						test.Fatal("injected prepared MCP response failed", text, err)
					}
					var item domain.EffectiveMemoryItem
					if err := json.Unmarshal([]byte(page.Fragment), &item); err != nil || item.Text != "Serialized inside the injected scope" {
						test.Fatal("prepared page serialization escaped its scope", page.Fragment, err)
					}
				}
				probe, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err != nil {
					test.Fatal("prepared MCP scope leaked its resource permit", err)
				}
				release()
			})
		}
	}
}

func TestPreparedMemoryMCPInjectedScopeFailurePreventsRead(test *testing.T) {
	for _, scenario := range []struct {
		code      string
		err       error
		retryable bool
	}{
		{code: "forbidden", err: domain.ErrForbidden},
		{code: "resource_overloaded", err: domain.ErrResourceOverloaded, retryable: true},
		{code: "prepared_memory_unavailable", err: domain.ErrCatalogUnsupported},
	} {
		test.Run(scenario.code, func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			broad := &preparedMemoryContextTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
			fixture.server.context = broad
			scopes := 0
			scope := preparedMemoryScopeFunc(func(ctx context.Context, repo domain.ContentHash, _ func(context.Context) error) error {
				scopes++
				if _, limited := ctx.Deadline(); !limited || repo != fixture.repo.ID || inbound.DocumentReadAuthorizer(ctx) == nil {
					test.Fatal("prepared scope omitted the request deadline or authorization binding")
				}
				return scenario.err
			})
			reader := preparedMemoryReader(scope, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
				test.Fatal("rejected scope reached the prepared query")
				return domain.MemoryReadPage{}, nil
			})
			requirePreparedMemoryReader(test, fixture.server, reader)
			version := 1
			args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(context.Background(), args))
			text, failed := memoryAdmissionResult(test, response)
			var failure struct {
				Error preparedMemoryToolError `json:"error"`
			}
			if err := json.Unmarshal([]byte(text), &failure); err != nil || !failed || failure.Error.Code != scenario.code ||
				failure.Error.Retryable != scenario.retryable || failure.Error.RetryCursor != "" || strings.Contains(text, "json_fragment") {
				test.Fatal("injected scope failure changed prepared error mapping", text, err)
			}
			if scopes != 1 || broad.scopes != 0 || broad.queries != 0 || fixture.backend.reads.Load() != 0 {
				test.Fatal("failed injected scope reached broad admission or payload reads")
			}
		})
	}
}

func TestPreparedMemoryMCPMissingReaderDoesNotDiscoverBroadBackend(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	broad := &preparedMemoryContextTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
	fixture.server.context = broad
	version := 1
	args := toolArgs{Repository: string(fixture.repo.ID), Mode: "effective", ReadVersion: &version, Ref: string(fixture.snapshot.ID), CodeCommit: strings.Repeat("a", 40)}
	raw, err := json.Marshal(args)
	if err != nil {
		test.Fatal(err)
	}
	if result, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", raw); !errors.Is(err, domain.ErrCatalogUnsupported) || result != "" || broad.scopes != 0 || broad.queries != 0 {
		test.Fatal("missing injected reader discovered broad prepared capabilities", result, err)
	}
}

func TestPreparedMemoryMCPReaderDoesNotReplaceLegacyScope(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	broad := &preparedMemoryContextTrap{preparedMemoryContextOnly: &preparedMemoryContextOnly{ContextBackend: fixture.backend, getRepo: fixture.backend.GetRepo}}
	fixture.server.context = broad
	setLiveMemoryFixture(test, fixture.server, fixture.backend.Service, broad)
	scope := preparedMemoryScopeFunc(func(context.Context, domain.ContentHash, func(context.Context) error) error {
		test.Fatal("legacy memory mode entered the prepared scope")
		return nil
	})
	reader := preparedMemoryReader(scope, func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
		test.Fatal("legacy memory mode entered the prepared query")
		return domain.MemoryReadPage{}, nil
	})
	if err := fixture.server.SetPreparedMemory(reader); err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"", "project", "effective"} {
		test.Run(mode, func(test *testing.T) {
			args := toolArgs{Repository: string(fixture.repo.ID), Mode: mode, Ref: "main"}
			raw, err := json.Marshal(args)
			if err != nil {
				test.Fatal(err)
			}
			before := broad.scopes
			if result, err := fixture.server.runTool(context.Background(), fixture.reader, "memory_load", raw); !errors.Is(err, domain.ErrIntegrity) || result != "" || broad.scopes != before+1 || broad.queries != 0 {
				test.Fatal("legacy memory scope selection changed", result, err)
			}
		})
	}
}
