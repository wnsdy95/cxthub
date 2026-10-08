package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestConnectionChecksExistingRootBeforeRegistration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	mainTestGit(t, root, "init", "--template=", "-q", "-b", "main")
	var origin, repoID string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repository-connections":
			json.NewEncoder(w).Encode(domain.RepositoryConnection{RepoID: repoID, RemoteURL: origin, RepositoryID: "synthetic"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+repoID:
			// A legacy peer may identify the repository without supporting roots.
			json.NewEncoder(w).Encode(domain.Repo{ID: repoID, RemoteURL: origin, DefaultBranch: "main"})
		default:
			t.Error("incompatible root reached registration or another mutation", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	origin = server.URL + "/team/repository"
	repoID = remotecfg.RepoIDFor(origin)
	store := storage.NewFileStore(root)
	manifest, _, err := domain.ConversationManifestForCIR(domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutConversationManifest(ctx, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: hash, RepoID: repoID, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareRemoteConnection(config{RepoRoot: root, GitDir: filepath.Join(root, ".git")})(ctx, root, origin, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Connect(ctx); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatalf("root capability preflight: %v", err)
	}
	if requests != 2 {
		t.Fatalf("request count = %d, want resolve + capability check", requests)
	}
	if _, err := store.GetDocReference(ctx, snap.DocumentRef()); err != nil {
		t.Fatal("connection changed source", err)
	}
}
