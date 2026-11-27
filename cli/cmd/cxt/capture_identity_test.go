package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestCaptureIdentityCommandFailureAndConfigRaceNeverEnable(t *testing.T) {
	for _, mode := range []string{"disabled", "config-race", "origin-race", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			ctx := context.Background()
			var origin string
			var calls atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("confirmation retargeted after observation")
				w.WriteHeader(500)
			}))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == http.MethodGet {
					if mode == "config-race" {
						if err := remotecfg.SetSecretsRedact(ctx, root, "concurrent"); err != nil {
							t.Error(err)
						}
					}
					if mode == "origin-race" {
						if err := configFixtureSave(root, remotecfg.Remotes{"origin": other.URL + "/team/other"}); err != nil {
							t.Error(err)
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"id": remotecfg.RepoIDFor(origin), "required_doc_identity": domain.DocumentIdentityRootV1, "root_publication_enabled": mode != "disabled", "doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"root_publication_enabled": true, "doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, "async_docs_supported": true, "chunks_supported": true, "bounded_chunks_supported": true, "chunk_formats_supported": []string{domain.ConversationManifestChunkFormat}})
			}))
			defer server.Close()
			origin = server.URL + "/team/repo"
			if err := configFixtureSave(root, remotecfg.Remotes{"origin": origin}); err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			cfg := config{RepoRoot: root, RemoteEndpoint: server.URL + "/api/v1", RemoteToken: "synthetic"}
			err := setCaptureIdentity(cfg)(ctx, root, domain.DocumentIdentityRootV1)
			if err == nil {
				t.Fatal("failed confirmation published preference")
			}
			if (mode == "config-race" || mode == "origin-race") && !errors.Is(err, remotecfg.ErrChanged) {
				t.Fatal(err)
			}
			if mode == "cancel" && (!errors.Is(err, context.Canceled) || calls.Load() != 0) {
				t.Fatal(err, calls.Load())
			}
			if got, err := remotecfg.CaptureIdentity(context.Background(), root); err != nil || got != "" {
				t.Fatal("root preference survived failed confirmation", got, err)
			}
			if mode == "config-race" && remotecfg.SecretsRedact(root) != "concurrent" {
				t.Fatal("race overwrote concurrent setting")
			}
		})
	}
}
