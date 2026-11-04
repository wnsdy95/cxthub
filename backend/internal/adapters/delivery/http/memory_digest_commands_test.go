package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type memoryDigestHTTPBackend struct{ *repositoryRefHTTPBackend }

func (*memoryDigestHTTPBackend) PutMemoryDigest(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error) {
	panic("memory command reached broad HTTP backend")
}

func (*memoryDigestHTTPBackend) PutMemoryDigestCAS(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error) {
	panic("memory CAS reached broad HTTP backend")
}

type memoryDigestHTTPScope struct {
	test  *testing.T
	calls []string
	err   error
}

func (scope *memoryDigestHTTPScope) check(ctx context.Context, stage string) {
	scope.test.Helper()
	if actor, system := inbound.RepositoryActor(ctx); actor != "actor" || system {
		scope.test.Fatal("memory command lost authenticated actor", actor, system)
	}
	scope.calls = append(scope.calls, stage)
}

func (scope *memoryDigestHTTPScope) WithinMemoryObjectWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error) {
	scope.check(ctx, "object")
	return write(ctx)
}

func (scope *memoryDigestHTTPScope) WithinMemoryAttachmentWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	scope.check(ctx, "attachment")
	if scope.err != nil {
		return scope.err
	}
	return write(ctx)
}

func TestMemoryDigestHTTPActualOwnerAndWireContract(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		endpoint string
		typed    bool
		invalid  bool
		role     domain.MemberRole
		err      error
		status   int
		writes   bool
	}{
		{name: "legacy", endpoint: "memories", role: domain.RoleMember, status: http.StatusOK, writes: true},
		{name: "causal", endpoint: "memory-attachments", role: domain.RoleMember, status: http.StatusOK, writes: true},
		{name: "typed", endpoint: "typed-memory-attachments", typed: true, role: domain.RoleMember, status: http.StatusOK, writes: true},
		{name: "legacy rejects typed", endpoint: "memories", typed: true, role: domain.RoleMember, status: http.StatusUnprocessableEntity},
		{name: "causal rejects typed", endpoint: "memory-attachments", typed: true, role: domain.RoleMember, status: http.StatusUnprocessableEntity},
		{name: "typed rejects legacy", endpoint: "typed-memory-attachments", role: domain.RoleMember, status: http.StatusUnprocessableEntity},
		{name: "decode before command", endpoint: "memory-attachments", invalid: true, role: domain.RoleMember, status: http.StatusBadRequest},
		{name: "viewer before decode", endpoint: "memory-attachments", invalid: true, role: domain.RoleViewer, status: http.StatusForbidden},
		{name: "current authority failure", endpoint: "memory-attachments", role: domain.RoleMember, err: domain.ErrForbidden, status: http.StatusForbidden, writes: true},
		{name: "CAS conflict", endpoint: "typed-memory-attachments", typed: true, role: domain.RoleMember, err: domain.ErrConflict, status: http.StatusConflict, writes: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			repo, target := domain.HashContent([]byte(test.Name())), domain.HashContent([]byte("HTTP memory target"))
			backend := &memoryDigestHTTPBackend{&repositoryRefHTTPBackend{repo: domain.Repo{ID: repo, RepositoryID: "ws_" + strings.Repeat("d", 32)}}}
			identity := &branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: scenario.role}
			server := NewServer(backend, identity)
			storage := store.NewFSStore(test.TempDir())
			if err := storage.PutSnapshot(test.Context(), domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
				test.Fatal(err)
			}
			scope := &memoryDigestHTTPScope{test: test, err: scenario.err}
			commands, err := app.NewMemoryDigestCommands(app.MemoryDigestCommandsConfig{Blobs: storage, Snapshots: storage, Scope: scope})
			if err != nil {
				test.Fatal(err)
			}
			if err := server.SetMemoryDigestCommands(commands); err != nil {
				test.Fatal(err)
			}
			digest := domain.MemoryDigest{SnapshotID: domain.HashContent([]byte("ignored body target")), Summary: "unchanged memory"}
			if scenario.typed {
				digest.ClaimsVersion = 1
				digest.Fragments = []domain.MemoryFragment{{SourceSnapshot: target, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "Keep provenance"}}}}
			}
			encoded, err := json.Marshal(digest)
			if err != nil {
				test.Fatal(err)
			}
			body := string(encoded)
			if scenario.invalid {
				body = "{"
			}
			response := bpHTTP(test, server.Handler(), http.MethodPut, "/api/v1/repos/"+string(repo)+"/"+scenario.endpoint+"/"+string(target), body, true)
			if response.Code != scenario.status {
				test.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if scenario.writes {
				if !reflect.DeepEqual(scope.calls, []string{"object", "attachment"}) {
					test.Fatal("HTTP did not consume actual owner", scope.calls)
				}
			} else if len(scope.calls) != 0 {
				test.Fatal("invalid request reached writer", scope.calls)
			}
			current, err := storage.GetSnapshot(test.Context(), repo, target)
			if err != nil {
				test.Fatal(err)
			}
			if scenario.status == http.StatusOK {
				digest.SnapshotID = target
				want, err := domain.MemoryDigestHash(digest)
				if err != nil || current.MemoryHash != want || !strings.Contains(response.Body.String(), string(want)) {
					test.Fatal("HTTP changed target, payload or result", current, err, response.Body.String())
				}
			} else if current.MemoryHash != "" {
				test.Fatal("rejected request changed pointer", current)
			}
		})
	}
}

func TestMemoryDigestHTTPRejectsNilCommands(test *testing.T) {
	server := &Server{}
	for _, commands := range []app.MemoryDigestWriter{nil, (*app.MemoryDigestCommands)(nil)} {
		if err := server.SetMemoryDigestCommands(commands); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("nil memory digest commands accepted", err)
		}
	}
}
