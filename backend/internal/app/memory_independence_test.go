package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Exercise the real archive, memory chunking and pointer stores together. File
// identity catches even a rewrite/reindex that happens to produce equal bytes.
func TestMemoryOnlyRecompressionPreservesArchiveAndSelection(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		name := "existing indexes"
		if !indexed {
			name = "missing indexes"
		}
		t.Run(name, func(t *testing.T) {
			ctx := systemTestContext()
			rootDir := t.TempDir()
			st := store.NewFSStore(rootDir)
			svc := NewService(st, st, nil, nil, st)
			repo := hh(t.Name())
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			parent := reuseSnapshot(t, svc, repo, "earlier conversation")
			cir := pendingGCCIR(domain.ProviderCodex, "original user request", strings.Repeat("immutable conversation evidence ", 40000))
			raw, err := domain.CanonicalBytes(cir)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.HashContent(raw)
			plan, ok := domain.PlanDocChunks(raw)
			if !ok || len(plan.Manifest.Chunks) < 2 {
				t.Fatal("fixture must contain multiple conversation chunks")
			}
			if _, err := st.PutDoc(ctx, repo, domain.SessionDoc{Hash: id, CIR: cir}); err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Parents: []domain.ContentHash{parent}, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, SessionID: cir.Envelope.SessionOriginID, Branch: "main", Message: "archived conversation"}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			original := domain.MemoryDigest{
				SnapshotID: id, Provider: domain.ProviderCodex, ClaimsVersion: domain.MemoryClaimsVersion,
				Summary:  strings.Repeat("verbose independently authored memory ", 3000),
				KeyFacts: []string{"retain source attribution"}, OpenTasks: []string{"verify the independent memory"},
				Fragments: []domain.MemoryFragment{
					{SourceSnapshot: parent, Summary: "inherited decision", Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "The original transcript is evidence."}}},
					{SourceSnapshot: id, Summary: "current decision", TasksAuthoritative: true, OpenTasks: []string{"verify the independent memory"}, Claims: []domain.MemoryClaim{{Kind: "decision", Text: "Recompress memory separately."}}},
				},
				GraftCoverage: &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, ProjectionComplete: true, LineageFingerprint: hh("observed memory lineage"), GraftSeq: 2, GraftParents: []domain.ContentHash{parent}, PinnedSources: []domain.ContentHash{parent, id}},
			}
			if _, chunked, err := domain.PlanMemoryChunks(original); err != nil || !chunked {
				t.Fatalf("fixture must contain chunked memory: %v", err)
			}
			previous, err := svc.PutMemoryDigestCAS(ctx, repo, original)
			if err != nil {
				t.Fatal(err)
			}
			event := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "position", Source: id, Target: id, MemorySource: id, MemoryHash: previous, MemoryPinned: true, WorktreeID: strings.Repeat("2", 32), GitAfter: strings.Repeat("a", 40), CreatedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
			if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: event.BranchID, Target: id}, ""); err != nil {
				t.Fatal(err)
			}
			if err := svc.RecordHistory(ctx, event); err != nil {
				t.Fatal(err)
			}
			history, err := svc.ListHistory(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			refs, err := st.ListRefs(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			snapshots, err := st.ListSnapshots(ctx, repo, "")
			if err != nil {
				t.Fatal(err)
			}
			repoDir := filepath.Join(rootDir, "repos", strings.TrimPrefix(string(repo), "sha256:"))
			if !indexed {
				// Missing derived indexes must not be rebuilt by a memory update.
				if err := os.RemoveAll(filepath.Join(repoDir, "read-index-v2")); err != nil {
					t.Fatal(err)
				}
			}
			before := memoryIndependenceFiles(t, repoDir)
			indexFiles := 0
			for path := range before {
				if strings.HasPrefix(path, "read-index-") {
					indexFiles++
				}
			}
			if indexed != (indexFiles > 0) {
				t.Fatalf("fixture indexed=%t, found %d index files", indexed, indexFiles)
			}
			guard := &memoryIndependenceStore{FSStore: st, t: t}
			svc.blobs, svc.meta = guard, guard

			recompressed := original
			recompressed.PreviousMemoryHash, recompressed.Summary = previous, "Keep the conversation immutable; version memory independently."
			if _, chunked, err := domain.PlanMemoryChunks(recompressed); err != nil || chunked {
				t.Fatalf("recompressed fixture should fit in one memory object: %v", err)
			}
			next, err := svc.PutMemoryDigestCAS(ctx, repo, recompressed)
			if err != nil || next == previous {
				t.Fatalf("recompression did not create a new version: %s %v", next, err)
			}
			if retry, err := svc.PutMemoryDigestCAS(ctx, repo, recompressed); err != nil || retry != next {
				t.Fatalf("exact retry: %s %v", retry, err)
			}
			for hash, want := range map[domain.ContentHash]domain.MemoryDigest{previous: original, next: recompressed} {
				got, err := svc.GetMemoryObject(ctx, repo, hash)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("memory version/provenance changed at %s: %v", hash, err)
				}
			}
			if got, err := svc.GetMemoryDigest(ctx, repo, id); err != nil || !reflect.DeepEqual(got, recompressed) {
				t.Fatalf("current memory did not advance: %v", err)
			}
			// A saved selection resolves the original hash even after the mutable
			// attachment advances; no new history event or branch movement occurs.
			selected, err := svc.QueryEffectiveMemory(ctx, repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: id, MemoryHash: event.MemoryHash, CodeCommit: event.GitAfter}, Content: "prompt", Limit: 50})
			if err != nil || selected.LineageHash != previous {
				t.Fatalf("saved memory selection moved: %s %v", selected.LineageHash, err)
			}
			if got, err := svc.ListHistory(ctx, repo); err != nil || !reflect.DeepEqual(got, history) {
				t.Fatalf("memory update changed accepted history: %v", err)
			}
			if got, err := st.ListRefs(ctx, repo); err != nil || !reflect.DeepEqual(got, refs) {
				t.Fatalf("memory update moved refs: %v", err)
			}
			for i := range snapshots {
				if snapshots[i].ID == id {
					snapshots[i].MemoryHash = next
				}
			}
			if got, err := st.ListSnapshots(ctx, repo, ""); err != nil || !reflect.DeepEqual(got, snapshots) {
				t.Fatalf("memory update changed snapshot fields beyond MemoryHash: %v", err)
			}
			after := memoryIndependenceFiles(t, repoDir)
			for path, old := range before {
				current, exists := after[path]
				if !exists || !os.SameFile(old.info, current.info) || !old.info.ModTime().Equal(current.info.ModTime()) || !bytes.Equal(old.body, current.body) {
					t.Errorf("memory update rewrote or removed immutable object/index %s", path)
				}
			}
			for path := range after {
				if _, exists := before[path]; !exists && !strings.HasPrefix(path, "objects/memories/") && !strings.HasPrefix(path, "objects/memory_chunks/") {
					t.Errorf("memory update created conversation object/index %s", path)
				}
			}
			// Inspect storage directly so the update guard remains active.
			doc, err := st.GetDoc(ctx, repo, id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := domain.CanonicalBytes(doc.CIR)
			if err != nil || !bytes.Equal(got, raw) || domain.HashContent(got) != id {
				t.Fatalf("archived conversation changed: %v", err)
			}
			if manifest, err := st.GetDocManifest(ctx, repo, id); err != nil || !reflect.DeepEqual(manifest, plan.Manifest) {
				t.Fatalf("conversation chunk manifest changed: %v", err)
			}
			for hash, body := range plan.Bodies {
				if got, err := st.GetChunk(ctx, repo, hash); err != nil || !bytes.Equal(got, body) {
					t.Fatalf("conversation chunk %s changed: %v", hash, err)
				}
			}
		})
	}
}

type memoryIndependenceFile struct {
	info fs.FileInfo
	body []byte
}

func memoryIndependenceFiles(t *testing.T, repoDir string) map[string]memoryIndependenceFile {
	t.Helper()
	files := map[string]memoryIndependenceFile{}
	err := filepath.WalkDir(repoDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "objects/") && !strings.HasPrefix(rel, "read-index-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		files[rel] = memoryIndependenceFile{info, body}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Retain the adapter's optional interfaces while making expensive archive
// reads/publication/indexing fail if the memory path accidentally invokes them.
type memoryIndependenceStore struct {
	*store.FSStore
	t *testing.T
}

func (s *memoryIndependenceStore) unexpected(operation string) error {
	s.t.Helper()
	s.t.Errorf("memory-only operation called %s", operation)
	return domain.ErrIntegrity
}
func (s *memoryIndependenceStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, s.unexpected("GetDoc")
}
func (s *memoryIndependenceStore) PutDoc(context.Context, domain.ContentHash, domain.SessionDoc) (bool, error) {
	return false, s.unexpected("PutDoc")
}
func (s *memoryIndependenceStore) PutVerifiedDoc(context.Context, domain.ContentHash, domain.VerifiedSessionDoc) (bool, error) {
	return false, s.unexpected("PutVerifiedDoc")
}
func (s *memoryIndependenceStore) VerifyStoredDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, error) {
	return domain.VerifiedDocReference{}, s.unexpected("VerifyStoredDoc")
}
func (s *memoryIndependenceStore) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	return domain.DocReadIndex{}, s.unexpected("DocReadIndex")
}
func (s *memoryIndependenceStore) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, s.unexpected("SearchDocEvents")
}
func (s *memoryIndependenceStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	return domain.DocChunkManifest{}, s.unexpected("GetDocManifest")
}
func (s *memoryIndependenceStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	return nil, s.unexpected("GetChunk")
}
func (s *memoryIndependenceStore) PutChunks(context.Context, domain.ContentHash, map[domain.ContentHash][]byte) (int, int, error) {
	return 0, 0, s.unexpected("PutChunks")
}

// Hold both real writes immediately before pointer CAS. This proves a common
// prior version cannot silently accept two concurrent recompressions.
type memoryIndependenceCASBarrier struct {
	*store.FSStore
	arrived chan struct{}
	release chan struct{}
}

func (s *memoryIndependenceCASBarrier) CompareAndSwapSnapshotMemory(ctx context.Context, repo, id, previous, next domain.ContentHash) error {
	s.arrived <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return s.FSStore.CompareAndSwapSnapshotMemory(ctx, repo, id, previous, next)
	}
}

func TestMemoryOnlyConcurrentRecompressionRetainsBothVersions(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	repo := hh(t.Name())
	id := reuseSnapshot(t, svc, repo, "original conversation")
	original := domain.MemoryDigest{SnapshotID: id, Provider: domain.ProviderCodex, Summary: "original memory", Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Summary: "source provenance"}}}
	previous, err := svc.PutMemoryDigestCAS(ctx, repo, original)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &memoryIndependenceCASBarrier{FSStore: st, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	svc.meta = barrier
	type result struct {
		hash domain.ContentHash
		err  error
	}
	results := make(chan result, 2)
	candidates := map[domain.ContentHash]domain.MemoryDigest{}
	for _, summary := range []string{"first recompression", "second recompression"} {
		next := original
		next.PreviousMemoryHash, next.Summary = previous, summary
		hash, err := domain.MemoryDigestHash(next)
		if err != nil {
			t.Fatal(err)
		}
		candidates[hash] = next
		go func() {
			hash, err := svc.PutMemoryDigestCAS(ctx, repo, next)
			results <- result{hash, err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.arrived:
		case <-ctx.Done():
			t.Fatal("concurrent writers did not both reach CAS")
		}
	}
	close(barrier.release)
	var winner domain.ContentHash
	conflicts := 0
	for i := 0; i < 2; i++ {
		out := <-results
		if errors.Is(out.err, domain.ErrConflict) {
			conflicts++
		} else if out.err != nil {
			t.Fatal(out.err)
		} else if winner != "" {
			t.Fatal("both writes from the same previous version succeeded")
		} else {
			winner = out.hash
		}
	}
	if winner == "" || conflicts != 1 {
		t.Fatalf("winner=%s conflicts=%d", winner, conflicts)
	}
	if got, err := st.GetSnapshot(ctx, repo, id); err != nil || got.MemoryHash != winner {
		t.Fatalf("pointer did not retain the winning CAS: %+v %v", got, err)
	}
	candidates[previous] = original
	for hash, want := range candidates {
		got, err := svc.GetMemoryObject(ctx, repo, hash)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("original or concurrent candidate lost its content/provenance: %s %v", hash, err)
		}
	}
}
