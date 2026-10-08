package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestP5RootCaptureConservativeRetention(t *testing.T) {
	for _, rootFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "old_root", false: "next_root"}[rootFirst], func(t *testing.T) {
			ctx := context.Background()
			s := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			root := rootFixture(t, "retained root")
			seedRootFS(t, s, repo, root)
			_, legacy, _ := docJobFixture(t, repo, "legacy capture")
			if _, err := s.PutVerifiedDoc(ctx, repo, legacy); err != nil {
				t.Fatal(err)
			}
			old, next := legacy.Hash(), root.hash
			if rootFirst {
				old, next = next, old
			}
			before := rootFSImage(t, s.dataDir)
			got, err := s.CaptureSupersedes(ctx, repo, old, next, domain.ProviderCodex, "session")
			if err != nil || got {
				t.Fatalf("root pair must conservatively retain: result=%v err=%v", got, err)
			}
			after := rootFSImage(t, s.dataDir)
			for path, value := range before {
				if after[path] != value {
					t.Fatal("capture comparison changed storage")
				}
			}
		})
	}
}

func p5RootJob(t *testing.T, repo domain.ContentHash, fixture rootReaderFixture) domain.DocFinalizationJob {
	t.Helper()
	job, err := domain.NewDocFinalizationJobForRepresentation(repo, domain.DocumentRepresentation{Hash: fixture.hash, Identity: domain.DocumentIdentityRootV1, RootManifest: fixture.manifestBytes}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestP5JobDependencyAndQueueHelpers(t *testing.T) {
	repo := domain.HashContent([]byte(t.Name()))
	for _, body := range []string{"", strings.Repeat("x", 3*domain.ConversationManifestChunkBytes)} {
		f := rootFixture(t, body)
		j := p5RootJob(t, repo, f)
		chunks, err := publicationJobChunks(j)
		if err != nil || len(chunks) != len(f.manifest.Chunks) {
			t.Fatal(chunks, err)
		}
		for i, h := range chunks {
			if h != f.manifest.Chunks[i].Hash {
				t.Fatal("ordered occurrence lost")
			}
		}
		queue := fsDocQueue{pending: map[string]domain.DocFinalizationJob{}}
		queue.record(j)
		summary := queue.pending[j.ID]
		if summary.RootManifest != nil || len(summary.Manifest.Chunks) != 0 || summary.Manifest.Envelope != nil || summary.ID != j.ID {
			t.Fatal("queue retains a manifest or drops identity")
		}
		j.RootManifest = []byte(`{"identity":null}`)
		if _, err := publicationJobChunks(j); err == nil {
			t.Fatal("unknown closure accepted")
		}
	}
}

func TestP5PendingRootRetentionAfterReload(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	_, legacy, _ := docJobFixture(t, repo, "legacy collector control")
	if _, err := s.PutVerifiedDoc(ctx, repo, legacy); err != nil {
		t.Fatal(err)
	}
	f := rootFixture(t, strings.Repeat("x", 3*domain.ConversationManifestChunkBytes))
	j := p5RootJob(t, repo, f)
	old := time.Now().Add(-time.Hour)
	for hash, body := range f.bodies {
		if err := writeAtomic(s.chunkPath(repo, hash), docCompress(body)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(s.chunkPath(repo, hash), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	fsDocQueues.Delete(s.dataDir) // This private fixture simulates a queue restart.
	reopened := NewFSStore(s.dataDir)
	claim, err := reopened.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil || len(claim.RootManifest) == 0 {
		t.Fatal(claim, err)
	}
	queue := reopened.docQueue()
	queue.Lock()
	summary := queue.pending[j.ID]
	queue.Unlock()
	if summary.RootManifest != nil || summary.Manifest.Envelope != nil || len(summary.Manifest.Chunks) != 0 {
		t.Fatal("reloaded queue retains payload")
	}
	if _, _, err := reopened.RepackDocs(); err != nil {
		t.Fatal(err)
	}
	for hash, body := range f.bodies {
		got, err := reopened.GetChunk(ctx, repo, hash)
		if err != nil || string(got) != string(body) {
			t.Fatal("pending dependency collected", err)
		}
	}
	// An unreadable durable pending job cannot authorize any sweep, including an
	// unrelated aged orphan. Restore exactly the prior claim before continuing.
	orphan := []byte("unrelated orphan protected by failed mark phase")
	orphanHash := domain.HashContent(orphan)
	if err := writeAtomic(s.chunkPath(repo, orphanHash), docCompress(orphan)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.chunkPath(repo, orphanHash), old, old); err != nil {
		t.Fatal(err)
	}
	jobPath := s.docJobPath(repo, claim.ID)
	jobBytes, err := os.ReadFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	var bad map[string]json.RawMessage
	if err := json.Unmarshal(jobBytes, &bad); err != nil {
		t.Fatal(err)
	}
	bad["root_manifest"] = json.RawMessage(`{"identity":null}`)
	badBytes, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(jobPath, badBytes); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.RepackDocs(); err == nil {
		t.Fatal("malformed pending closure allowed sweep")
	}
	if _, err := os.Stat(s.chunkPath(repo, orphanHash)); err != nil {
		t.Fatal("failed mark swept an orphan", err)
	}
	if err := writeAtomic(jobPath, jobBytes); err != nil {
		t.Fatal(err)
	}

	// Only the existing collector runs after the final pending reference releases.
	claim.State = "rejected"
	queue.Lock()
	err = reopened.writeDocJob(claim)
	queue.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.RepackDocs(); err != nil {
		t.Fatal(err)
	}
	for hash := range f.bodies {
		if _, err := os.Stat(s.chunkPath(repo, hash)); !os.IsNotExist(err) {
			t.Fatal("released orphan not swept", err)
		}
	}
	if _, err := reopened.GetDoc(ctx, repo, legacy.Hash()); err != nil {
		t.Fatal("legacy live doc damaged", err)
	}
}
