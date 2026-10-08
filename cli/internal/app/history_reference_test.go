package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func historyRootSnapshot(t *testing.T, store *storage.FileStore, repo, text string, parents ...domain.ContentHash) (domain.Snapshot, domain.ConversationManifest) {
	t.Helper()
	ctx := context.Background()
	cir := domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CIRVersion: "1"}, Events: []domain.Event{{Seq: 1, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for hash, body := range bodies {
		if err := store.PutChunk(ctx, hash, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutConversationManifest(ctx, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: hash, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, RepoID: repo, Branch: "main", Parents: parents}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	return snap, manifest
}

type historyReferenceCounter struct {
	*storage.FileStore
	refs        []domain.DocumentRef
	getSnapshot func(domain.Snapshot) domain.Snapshot
}

func (s *historyReferenceCounter) GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	snap, err := s.FileStore.GetSnapshot(ctx, id)
	if err == nil && s.getSnapshot != nil {
		snap = s.getSnapshot(snap)
	}
	return snap, err
}

func (s *historyReferenceCounter) VerifyStoredDoc(ctx context.Context, hash domain.ContentHash) error {
	s.refs = append(s.refs, domain.DocumentRef{Hash: hash})
	return s.FileStore.VerifyStoredDoc(ctx, hash)
}

func (s *historyReferenceCounter) VerifyStoredDocReference(ctx context.Context, ref domain.DocumentRef) error {
	s.refs = append(s.refs, ref)
	return s.FileStore.VerifyStoredDocReference(ctx, ref)
}

func TestHistorySourceReferenceRootClosureAndOperationCache(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	owner, _ := historyRootSnapshot(t, store, repo, "root memory owner")
	memory, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner.ID, Summary: "inherited current root proof"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapSnapshotMemory(ctx, owner.ID, "", memory); err != nil {
		t.Fatal(err)
	}
	target, _ := historyRootSnapshot(t, store, repo, "root history target", owner.ID)
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: "root-history", Kind: "position", Source: target.ID, Target: target.ID, SharedTarget: target.ID, CreatedAt: time.Unix(1, 0).UTC()}
	counter := &historyReferenceCounter{FileStore: store}
	svc := NewContextHistoryService(counter, store)
	for operation := 1; operation <= 2; operation++ {
		got, err := svc.ValidateHistorySource(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if got.MemoryHash != memory || got.MemorySource != owner.ID || !got.MemoryPinned {
			t.Fatal("lost verified inherited memory", got)
		}
		if len(counter.refs) != operation*2 {
			t.Fatal("expected one proof per exact reference per operation", counter.refs)
		}
		for i, ref := range []domain.DocumentRef{target.DocumentRef(), owner.DocumentRef()} {
			if counter.refs[(operation-1)*2+i] != ref {
				t.Fatal("hash-only or reordered proof", counter.refs)
			}
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.ValidateHistorySource(canceled, e); !errors.Is(err, context.Canceled) || len(counter.refs) != 4 {
		t.Fatal("canceled operation made proof", err, counter.refs)
	}
}

// Deliberately vary adapter metadata between reads of one snapshot. A cached
// proof for the same hash under another scheme must never cover that read.
func TestHistorySourceReferenceCacheDoesNotCoverDifferentScheme(t *testing.T) {
	for _, next := range []domain.DocumentIdentity{domain.DocumentIdentityRootV1, "unknown"} {
		t.Run(string(next), func(t *testing.T) {
			store, e, _ := historyVerificationFixture(t, 1024)
			counter := &historyReferenceCounter{FileStore: store}
			reads := 0
			counter.getSnapshot = func(snap domain.Snapshot) domain.Snapshot {
				reads++
				if reads > 1 {
					snap.DocIdentity = next
				}
				return snap
			}
			if _, err := NewContextHistoryService(counter, store).ValidateHistorySource(context.Background(), e); err == nil {
				t.Fatal("hash-only cached proof covered changed scheme")
			}
			if next == domain.DocumentIdentityRootV1 && (len(counter.refs) != 2 || counter.refs[1].Identity != next) {
				t.Fatal("changed exact reference was not verified", counter.refs)
			}
		})
	}
}

func TestHistorySourceReferenceWarmRootRejectsCorruptionAndScheme(t *testing.T) {
	for _, mode := range []string{"changed-chunk", "missing-chunk", "stripped-scheme", "unknown-scheme"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			store := storage.NewFileStore(root)
			store.EnableDocVerificationCache(filepath.Join(t.TempDir(), "private", "key"))
			repo := domain.HashContent([]byte(t.Name()))
			snap, manifest := historyRootSnapshot(t, store, repo, "warm root source")
			e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: "root-history", Kind: "position", Source: snap.ID, Target: snap.ID, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
			svc := NewContextHistoryService(store, store)
			if _, err := svc.ValidateHistorySource(ctx, e); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".cxt", "objects", "chunks", strings.TrimPrefix(string(manifest.Chunks[0].Hash), "sha256:"))
			switch mode {
			case "changed-chunk":
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				bad := bytes.Clone(raw)
				bad[len(bad)/2] ^= 1
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "missing-chunk":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			default:
				if mode == "stripped-scheme" {
					snap.DocIdentity = domain.DocumentIdentityLegacy
				}
				raw, err := json.Marshal(snap)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unknown-scheme" {
					// The typed writer rejects unknown schemes; corrupt bytes directly.
					raw = bytes.Replace(raw, []byte(domain.DocumentIdentityRootV1), []byte("unknown"), 1)
				}
				if err := os.WriteFile(filepath.Join(root, ".cxt", "objects", "snapshots", strings.TrimPrefix(string(snap.ID), "sha256:")), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.ValidateHistorySource(ctx, e); err == nil {
				t.Fatal("prior proof hid current corruption or scheme mismatch")
			}
		})
	}
}
