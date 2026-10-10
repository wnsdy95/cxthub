package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type sessionArchiveTestStore interface {
	outbound.SessionArchiveStore
	outbound.MetadataStore
	outbound.BlobStore
	outbound.RepositoryRevisions
}

func TestFSSessionArchiveCannotReferenceDeletedCapture(t *testing.T) {
	ctx := context.Background()
	storage := NewFSStore(t.TempDir())
	record := sessionArchiveFixture(t, ctx, storage, domain.HashContent([]byte(t.Name())), domain.ProviderCodex, "worker", "capture")
	if err := storage.DeleteSnapshot(ctx, record.RepoID, record.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSessionArchive(ctx, record); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("archive of collected capture succeeded: %v", err)
	}
	archives, err := storage.ListSessionArchives(ctx, record.RepoID)
	if err != nil || len(archives) != 0 {
		t.Fatalf("dangling archive: %+v %v", archives, err)
	}
}

func sessionArchiveFixture(t *testing.T, ctx context.Context, st sessionArchiveTestStore, repo domain.ContentHash, provider domain.ProviderKind, session, text string) domain.SessionArchive {
	t.Helper()
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: provider, SessionOriginID: session, Fidelity: domain.FidelityFull},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, CIR: cir}); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, Provider: provider, SessionID: session, Fidelity: domain.FidelityFull, Branch: "main"}
	if err := st.PutSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	return domain.SessionArchive{
		RepoID: repo, Key: domain.SessionArchiveKey(snapshot), SnapshotID: hash,
		Provider: provider, SessionID: session, ArchivedAt: time.Now().UTC().Truncate(time.Microsecond), ArchivedBy: "dev:maintainer",
	}
}

func requireSessionArchive(t *testing.T, got, want domain.SessionArchive) {
	t.Helper()
	got.ArchivedAt = got.ArchivedAt.UTC()
	want.ArchivedAt = want.ArchivedAt.UTC()
	if got != want {
		t.Fatalf("archive mismatch: got %+v, want %+v", got, want)
	}
}

func checkSessionArchivePersistence(t *testing.T, ctx context.Context, st sessionArchiveTestStore) {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	otherRepo := domain.HashContent([]byte(string(repo) + "other"))
	first := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "same-session", "first capture")
	retry := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, first.SessionID, "later capture")
	retry.ArchivedAt = first.ArchivedAt.Add(time.Hour)
	retry.ArchivedBy = "dev:owner"
	other := sessionArchiveFixture(t, ctx, st, otherRepo, first.Provider, first.SessionID, "first capture")
	provider := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderClaude, first.SessionID, "claude capture")
	anonymous := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderUnknown, "", "anonymous capture")
	otherAnonymous := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderUnknown, "", "another anonymous capture")
	if first.Key != retry.Key || first.Key != other.Key || first.SnapshotID != other.SnapshotID || first.Key == provider.Key || anonymous.Key == otherAnonymous.Key {
		t.Fatal("fixture does not exercise session, provider, snapshot, and repository identities")
	}
	before, err := st.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := st.ListSessionArchives(ctx, repo); err != nil || records == nil || len(records) != 0 {
		t.Fatalf("empty archive list: %+v %v", records, err)
	}
	for _, record := range []domain.SessionArchive{first, retry, other, provider, anonymous, otherAnonymous} {
		if err := st.PutSessionArchive(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	want := []domain.SessionArchive{first, provider, anonymous, otherAnonymous}
	sort.Slice(want, func(left, right int) bool { return want[left].Key < want[right].Key })
	for attempt := 0; attempt < 2; attempt++ {
		records, err := st.ListSessionArchives(ctx, repo)
		if err != nil || len(records) != len(want) {
			t.Fatalf("archive list: %+v %v", records, err)
		}
		for index, record := range records {
			requireSessionArchive(t, record, want[index])
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := st.DeleteSessionArchive(ctx, repo, first.Key); err != nil {
			t.Fatal(err)
		}
	}
	records, err := st.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != len(want)-1 {
		t.Fatalf("restore did not delete exactly one archive: %+v %v", records, err)
	}
	for _, record := range records {
		if record.Key == first.Key {
			t.Fatal("restored archive still listed")
		}
	}
	records, err = st.ListSessionArchives(ctx, otherRepo)
	if err != nil || len(records) != 1 {
		t.Fatalf("restore affected another repository: %+v %v", records, err)
	}
	requireSessionArchive(t, records[0], other)
	for _, record := range []domain.SessionArchive{first, retry, other, provider, anonymous, otherAnonymous} {
		if snapshot, err := st.GetSnapshot(ctx, record.RepoID, record.SnapshotID); err != nil || snapshot.ID != record.SnapshotID {
			t.Fatalf("archive operation changed snapshot: %+v %v", snapshot, err)
		}
		if doc, err := st.GetDoc(ctx, record.RepoID, record.SnapshotID); err != nil || doc.Hash != record.SnapshotID {
			t.Fatalf("archive operation changed document: %+v %v", doc, err)
		}
	}
	if err := st.PutSessionArchive(ctx, retry); err != nil {
		t.Fatal(err)
	}
	records, err = st.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != len(want) {
		t.Fatalf("rearchive list: %+v %v", records, err)
	}
	for _, record := range records {
		if record.Key == retry.Key {
			requireSessionArchive(t, record, retry)
		}
	}
	if after, err := st.RepositoryRevision(ctx, repo); err != nil || before != after {
		t.Fatalf("persistence independently changed revision: before=%+v after=%+v err=%v", before, after, err)
	}
}

func checkSessionArchiveValidation(t *testing.T, ctx context.Context, st sessionArchiveTestStore) {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	valid := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "validation")
	for _, test := range []struct {
		name   string
		change func(*domain.SessionArchive)
	}{
		{"repo hash", func(record *domain.SessionArchive) { record.RepoID = "../outside" }},
		{"key hash", func(record *domain.SessionArchive) { record.Key = "../outside" }},
		{"snapshot hash", func(record *domain.SessionArchive) { record.SnapshotID = "../outside" }},
		{"wrong key", func(record *domain.SessionArchive) { record.Key = domain.HashContent([]byte("wrong")) }},
		{"wrong provider", func(record *domain.SessionArchive) { record.Provider = domain.ProviderClaude }},
		{"wrong session", func(record *domain.SessionArchive) { record.SessionID += "changed" }},
		{"missing time", func(record *domain.SessionArchive) { record.ArchivedAt = time.Time{} }},
		{"missing actor", func(record *domain.SessionArchive) { record.ArchivedBy = " \t\n" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := valid
			test.change(&record)
			if err := st.PutSessionArchive(ctx, record); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	for _, invalid := range []domain.ContentHash{"", "../outside", domain.ContentHash("sha256:" + strings.Repeat("A", 64))} {
		if records, err := st.ListSessionArchives(ctx, invalid); !errors.Is(err, domain.ErrIntegrity) || records != nil {
			t.Fatalf("invalid repo list: %+v %v", records, err)
		}
		if err := st.DeleteSessionArchive(ctx, invalid, valid.Key); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("invalid delete repo: %v", err)
		}
		if err := st.DeleteSessionArchive(ctx, repo, invalid); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("invalid delete key: %v", err)
		}
	}
	if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 0 {
		t.Fatalf("invalid insert persisted: %+v %v", records, err)
	}
	missingRepo := domain.HashContent([]byte(string(repo) + "missing"))
	if err := st.DeleteSessionArchive(ctx, missingRepo, valid.Key); err != nil {
		t.Fatalf("missing archive restore: %v", err)
	}
	if records, err := st.ListSessionArchives(ctx, missingRepo); err != nil || records == nil || len(records) != 0 {
		t.Fatalf("missing repo list: %+v %v", records, err)
	}
}

func TestFSSessionArchivePersistence(t *testing.T) {
	checkSessionArchivePersistence(t, context.Background(), NewFSStore(t.TempDir()))
}

func TestFSSessionArchiveValidation(t *testing.T) {
	checkSessionArchiveValidation(t, context.Background(), NewFSStore(t.TempDir()))
}

func TestFSSessionArchiveRestartAndSafeFilename(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "../../outside/\\session", "safe archive")
	if err := st.PutSessionArchive(ctx, record); err != nil {
		t.Fatal(err)
	}
	path := st.sessionArchivePath(repo, record.Key)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != hexOf(record.Key)+".json" {
		t.Fatalf("unsafe archive filename or leaked temporary file: %+v %v", entries, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewFSStore(st.dataDir)
	retry := record
	retry.ArchivedBy = "dev:another-maintainer"
	retry.ArchivedAt = retry.ArchivedAt.Add(time.Hour)
	if err := reopened.PutSessionArchive(ctx, retry); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("retry rewrote original archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-abandoned"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	records, err := reopened.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 1 {
		t.Fatalf("restarted list: %+v %v", records, err)
	}
	requireSessionArchive(t, records[0], record)
	if err := reopened.DeleteSessionArchive(ctx, repo, record.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore did not remove archive: %v", err)
	}
}

func TestFSSessionArchiveMalformedReadsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		raw    string
		change func(*domain.SessionArchive)
	}{
		{name: "invalid JSON", raw: "{"},
		{name: "null", raw: "null"},
		{name: "wrong JSON type", raw: "[]"},
		{name: "foreign repository", change: func(record *domain.SessionArchive) { record.RepoID = domain.HashContent([]byte("foreign")) }},
		{name: "filename identity", change: func(record *domain.SessionArchive) {
			record.SessionID = "another-session"
			record.Key = domain.SessionArchiveKey(domain.Snapshot{ID: record.SnapshotID, Provider: record.Provider, SessionID: record.SessionID})
		}},
		{name: "key identity", change: func(record *domain.SessionArchive) { record.SessionID += "changed" }},
		{name: "invalid hash", change: func(record *domain.SessionArchive) { record.SnapshotID = "../outside" }},
		{name: "missing attribution", change: func(record *domain.SessionArchive) { record.ArchivedBy = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "corruption")
			if err := st.PutSessionArchive(ctx, record); err != nil {
				t.Fatal(err)
			}
			raw := []byte(test.raw)
			if test.change != nil {
				broken := record
				test.change(&broken)
				var err error
				raw, err = json.Marshal(broken)
				if err != nil {
					t.Fatal(err)
				}
			}
			path := st.sessionArchivePath(repo, record.Key)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if records, err := st.ListSessionArchives(ctx, repo); !errors.Is(err, domain.ErrIntegrity) || records != nil {
				t.Fatalf("malformed archive list did not fail closed: %+v %v", records, err)
			}
			if err := st.PutSessionArchive(ctx, record); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("retry replaced malformed archive: %v", err)
			}
			if err := st.DeleteSessionArchive(ctx, repo, record.Key); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("restore ignored malformed archive: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
				t.Fatalf("malformed archive was modified: %v", err)
			}
			if records, err := st.ListSessionArchives(ctx, domain.HashContent([]byte("other repo"))); err != nil || len(records) != 0 {
				t.Fatalf("another repo's corruption leaked: %+v %v", records, err)
			}
		})
	}
}

func TestFSSessionArchiveRejectsInvalidDirectoryEntries(t *testing.T) {
	for _, kind := range []string{"filename", "hidden filename", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "entries")
			if err := st.PutSessionArchive(ctx, record); err != nil {
				t.Fatal(err)
			}
			path := st.sessionArchivePath(repo, record.Key)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "filename":
				err = os.WriteFile(filepath.Join(filepath.Dir(path), "not-a-hash.json"), []byte("{}"), 0600)
			case "hidden filename":
				err = os.WriteFile(filepath.Join(filepath.Dir(path), ".not-a-hash.json"), []byte("{}"), 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				err = os.Symlink(filepath.Join(st.dataDir, "missing"), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if records, err := st.ListSessionArchives(ctx, repo); !errors.Is(err, domain.ErrIntegrity) || records != nil {
				t.Fatalf("invalid entry accepted: %+v %v", records, err)
			}
		})
	}
}

func TestFSSessionArchiveConcurrentInstancesPreserveFirstWrite(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "concurrent archive")
	peer := NewFSStore(st.dataDir)
	start := make(chan struct{})
	observed := make(chan domain.SessionArchive, 20)
	failures := make(chan error, 20)
	var writers sync.WaitGroup
	for index := 0; index < 20; index++ {
		writers.Go(func() {
			<-start
			target := []*FSStore{st, peer}[index%2]
			candidate := record
			candidate.ArchivedBy = fmt.Sprintf("dev:writer-%d", index)
			candidate.ArchivedAt = candidate.ArchivedAt.Add(time.Duration(index) * time.Second)
			if err := target.PutSessionArchive(ctx, candidate); err != nil {
				failures <- err
				return
			}
			records, err := target.ListSessionArchives(ctx, repo)
			if err != nil || len(records) != 1 {
				failures <- fmt.Errorf("concurrent list: %+v %v", records, err)
				return
			}
			observed <- records[0]
		})
	}
	close(start)
	writers.Wait()
	close(failures)
	close(observed)
	for err := range failures {
		t.Error(err)
	}
	records, err := st.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 1 {
		t.Fatalf("final archive: %+v %v", records, err)
	}
	for got := range observed {
		requireSessionArchive(t, got, records[0])
	}
}

func TestFSSessionArchiveCanceledOperations(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "cancellation")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.PutSessionArchive(canceled, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled insert: %v", err)
	}
	if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 0 {
		t.Fatalf("canceled insert wrote archive: %+v %v", records, err)
	}
	if err := st.PutSessionArchive(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSessionArchive(canceled, repo, record.Key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delete: %v", err)
	}
	if records, err := st.ListSessionArchives(canceled, repo); !errors.Is(err, context.Canceled) || records != nil {
		t.Fatalf("canceled list: %+v %v", records, err)
	}
	if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 1 {
		t.Fatalf("canceled delete removed archive: %+v %v", records, err)
	}
}
