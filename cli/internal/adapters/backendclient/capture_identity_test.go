package backendclient

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRootCaptureCapabilityConfirmation(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic-capture-repo")))
	for _, mode := range []string{"valid", "old", "wrong-repo", "not-opted", "disabled", "duplicate", "unknown", "no-async", "no-chunks", "no-bounded", "no-v2", "negotiate-disabled", "negotiate-identity", "malformed", "unexpected-wants", "forbidden", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("X-Cxt-Doc-Identities") != string(domain.DocumentIdentityRootV1) {
					t.Error("lost auth or identity declaration")
				}
				if mode == "malformed" {
					w.Write([]byte(`{"id":`))
					return
				}
				if mode == "forbidden" {
					w.WriteHeader(403)
					return
				}
				if mode == "oversized" {
					w.Write(make([]byte, (1<<20)+1))
					return
				}
				ids := []string{"", string(domain.DocumentIdentityRootV1)}
				if mode == "duplicate" {
					ids = append(ids, string(domain.DocumentIdentityRootV1))
				}
				if mode == "unknown" {
					ids = append(ids, "unknown")
				}
				view := map[string]any{"id": repo, "required_doc_identity": string(domain.DocumentIdentityRootV1), "root_publication_enabled": true, "doc_identities_supported": ids}
				switch mode {
				case "old":
					view = map[string]any{"id": repo}
				case "wrong-repo":
					view["id"] = "other"
				case "not-opted":
					view["required_doc_identity"] = ""
				case "disabled":
					view["root_publication_enabled"] = false
				}
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+repo {
					json.NewEncoder(w).Encode(view)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/"+repo+"/push/negotiate" {
					t.Error("unexpected capability path")
					w.WriteHeader(500)
					return
				}
				var request negotiateReq
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if len(request.DocHaves)+len(request.SnapshotHaves)+len(request.ChunkHaves) != 0 {
					t.Error("probe offered objects")
				}
				view["async_docs_supported"] = mode != "no-async"
				view["chunks_supported"] = mode != "no-chunks"
				view["bounded_chunks_supported"] = mode != "no-bounded"
				view["chunk_formats_supported"] = []string{domain.ConversationManifestChunkFormat}
				if mode == "negotiate-disabled" {
					view["root_publication_enabled"] = false
				}
				if mode == "negotiate-identity" {
					delete(view, "doc_identities_supported")
				}
				if mode == "no-v2" {
					view["chunk_formats_supported"] = []string{"legacy"}
				}
				if mode == "unexpected-wants" {
					view["doc_wants"] = []string{repo}
				}
				json.NewEncoder(w).Encode(view)
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
			err := c.ConfirmRootCapture(context.Background(), repo)
			if mode == "valid" {
				if err != nil || calls != 2 {
					t.Fatal(calls, err)
				}
			} else if err == nil {
				t.Fatal("unsupported confirmation accepted")
			}
			before := calls
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := c.ConfirmRootCapture(ctx, repo); err == nil || calls != before {
				t.Fatal("canceled confirmation used network", err)
			}
		})
	}
}
