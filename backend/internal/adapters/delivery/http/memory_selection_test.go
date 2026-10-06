package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memorySelectionHTTPBackend struct {
	Backend
	repo  domain.Repo
	calls []domain.HistoryEvent
	err   error
}

func (b *memorySelectionHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return b.repo, nil
}
func (b *memorySelectionHTTPBackend) RecordHistory(_ context.Context, e domain.HistoryEvent) error {
	b.calls = append(b.calls, e)
	return b.err
}

func TestMemorySelectionHTTPBoundary(t *testing.T) {
	for _, name := range []string{"member", "unauthenticated", "puller", "missing-parent", "wrong-repo", "application-conflict", "ordinary-history"} {
		t.Run(name, func(t *testing.T) {
			b := &memorySelectionHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("memory route")), RepositoryID: "ws_" + strings.Repeat("a", 32)}}
			id := branchPullHTTPIdentity{repo: domain.Repository{ID: b.repo.RepositoryID}, role: domain.RoleMember}
			e := domain.HistoryEvent{RepoID: string(b.repo.ID), ID: strings.Repeat("b", 32), MemorySelectionParent: strings.Repeat("c", 32)}
			path := "/api/v1/repos/" + string(b.repo.ID) + "/history/memory-selection"
			authed, status, calls := true, 200, 1
			switch name {
			case "unauthenticated":
				authed, status, calls = false, 401, 0
			case "puller":
				id.role, status, calls = domain.RolePuller, 403, 0
			case "missing-parent":
				e.MemorySelectionParent, status, calls = "", 422, 0
			case "wrong-repo":
				e.RepoID, status, calls = string(domain.HashContent([]byte("other"))), 422, 0
			case "application-conflict":
				b.err, status = domain.ErrConflict, 409
			case "ordinary-history":
				e.MemorySelectionParent = ""
				path = strings.TrimSuffix(path, "/memory-selection")
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodPost, path, string(raw), authed)
			if w.Code != status || len(b.calls) != calls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, len(b.calls), w.Body.String())
			}
			if calls == 1 && (b.calls[0].ID != e.ID || b.calls[0].MemorySelectionParent != e.MemorySelectionParent) {
				t.Fatal("HTTP handler stripped immutable selection evidence")
			}
		})
	}
}
