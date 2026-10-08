package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestLocalMCPRootFetchChecksCurrentSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := storage.NewFileStore(dir)
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events:   []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Synthetic root transcript"}}}},
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for hash, body := range bodies {
		if err := store.PutChunk(ctx, hash, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutConversationManifest(ctx, domain.DocumentRepresentation{Hash: root, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: root, RepoID: "repo-1", Branch: "main", DocHash: root, DocIdentity: domain.DocumentIdentityRootV1}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{RepoID: snap.RepoID, Kind: domain.RefBranch, Name: "main", Target: root}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(fakeGit{}, store, nil)
	text, err := server.toolFetch(ctx, dir, domain.Repo{ID: snap.RepoID}, "main", 12)
	if err != nil || !strings.Contains(text, "Synthetic root transcript") {
		t.Fatalf("root fetch: %q %v", text, err)
	}
	if err := os.Remove(filepath.Join(dir, ".cxt", "objects", "chunks", strings.TrimPrefix(string(manifest.Chunks[0].Hash), "sha256:"))); err != nil {
		t.Fatal(err)
	}
	text, err = server.toolFetch(ctx, dir, domain.Repo{ID: snap.RepoID}, "main", 12)
	if err == nil || text != "" {
		t.Fatalf("warm fetch hid missing source: %q %v", text, err)
	}
}
