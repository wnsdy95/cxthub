package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestReviewAdmissionOffBeforePendingSettings(t *testing.T) {
	ctx := context.Background()
	rep, bodies, _ := p4AppRoot(t)
	repo := domain.HashContent([]byte(t.Name()))
	store := storage.NewFileStore(t.TempDir())
	for h, b := range bodies {
		if err := store.PutChunk(ctx, h, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutConversationManifest(ctx, rep); err != nil {
		t.Fatal(err)
	}
	settings, err := store.PutSettingsObject(ctx, domain.SettingsBundle{Kind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo, Provider: domain.ProviderCodex, SessionID: "synthetic", ClaudeSettings: settings}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPending(ctx, domain.Pending{RepoID: repo, Provider: domain.ProviderCodex, SessionID: "synthetic", Target: rep.Hash}); err != nil {
		t.Fatal(err)
	}
	var settingWrites, otherWrites, negotiations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out any
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/manifest"):
			out = domain.Manifest{RepoID: repo}
		case r.Method == "GET":
			out = map[string]any{"id": repo, "required_doc_identity": rep.Identity, "doc_identities_supported": []domain.DocumentIdentity{rep.Identity}, "root_publication_enabled": false}
		case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
			negotiations.Add(1)
			var req struct {
				Snapshots []domain.ContentHash `json:"snapshot_haves"`
				Docs      []domain.ContentHash `json:"doc_haves"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			out = map[string]any{"snapshot_wants": req.Snapshots, "doc_wants": req.Docs, "doc_identities_supported": []domain.DocumentIdentity{rep.Identity}, "root_publication_enabled": false, "chunks_supported": true, "bounded_chunks_supported": true, "chunk_formats_supported": []string{domain.ConversationManifestChunkFormat}, "async_docs_supported": true}
		case strings.Contains(r.URL.Path, "/settings-objects/"):
			settingWrites.Add(1)
			out = map[string]any{}
		default:
			otherWrites.Add(1)
			http.Error(w, "unexpected write", 400)
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
	svc := NewSyncRepoService(store, client, nil, storage.NewSyncOutbox())
	n, err := svc.SyncPendings(ctx, inbound.SyncInput{RepoID: repo, PendingSessionID: "synthetic"}, nil)
	t.Logf("synced=%d err=%v negotiations=%d settings_writes=%d other_writes=%d", n, err, negotiations.Load(), settingWrites.Load(), otherWrites.Load())
	if err == nil || n != 0 {
		t.Fatal("expected new-root admission failure")
	}
	if settingWrites.Load() != 0 || otherWrites.Load() != 0 {
		t.Fatal("new-root admission failure occurred after a remote mutation")
	}
}
