package main

import (
	"context"
	"encoding/json"
	"errors"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCaptureIdentityCommandConfirmsThenOrdinaryHookCaptureIsOffline(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	mainTestGit(t, root, "init", "--template=", "-q", "-b", "main")
	mainTestGit(t, root, "-c", "core.hooksPath=/dev/null", "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "synthetic")
	var origin string
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer capture-fixture" || r.Header.Get("X-Cxt-Doc-Identities") != string(domain.DocumentIdentityRootV1) {
			t.Error("lost scoped auth/declaration")
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+remotecfg.RepoIDFor(origin) {
			json.NewEncoder(w).Encode(map[string]any{"id": remotecfg.RepoIDFor(origin), "required_doc_identity": domain.DocumentIdentityRootV1, "root_publication_enabled": true, "doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/push/negotiate") {
			json.NewEncoder(w).Encode(map[string]any{"root_publication_enabled": true, "doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, "async_docs_supported": true, "chunks_supported": true, "bounded_chunks_supported": true, "chunk_formats_supported": []string{domain.ConversationManifestChunkFormat}})
			return
		}
		t.Error("unexpected request", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	defer server.Close()
	origin = server.URL + "/team/repo"
	if err := configFixtureSave(root, remotecfg.Remotes{"origin": origin}); err != nil {
		t.Fatal(err)
	}
	cfg := config{RepoRoot: root, GitDir: filepath.Join(root, ".git"), GitBranch: "main", GitCommit: configGitValue(root, "rev-parse", "--verify", "HEAD"), RemoteEndpoint: server.URL + "/api/v1", RemoteToken: "capture-fixture"}
	ctr := buildContainer(cfg)
	if err := delivcli.Run(ctr.clictr, []string{"cxt", "config", "capture.identity", string(domain.DocumentIdentityRootV1)}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected capability calls", calls.Load())
	}
	// An offline hook must consume the local preference, not poll the server.
	server.Close()
	session := filepath.Join(root, "synthetic.jsonl")
	raw := `{"type":"session_meta","payload":{"id":"root-hook-synthetic","cwd":"` + root + `","model":"fixture"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"offline capture"}]}}` + "\n"
	if err := os.WriteFile(session, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := ctr.clictr.Save.Save(ctx, inbound.SaveInput{Cwd: root, Provider: domain.ProviderCodex, SessionPath: session, Message: domain.HookMessagePrefix + " test", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewFileStore(root)
	snap, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil || snap.DocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal("composed capture remained legacy", snap.DocIdentity, err)
	}
	if _, err := store.GetDocReference(ctx, snap.DocumentRef()); err != nil {
		t.Fatal(err)
	}
	if err := delivcli.Run(ctr.clictr, []string{"cxt", "config", "capture.identity", "legacy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDocReference(ctx, snap.DocumentRef()); err != nil {
		t.Fatal("clearing preference damaged existing root", err)
	}
	if calls.Load() != 2 {
		t.Fatal("offline operations contacted server")
	}
}

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
