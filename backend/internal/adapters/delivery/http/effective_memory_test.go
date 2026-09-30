package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type effectiveMemoryQueryFunc func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error)

func (f effectiveMemoryQueryFunc) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	return f(ctx, repo, request)
}

func TestEffectiveMemoryHTTPStaleCursorContract(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	api := NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st))
	api.SetEffectiveMemory(svc)
	handler := api.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()
	var me struct{ Username string }
	if doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me) != http.StatusOK {
		t.Fatal("login")
	}
	var record struct{ Slug string }
	if doJSON(t, http.MethodPost, ts.URL+"/api/v1/repositories", map[string]any{"name": "MemoryCursor"}, &record) != http.StatusOK {
		t.Fatal("repository")
	}
	remote := "http://cxthub.test/" + me.Username + "/" + record.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if doJSON(t, http.MethodPost, ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil) != http.StatusOK {
		t.Fatal("repo")
	}
	ctx := systemTestContext()
	snapshot := domain.HashContent([]byte("synthetic cursor fixture"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapshot, RepoID: repo, DocHash: snapshot}); err != nil {
		t.Fatal(err)
	}
	digest := domain.MemoryDigest{SnapshotID: snapshot, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: snapshot, Claims: []domain.MemoryClaim{{Kind: "decision", Text: "Keep the selected context"}, {Kind: "rationale", Text: "Preserve every observed page"}}}}}
	if _, err := svc.PutMemoryDigestCAS(ctx, repo, digest); err != nil {
		t.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, CodeCommit: strings.Repeat("a", 40)}, Content: "prompt", Limit: 1}
	base := "/api/v1/repos/" + url.PathEscape(string(repo)) + "/effective-memory"
	query := url.Values{"snapshot_id": {string(snapshot)}, "code_commit": {request.Selection.CodeCommit}, "content": {request.Content}, "limit": {"1"}}
	checkError := func(t *testing.T, token, cursor string, wantStatus int, wantCode string) {
		t.Helper()
		query.Set("cursor", cursor)
		req := httptest.NewRequest(http.MethodGet, base+"?"+query.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		var envelope struct{ Error struct{ Code string } }
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if response.Code != wantStatus || envelope.Error.Code != wantCode {
			t.Fatalf("got %d %s, want %d %s", response.Code, response.Body.String(), wantStatus, wantCode)
		}
	}
	for _, mode := range []string{"pending", "graph", "evidence", "malformed", "invalid_version", "invalid_offset"} {
		t.Run(mode, func(t *testing.T) {
			request.Cursor = ""
			query.Del("cursor")
			var first domain.EffectiveMemoryPage
			if status := doJSON(t, http.MethodGet, ts.URL+base+"?"+query.Encode(), nil, &first); status != http.StatusOK || first.NextCursor == "" {
				t.Fatalf("first page: %d %+v", status, first)
			}
			request.Cursor = first.NextCursor
			var err error
			switch mode {
			case "pending", "graph":
				err = st.AdvanceRepositoryRevision(ctx, repo, mode == "pending")
			case "evidence":
				err = st.AdvanceEvidenceRevision(ctx, repo)
			case "malformed":
				request.Cursor = "%%%"
			case "invalid_version", "invalid_offset":
				version, offset := 1, first.Total+1
				if mode == "invalid_version" {
					version, offset = 2, 1
				}
				raw, marshalErr := json.Marshal(map[string]any{"v": version, "state": first.StateHash, "index": offset})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				request.Cursor = base64.RawURLEncoding.EncodeToString(raw)
			}
			if err != nil {
				t.Fatal(err)
			}
			next, err := svc.QueryEffectiveMemory(ctx, repo, request)
			if mode == "pending" {
				if err != nil || next.StateHash != first.StateHash || len(next.Items) != 1 || next.Items[0].ID == first.Items[0].ID {
					t.Fatalf("pending invalidated cursor: %+v %v", next, err)
				}
				checkError(t, "dev:test@t.io:Test", request.Cursor, http.StatusOK, "")
				return
			}
			if mode == "graph" || mode == "evidence" {
				if !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) || !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("stale cursor lost typed conflict: %v", err)
				}
				checkError(t, "dev:test@t.io:Test", request.Cursor, http.StatusConflict, "memory_cursor_stale")
				checkError(t, "dev:outsider@example.test:Other", request.Cursor, http.StatusForbidden, "forbidden")
				return
			}
			if !errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
				t.Fatalf("malformed cursor became stale: %v", err)
			}
			checkError(t, "dev:test@t.io:Test", request.Cursor, http.StatusUnprocessableEntity, "validation")
		})
	}
	for _, tc := range []struct {
		name, code string
		err        error
		status     int
	}{
		{"wrapped_stale", "memory_cursor_stale", fmt.Errorf("read failed: %w", domain.ErrEffectiveMemoryCursorStale), http.StatusConflict},
		{"generic_conflict", "conflict", fmt.Errorf("%w: effective memory changed; restart without cursor", domain.ErrConflict), http.StatusConflict},
		{"resource_limit", "memory_projection_limit", fmt.Errorf("read failed: %w", domain.ErrMemoryProjectionLimit), http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api.SetEffectiveMemory(effectiveMemoryQueryFunc(func(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				return domain.EffectiveMemoryPage{}, tc.err
			}))
			checkError(t, "dev:test@t.io:Test", "page-two", tc.status, tc.code)
		})
	}
}
