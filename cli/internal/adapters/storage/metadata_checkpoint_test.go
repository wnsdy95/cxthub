package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func metadataCheckpointFixture() outbound.MetadataCheckpoint {
	hash := func(label string) domain.ContentHash { return domain.HashContent([]byte(label)) }
	repo := hash("metadata checkpoint repository")
	return outbound.MetadataCheckpoint{
		Version: 1, RepoID: repo, Remote: "https://checkpoint.example.test",
		Snapshots: []domain.Snapshot{{
			ID: hash("fetched document"), RepoID: repo, DocHash: hash("fetched document"),
			Branch: "main", Parents: []domain.ContentHash{hash("parent")},
			MemoryHash: hash("memory"), ClaudeSettings: hash("claude settings"),
			AgentsSettings: hash("agents settings"), CodexSettings: hash("codex settings"),
			Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "Fetched metadata",
			Author:    domain.TeamIdentity{Name: "Synthetic Author", Email: "author@example.test", Team: "test"},
			CreatedAt: time.Date(2026, time.October, 1, 2, 3, 4, 0, time.UTC),
			Grafted:   true, GraftParents: []domain.ContentHash{hash("graft parent")}, GraftSeq: 2,
			SessionID: "synthetic-session", Models: []string{"synthetic-model"}, CompactionCount: 1,
		}},
	}
}

func metadataCheckpointBytes(t *testing.T, value outbound.MetadataCheckpoint) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMetadataCheckpointRoundTripAndCanonicalRevision(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	next := metadataCheckpointFixture()
	missing, err := store.ReadMetadataCheckpoint(ctx, next.RepoID, next.Remote)
	if err != nil || !reflect.DeepEqual(missing, outbound.MetadataCheckpoint{Version: 1, RepoID: next.RepoID, Remote: next.Remote}) {
		t.Fatalf("absent checkpoint = %+v, %v", missing, err)
	}
	if _, err := os.Stat(store.storeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent read created state: %v", err)
	}
	second := next.Snapshots[0]
	second.ID = domain.HashContent([]byte("another fetched document"))
	second.DocHash = second.ID
	next.Snapshots = append(next.Snapshots, second)
	sort.Slice(next.Snapshots, func(i, j int) bool { return next.Snapshots[i].ID > next.Snapshots[j].ID })
	next.Revision = domain.HashContent([]byte("ignored caller revision"))
	before := metadataCheckpointBytes(t, next)
	if _, err := store.CompareAndSwapMetadataCheckpoint(ctx, next.Revision, next); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("nonempty expected revision created absent checkpoint: %v", err)
	}
	saved, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", next)
	if err != nil {
		t.Fatal(err)
	}
	if domain.ValidateContentHash(saved.Revision) != nil || saved.Revision == next.Revision || saved.Snapshots[0].ID >= saved.Snapshots[1].ID {
		t.Fatalf("noncanonical checkpoint: %+v", saved)
	}
	if !bytes.Equal(before, metadataCheckpointBytes(t, next)) {
		t.Fatal("CAS mutated the caller's checkpoint")
	}
	// The checksum excludes the revision field, including an empty revision key.
	snapshots, err := json.Marshal(saved.Snapshots)
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"version":1,"repo_id":%q,"remote":%q,"snapshots":%s}`, saved.RepoID, saved.Remote, snapshots)
	if want := domain.HashContent([]byte(payload)); saved.Revision != want {
		t.Fatalf("revision = %s, want checksum of full payload %s", saved.Revision, want)
	}
	got, err := NewFileStore(store.repoRoot).ReadMetadataCheckpoint(ctx, next.RepoID, next.Remote)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("reopened checkpoint = %+v, %v; want %+v", got, err, saved)
	}
	if _, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", next); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale CAS = %v", err)
	}
	reordered, err := store.CompareAndSwapMetadataCheckpoint(ctx, saved.Revision, next)
	if err != nil || !reflect.DeepEqual(reordered, saved) {
		t.Fatalf("snapshot order changed checksum: %+v, %v", reordered, err)
	}
	next.Snapshots[0].MemoryHash = domain.HashContent([]byte("new fetched memory"))
	updated, err := store.CompareAndSwapMetadataCheckpoint(ctx, saved.Revision, next)
	if err != nil || updated.Revision == saved.Revision {
		t.Fatalf("mutable metadata did not change revision: %+v, %v", updated, err)
	}
	got, err = store.ReadMetadataCheckpoint(ctx, next.RepoID, next.Remote)
	if err != nil || !reflect.DeepEqual(got, updated) {
		t.Fatalf("updated checkpoint = %+v, %v", got, err)
	}
	for _, snap := range next.Snapshots {
		if _, err := store.GetSnapshot(ctx, snap.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("fetched metadata became an applied snapshot: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(store.storeDir(), "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint created verified content objects: %v", err)
	}
	path, err := store.metadataCheckpointPath(next.RepoID, next.Remote)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.HashContent([]byte(next.RepoID + "\x00" + next.Remote))
	if path != filepath.Join(store.storeDir(), "metadata-checkpoints", hexOf(key)+".json") {
		t.Fatalf("unexpected checkpoint namespace/key: %s", path)
	}
}

func TestMetadataCheckpointEmptySnapshotCanonicalization(t *testing.T) {
	store := NewFileStore(t.TempDir())
	value := metadataCheckpointFixture()
	value.Snapshots = nil
	saved, err := store.CompareAndSwapMetadataCheckpoint(context.Background(), "", value)
	if err != nil || saved.Revision == "" || saved.Snapshots == nil {
		t.Fatalf("empty persisted checkpoint = %+v, %v", saved, err)
	}
	value.Snapshots = []domain.Snapshot{}
	got, err := store.CompareAndSwapMetadataCheckpoint(context.Background(), saved.Revision, value)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("nil and empty lists differ: %+v, %v", got, err)
	}
}

func TestMetadataCheckpointTamperIsNeverRepaired(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	saved, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", metadataCheckpointFixture())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.metadataCheckpointPath(saved.RepoID, saved.Remote)
	if err != nil {
		t.Fatal(err)
	}
	original := metadataCheckpointBytes(t, saved)
	// Every snapshot metadata field participates, even fields outside identity.
	for _, field := range []string{"id", "repo_id", "doc_hash", "branch", "parents", "memory_hash", "claude_settings", "agents_settings", "codex_settings", "provider", "fidelity", "message", "author", "created_at", "grafted", "graft_parents", "graft_seq", "session_id", "models", "compaction_count"} {
		t.Run(field, func(t *testing.T) {
			var record map[string]json.RawMessage
			if err := json.Unmarshal(original, &record); err != nil {
				t.Fatal(err)
			}
			var snapshots []map[string]json.RawMessage
			if err := json.Unmarshal(record["snapshots"], &snapshots); err != nil {
				t.Fatal(err)
			}
			delete(snapshots[0], field)
			record["snapshots"], err = json.Marshal(snapshots)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			assertMetadataCheckpointCorrupt(t, store, path, raw, saved)
		})
	}
	for name, raw := range map[string][]byte{
		"malformed JSON":         []byte(`{"version":`),
		"trailing JSON":          append(append([]byte{}, original...), []byte(` {}`)...),
		"unknown field":          bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"extra":true`), 1),
		"unknown snapshot field": bytes.Replace(original, []byte(`"branch":"main"`), []byte(`"branch":"main","extra":true`), 1),
		"empty revision":         bytes.Replace(original, []byte(saved.Revision), nil, 1),
		"wrong revision":         bytes.Replace(original, []byte(saved.Revision), []byte(domain.HashContent([]byte("wrong revision"))), 1),
		"changed message":        bytes.Replace(original, []byte("Fetched metadata"), []byte("Tampered metadata"), 1),
	} {
		t.Run(name, func(t *testing.T) { assertMetadataCheckpointCorrupt(t, store, path, raw, saved) })
	}
}

func assertMetadataCheckpointCorrupt(t *testing.T, store *FileStore, path string, raw []byte, next outbound.MetadataCheckpoint) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadMetadataCheckpoint(context.Background(), next.RepoID, next.Remote); !errors.Is(err, domain.ErrHashMismatch) || !reflect.DeepEqual(got, outbound.MetadataCheckpoint{}) {
		t.Fatalf("corrupt read = %+v, %v", got, err)
	}
	for _, expected := range []domain.ContentHash{"", next.Revision} {
		if _, err := store.CompareAndSwapMetadataCheckpoint(context.Background(), expected, next); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("CAS repaired corrupt record: %v", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("corrupt bytes changed: %v", err)
	}
}

func TestMetadataCheckpointValidatesMetadataOnReadAndWrite(t *testing.T) {
	other := domain.HashContent([]byte("different valid hash"))
	cases := map[string]func(*outbound.MetadataCheckpoint){
		"version":            func(v *outbound.MetadataCheckpoint) { v.Version = 2 },
		"repo":               func(v *outbound.MetadataCheckpoint) { v.RepoID = "../repo" },
		"remote":             func(v *outbound.MetadataCheckpoint) { v.Remote = "" },
		"foreign snapshot":   func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].RepoID = other },
		"duplicate snapshot": func(v *outbound.MetadataCheckpoint) { v.Snapshots = append(v.Snapshots, v.Snapshots[0]) },
		"id":                 func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].ID = "invalid" },
		"doc hash":           func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].DocHash = "invalid" },
		"id doc mismatch":    func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].DocHash = other },
		"memory":             func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].MemoryHash = "invalid" },
		"claude settings":    func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].ClaudeSettings = "invalid" },
		"agents settings":    func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].AgentsSettings = "invalid" },
		"codex settings":     func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].CodexSettings = "invalid" },
		"parent":             func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].Parents = []domain.ContentHash{"invalid"} },
		"graft parent":       func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].GraftParents = []domain.ContentHash{"invalid"} },
		"graft sequence":     func(v *outbound.MetadataCheckpoint) { v.Snapshots[0].GraftSeq = domain.MaxGraftSeq + 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			valid := metadataCheckpointFixture()
			invalid := metadataCheckpointFixture()
			mutate(&invalid)
			if _, err := store.CompareAndSwapMetadataCheckpoint(context.Background(), "", invalid); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("invalid metadata written: %v", err)
			}
			if _, err := os.Stat(store.storeDir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid write created state: %v", err)
			}
			// A correct checksum cannot make malformed or foreign metadata valid.
			invalid, err := canonicalMetadataCheckpoint(invalid)
			if err != nil {
				t.Fatal(err)
			}
			path, err := store.metadataCheckpointPath(valid.RepoID, valid.Remote)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			assertMetadataCheckpointCorrupt(t, store, path, metadataCheckpointBytes(t, invalid), valid)
		})
	}
}

func TestMetadataCheckpointRepositoryAndEndpointIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	first := metadataCheckpointFixture()
	saved, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", first)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct{ repo, remote string }{
		{first.RepoID, "https://other.example.test"},
		{domain.HashContent([]byte("other repository")), first.Remote},
	} {
		t.Run(scope.repo+scope.remote, func(t *testing.T) {
			got, err := store.ReadMetadataCheckpoint(ctx, scope.repo, scope.remote)
			if err != nil || got.Revision != "" || len(got.Snapshots) != 0 || got.RepoID != scope.repo || got.Remote != scope.remote {
				t.Fatalf("scope leaked: %+v, %v", got, err)
			}
			if _, err := store.CompareAndSwapMetadataCheckpoint(ctx, saved.Revision, got); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("foreign revision accepted: %v", err)
			}
			other, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", got)
			if err != nil || other.Revision == saved.Revision {
				t.Fatalf("independent scope = %+v, %v", other, err)
			}
			path, err := store.metadataCheckpointPath(scope.repo, scope.remote)
			if err != nil {
				t.Fatal(err)
			}
			assertMetadataCheckpointCorrupt(t, store, path, metadataCheckpointBytes(t, saved), other)
		})
	}
	got, err := store.ReadMetadataCheckpoint(ctx, first.RepoID, first.Remote)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("other scopes replaced original checkpoint: %+v, %v", got, err)
	}
	for _, scope := range []struct{ repo, remote string }{{"../repo", first.Remote}, {first.RepoID, ""}} {
		if _, err := store.ReadMetadataCheckpoint(ctx, scope.repo, scope.remote); err == nil {
			t.Fatalf("invalid scope accepted: %+v", scope)
		}
	}
}

func TestMetadataCheckpointConcurrentCASHasExactlyOneWinner(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			ctx := context.Background()
			store := NewFileStore(t.TempDir())
			var expected domain.ContentHash
			if existing {
				initial, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", metadataCheckpointFixture())
				if err != nil {
					t.Fatal(err)
				}
				expected = initial.Revision
			}
			const writers = 12
			type result struct {
				value outbound.MetadataCheckpoint
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, writers)
			for i := range writers {
				go func() {
					next := metadataCheckpointFixture()
					next.Snapshots[0].Message = fmt.Sprintf("writer %d", i)
					<-start
					value, err := NewFileStore(store.repoRoot).CompareAndSwapMetadataCheckpoint(ctx, expected, next)
					results <- result{value, err}
				}()
			}
			close(start)
			var winner outbound.MetadataCheckpoint
			wins, conflicts := 0, 0
			for range writers {
				out := <-results
				if out.err == nil {
					wins++
					winner = out.value
				} else if errors.Is(out.err, domain.ErrSyncConflict) {
					conflicts++
				} else {
					t.Errorf("unexpected CAS error: %v", out.err)
				}
			}
			if wins != 1 || conflicts != writers-1 {
				t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
			}
			got, err := store.ReadMetadataCheckpoint(ctx, winner.RepoID, winner.Remote)
			if err != nil || !reflect.DeepEqual(got, winner) {
				t.Fatalf("winner lost: %+v, %v; want %+v", got, err, winner)
			}
		})
	}
}

func TestMetadataCheckpointCancellation(t *testing.T) {
	store := NewFileStore(t.TempDir())
	next := metadataCheckpointFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadMetadataCheckpoint(ctx, next.RepoID, next.Remote); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v", err)
	}
	if _, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", next); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	if _, err := os.Stat(store.storeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled operation created state: %v", err)
	}
	saved, err := store.CompareAndSwapMetadataCheckpoint(context.Background(), "", next)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.metadataCheckpointPath(next.RepoID, next.Remote)
	if err != nil {
		t.Fatal(err)
	}
	next.Snapshots[0].Message = "must not be written"
	err = store.withMutationLock(context.Background(), metadataCheckpointNamespace, filepath.Base(path), func() error {
		blocked, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer stop()
		if _, err := NewFileStore(store.repoRoot).CompareAndSwapMetadataCheckpoint(blocked, saved.Revision, next); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("cancellation while waiting for lock = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadMetadataCheckpoint(context.Background(), next.RepoID, next.Remote)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("canceled writer changed checkpoint: %+v, %v", got, err)
	}
}

func TestMetadataCheckpointRejectsSymlinks(t *testing.T) {
	for _, location := range []string{"record", "dangling record", "namespace", ".cxt", "lock"} {
		t.Run(location, func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			next := metadataCheckpointFixture()
			path, err := store.metadataCheckpointPath(next.RepoID, next.Remote)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			target := filepath.Join(outside, "sentinel")
			const sentinel = "outside must remain unchanged"
			if err := os.WriteFile(target, []byte(sentinel), 0600); err != nil {
				t.Fatal(err)
			}
			link := path
			switch location {
			case "dangling record":
				target = filepath.Join(outside, "absent")
			case "namespace":
				link, target = filepath.Dir(path), outside
			case ".cxt":
				link, target = store.storeDir(), outside
			case "lock":
				link = filepath.Join(store.storeDir(), "locks", metadataCheckpointNamespace, filepath.Base(path)+".flock")
			}
			if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if location != "lock" {
				if _, err := store.ReadMetadataCheckpoint(context.Background(), next.RepoID, next.Remote); !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("symlink read = %v", err)
				}
			}
			if _, err := store.CompareAndSwapMetadataCheckpoint(context.Background(), "", next); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("symlink write = %v", err)
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("symlink replaced: %s, %v", got, err)
			}
			raw, err := os.ReadFile(filepath.Join(outside, "sentinel"))
			if err != nil || string(raw) != sentinel {
				t.Fatalf("outside file changed: %q, %v", raw, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("write escaped store: %v, %v", entries, err)
			}
		})
	}
}

func TestMetadataCheckpointLeavesVerifiedRemoteObservationUntouched(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	next := metadataCheckpointFixture()
	verified := outbound.RemoteObservation{
		Version: 1, RepoID: next.RepoID, Remote: next.Remote,
		Refs: []domain.Ref{{RepoID: next.RepoID, Kind: domain.RefBranch, Name: "main", Target: domain.HashContent([]byte("verified target"))}},
	}
	if err := store.CompareAndSwapRemoteObservation(ctx, "", verified); err != nil {
		t.Fatal(err)
	}
	verified, err := store.ReadRemoteObservation(ctx, next.RepoID, next.Remote)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.remoteObservationPath(next.RepoID, next.Remote)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadMetadataCheckpoint(ctx, next.RepoID, next.Remote)
	if err != nil || missing.Revision != "" || len(missing.Snapshots) != 0 {
		t.Fatalf("checkpoint inherited verified state: %+v, %v", missing, err)
	}
	if _, err := store.CompareAndSwapMetadataCheckpoint(ctx, verified.Revision, next); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("verified revision accepted as checkpoint revision: %v", err)
	}
	saved, err := store.CompareAndSwapMetadataCheckpoint(ctx, "", next)
	if err != nil {
		t.Fatal(err)
	}
	next.Snapshots[0].Message = "next fetched metadata"
	if _, err := store.CompareAndSwapMetadataCheckpoint(ctx, saved.Revision, next); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadRemoteObservation(ctx, next.RepoID, next.Remote)
	if err != nil || !reflect.DeepEqual(got, verified) {
		t.Fatalf("checkpoint changed verified observation: %+v, %v", got, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("verified observation bytes changed: %v", err)
	}
}
