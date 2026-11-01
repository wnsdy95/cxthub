//go:build postgres

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func repositoryViewSourcePagePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) domain.RepositoryViewSourcePage {
	t.Helper()
	page, err := storage.RepositoryViewSourceChanges(ctx, repo, request)
	if err != nil {
		t.Fatal(err)
	}
	if page.Version != 1 || page.Scope != "repository-view-v1" || page.RepoID != repo || page.Epoch == "" || page.Through < 0 || (page.Mode != "baseline" && page.Mode != "delta") {
		t.Fatalf("invalid source page envelope: %+v", page)
	}
	limit := request.Limit
	if limit == 0 {
		limit = domain.DefaultRepositoryViewSourceLimit
	}
	if page.Entries == nil || len(page.Entries) > limit {
		t.Fatalf("null or unbounded source entries: %+v", page)
	}
	if page.NextCursor != "" {
		if page.Checkpoint != nil || len(page.Entries) == 0 {
			t.Fatalf("partial source page published a checkpoint or made no progress: %+v", page)
		}
	} else if page.Checkpoint == nil || page.Checkpoint.Version != 1 || page.Checkpoint.Scope != page.Scope || page.Checkpoint.RepoID != repo || page.Checkpoint.Epoch != page.Epoch || page.Checkpoint.Sequence != page.Through {
		t.Fatalf("final source page did not checkpoint its fixed head: %+v", page)
	}
	for _, entry := range page.Entries {
		if !domain.RepositoryViewSourceKindValid(entry.Kind) || entry.Key == "" || entry.Sequence < 0 || entry.Sequence > page.Through || (entry.Deleted && len(entry.Value) != 0) || (!entry.Deleted && !json.Valid(entry.Value)) || (page.Mode == "baseline" && entry.Deleted) {
			t.Fatalf("invalid source entry: %+v", entry)
		}
	}
	return page
}

func repositoryViewSourceDrainPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, first domain.RepositoryViewSourcePage, limit int) ([]domain.RepositoryViewSourceEntry, domain.RepositoryViewSourceCheckpoint) {
	t.Helper()
	page := first
	entries := []domain.RepositoryViewSourceEntry{}
	seen := map[string]bool{}
	for pages := 0; pages < 4096; pages++ {
		if page.Epoch != first.Epoch || page.Through != first.Through || page.Mode != first.Mode {
			t.Fatalf("source continuation changed fixed image: first=%+v next=%+v", first, page)
		}
		for _, entry := range page.Entries {
			if len(entries) != 0 {
				last := entries[len(entries)-1]
				ordered := last.Kind < entry.Kind || (last.Kind == entry.Kind && last.Key < entry.Key)
				if page.Mode == "delta" {
					ordered = last.Sequence < entry.Sequence || (last.Sequence == entry.Sequence && ordered)
				}
				if !ordered {
					t.Fatalf("source entries are not strictly ordered: %+v then %+v", last, entry)
				}
			}
			entries = append(entries, entry)
		}
		if page.NextCursor == "" {
			return entries, *page.Checkpoint
		}
		if seen[page.NextCursor] {
			t.Fatal("source cursor repeated without progress")
		}
		seen[page.NextCursor] = true
		page = repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: page.NextCursor, Limit: limit})
	}
	t.Fatal("source traversal exceeded fixture bound")
	return nil, domain.RepositoryViewSourceCheckpoint{}
}

func repositoryViewSourceCheckpointPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.RepositoryViewSourceCheckpoint {
	t.Helper()
	_, checkpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1}), 0)
	return checkpoint
}

func repositoryViewSourceDeltaPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, after domain.RepositoryViewSourceCheckpoint) ([]domain.RepositoryViewSourceEntry, domain.RepositoryViewSourceCheckpoint) {
	t.Helper()
	return repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &after, Limit: 1}), 1)
}

func repositoryViewSourceEntryPG(t *testing.T, entries []domain.RepositoryViewSourceEntry, kind, key string) domain.RepositoryViewSourceEntry {
	t.Helper()
	if kind == "unsync" || kind == "ref" {
		canonical, err := json.Marshal(catalogJSONPG[[]string](t, []byte(key)))
		if err != nil {
			t.Fatal(err)
		}
		key = string(canonical)
	}
	var found []domain.RepositoryViewSourceEntry
	for _, entry := range entries {
		if entry.Kind != kind {
			continue
		}
		entryKey := entry.Key
		if kind == "unsync" || kind == "ref" {
			parts := catalogJSONPG[[]string](t, []byte(entryKey))
			raw, err := json.Marshal(parts)
			if err != nil {
				t.Fatal(err)
			}
			entryKey = string(raw)
		}
		if entryKey == key {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one source entry for %s/%s, found %d: %+v", kind, key, len(found), entries)
	}
	return found[0]
}

func repositoryViewSourceErrorPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, request domain.RepositoryViewSourceRequest, want error) {
	t.Helper()
	page, err := storage.RepositoryViewSourceChanges(ctx, repo, request)
	if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
		t.Fatalf("source error=%v want=%v page=%+v", err, want, page)
	}
}

type repositoryViewSourceFixture struct {
	snapshot domain.Snapshot
	pending  domain.Pending
	unsync   domain.Unsync
	archive  domain.SessionArchive
	reflog   string
}

func repositoryViewSourceSeedPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) repositoryViewSourceFixture {
	t.Helper()
	stamp := time.Date(2026, 10, 19, 1, 2, 3, 456000, time.UTC)
	fixture := repositoryViewSourceFixture{snapshot: initializationPGSnapshot(t, storage, repo, "source fixture")}
	fixture.pending = domain.Pending{RepoID: repo, SessionID: "source-session", Provider: domain.ProviderClaude, Branch: "main", Target: fixture.snapshot.ID, Author: domain.TeamIdentity{Name: "Source", Email: "source@example.test", Team: "fixture"}, UpdatedAt: stamp, ActivityAt: &stamp}
	fixture.unsync = domain.Unsync{RepoID: repo, User: "source-user", Branch: "feature/source", Target: fixture.snapshot.ID, Author: fixture.pending.Author, UpdatedAt: stamp}
	fixture.archive = domain.SessionArchive{RepoID: repo, Key: domain.SessionArchiveKey(fixture.snapshot), SnapshotID: fixture.snapshot.ID, Provider: fixture.snapshot.Provider, ArchivedAt: stamp, ArchivedBy: "source-user"}
	if err := storage.PutPending(ctx, repo, fixture.pending); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutUnsync(ctx, repo, fixture.unsync); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSessionArchive(ctx, fixture.archive); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "source/ref", Target: fixture.snapshot.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT id::text FROM reflog WHERE repo_id=$1 AND name='source/ref'`, string(repo)).Scan(&fixture.reflog); err != nil {
		t.Fatal(err)
	}
	if err := storage.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestPGRepositoryViewSourceAdaptersAndSQLShareCoalescedSequence(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	next := initializationPGSnapshot(t, storage, repo, "source next")
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	publicBefore := catalogCheckpointPG(t, ctx, storage, repo)
	event := catalogHistoryPG(repo)
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var reflog string
	err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		fixture.pending.Target = next.ID
		if err := storage.PutPending(bound, repo, fixture.pending); err != nil {
			return err
		}
		if _, err := storage.SetPendingDismissed(bound, repo, fixture.pending.SessionID, true); err != nil {
			return err
		}
		fixture.unsync.Target = next.ID
		if err := storage.PutUnsync(bound, repo, fixture.unsync); err != nil {
			return err
		}
		branch, protect := "source-default", true
		if err := storage.UpdateRepoConfig(bound, repo, &branch, &protect); err != nil {
			return err
		}
		if err := storage.CompareAndSwapRef(bound, repo, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "source/ref", Target: next.ID}, fixture.snapshot.ID); err != nil {
			return err
		}
		if err := storage.AdvanceRepositoryRevision(bound, repo, true); err != nil {
			return err
		}
		if err := storage.AdvanceEvidenceRevision(bound, repo); err != nil {
			return err
		}
		catalogExecPG(t, bound, storage.db(bound), `UPDATE snapshots SET message='intermediate' WHERE repo_id=$1 AND id=$2`, string(repo), string(fixture.snapshot.ID))
		catalogExecPG(t, bound, storage.db(bound), `UPDATE snapshots SET message='final source image' WHERE repo_id=$1 AND id=$2`, string(repo), string(fixture.snapshot.ID))
		catalogExecPG(t, bound, storage.db(bound), `UPDATE session_archives SET archived_by='changed-user' WHERE repo_id=$1`, string(repo))
		catalogExecPG(t, bound, storage.db(bound), `UPDATE repos SET context_protocol=1 WHERE id=$1`, string(repo))
		catalogExecPG(t, bound, storage.db(bound), `INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,$3)`, string(repo), event.ID, raw)
		return storage.db(bound).QueryRow(bound, `SELECT id::text FROM reflog WHERE repo_id=$1 AND name='source/ref' AND new=$2`, string(repo), string(next.ID)).Scan(&reflog)
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, checkpoint := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
	if len(entries) != 10 || checkpoint.Sequence != before.Sequence+1 || checkpoint.Epoch != before.Epoch {
		t.Fatalf("mixed transaction did not coalesce ten source kinds: entries=%+v checkpoint=%+v", entries, checkpoint)
	}
	for _, entry := range entries {
		if entry.Sequence != checkpoint.Sequence || entry.Deleted {
			t.Fatalf("mixed source transaction split or lost its image: %+v", entry)
		}
	}
	if image := catalogJSONPG[domain.Pending](t, repositoryViewSourceEntryPG(t, entries, "pending", fixture.pending.SessionID).Value); image.Target != next.ID || !image.Dismissed || image.ActivityAt == nil || !image.ActivityAt.Equal(*fixture.pending.ActivityAt) {
		t.Fatalf("pending coalescing lost policy/activity: %+v", image)
	}
	if image := catalogJSONPG[domain.Unsync](t, repositoryViewSourceEntryPG(t, entries, "unsync", `["source-user","feature/source"]`).Value); image.Target != next.ID || image.Author != fixture.unsync.Author {
		t.Fatalf("unsync coalescing lost image: %+v", image)
	}
	if image := catalogJSONPG[domain.Repo](t, repositoryViewSourceEntryPG(t, entries, "repository", string(repo)).Value); image.ID != repo || image.DefaultBranch != "source-default" || !image.ProtectDefault || image.ContextProtocol != 1 {
		t.Fatalf("repository policy image incomplete: %+v", image)
	}
	if image := catalogJSONPG[domain.RepositoryRevision](t, repositoryViewSourceEntryPG(t, entries, "revision", string(repo)).Value); image.Graph != 1 || image.Pending != 1 || image.Evidence != 1 {
		t.Fatalf("revision source image incomplete: %+v", image)
	}
	if image := catalogJSONPG[domain.SessionArchive](t, repositoryViewSourceEntryPG(t, entries, "session_archive", string(fixture.archive.Key)).Value); image.ArchivedBy != "changed-user" || image.SnapshotID != fixture.snapshot.ID {
		t.Fatalf("archive source image incomplete: %+v", image)
	}
	if image := catalogJSONPG[domain.RefLogEntry](t, repositoryViewSourceEntryPG(t, entries, "reflog", reflog).Value); image.Old != fixture.snapshot.ID || image.New != next.ID {
		t.Fatalf("adapter reflog was not captured: %+v", image)
	}
	if image := catalogJSONPG[domain.Snapshot](t, repositoryViewSourceEntryPG(t, entries, "snapshot", string(fixture.snapshot.ID)).Value); image.Message != "final source image" {
		t.Fatalf("snapshot did not coalesce: %+v", image)
	}
	publicEntries, publicCheckpoint := catalogDrainPG(t, ctx, storage, repo, catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicBefore, Limit: 1}), 1)
	if len(publicEntries) != 4 || publicCheckpoint.Sequence != checkpoint.Sequence || publicCheckpoint.Epoch != checkpoint.Epoch {
		t.Fatalf("public catalog changed scope or diverged from source sequence: %+v %+v", publicEntries, publicCheckpoint)
	}
	for _, entry := range publicEntries {
		source := repositoryViewSourceEntryPG(t, entries, entry.Kind, entry.Key)
		if source.Sequence != entry.Sequence || source.Deleted != entry.Deleted || !reflect.DeepEqual(catalogJSONPG[any](t, source.Value), catalogJSONPG[any](t, entry.Value)) {
			t.Fatalf("source union changed public image: source=%+v public=%+v", source, entry)
		}
	}
	var rows, sequences, kinds int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT seq),count(DISTINCT entity_kind) FROM repository_view_source_images WHERE repo_id=$1 AND epoch=$2 AND seq>$3`, string(repo), checkpoint.Epoch, before.Sequence).Scan(&rows, &sequences, &kinds); err != nil {
		t.Fatal(err)
	}
	if rows != 10 || sequences != 1 || kinds != 10 {
		t.Fatalf("SQL source view omitted or duplicated journal rows: rows=%d sequences=%d kinds=%d", rows, sequences, kinds)
	}
}

func TestPGRepositoryViewSourcePrivateMutationsAndTombstones(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE unsync_contexts SET data=jsonb_set(data,'{author,name}','"Changed author"') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE session_archives SET archived_by='changed-user' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE reflog SET old=new WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE repos SET protect_default=true WHERE id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE repository_revisions SET graph=graph+1,pending=pending+1,evidence=evidence+1 WHERE repo_id=$1`, string(repo))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, changed := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
	if len(entries) != 6 || changed.Sequence != before.Sequence+1 {
		t.Fatalf("direct private updates were not captured: %+v %+v", entries, changed)
	}
	if image := catalogJSONPG[domain.Pending](t, repositoryViewSourceEntryPG(t, entries, "pending", fixture.pending.SessionID).Value); !image.Dismissed {
		t.Fatalf("dismissed policy flag lost: %+v", image)
	}
	if image := catalogJSONPG[domain.Unsync](t, repositoryViewSourceEntryPG(t, entries, "unsync", `["source-user","feature/source"]`).Value); image.Author.Name != "Changed author" {
		t.Fatalf("unsync update lost: %+v", image)
	}
	if image := catalogJSONPG[domain.SessionArchive](t, repositoryViewSourceEntryPG(t, entries, "session_archive", string(fixture.archive.Key)).Value); image.ArchivedBy != "changed-user" {
		t.Fatalf("archive update lost: %+v", image)
	}
	if image := catalogJSONPG[domain.RefLogEntry](t, repositoryViewSourceEntryPG(t, entries, "reflog", fixture.reflog).Value); image.Old != fixture.snapshot.ID {
		t.Fatalf("reflog update lost: %+v", image)
	}
	if image := catalogJSONPG[domain.Repo](t, repositoryViewSourceEntryPG(t, entries, "repository", string(repo)).Value); !image.ProtectDefault {
		t.Fatalf("repository protection flag lost: %+v", image)
	}
	if image := catalogJSONPG[domain.RepositoryRevision](t, repositoryViewSourceEntryPG(t, entries, "revision", string(repo)).Value); image.Graph != 2 || image.Pending != 1 || image.Evidence != 1 {
		t.Fatalf("raw revision update lost: %+v", image)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := storage.DeletePending(bound, repo, fixture.pending.SessionID); err != nil {
			return err
		}
		if err := storage.DeleteUnsync(bound, repo, fixture.unsync.User, fixture.unsync.Branch); err != nil {
			return err
		}
		if err := storage.DeleteSessionArchive(bound, repo, fixture.archive.Key); err != nil {
			return err
		}
		catalogExecPG(t, bound, storage.db(bound), `DELETE FROM reflog WHERE repo_id=$1`, string(repo))
		catalogExecPG(t, bound, storage.db(bound), `DELETE FROM repository_revisions WHERE repo_id=$1`, string(repo))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deleted, end := repositoryViewSourceDeltaPG(t, ctx, storage, repo, changed)
	if len(deleted) != 5 || end.Sequence != changed.Sequence+1 {
		t.Fatalf("private deletions did not coalesce: %+v %+v", deleted, end)
	}
	for kind, key := range map[string]string{"pending": fixture.pending.SessionID, "unsync": `["source-user","feature/source"]`, "session_archive": string(fixture.archive.Key), "reflog": fixture.reflog, "revision": string(repo)} {
		entry := repositoryViewSourceEntryPG(t, deleted, kind, key)
		if !entry.Deleted || len(entry.Value) != 0 || entry.Sequence != end.Sequence {
			t.Fatalf("missing private tombstone: %+v", entry)
		}
	}
	baseline, _ := repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1}), 0)
	for _, entry := range baseline {
		switch entry.Kind {
		case "pending", "unsync", "session_archive", "reflog", "revision":
			t.Fatalf("fresh source baseline resurrected deleted entity: %+v", entry)
		}
	}
}

func TestPGRepositoryViewSourceImmutableBaselineAndSplitDelta(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	if first.NextCursor == "" {
		t.Fatal("fixture must open a partial baseline")
	}
	want, before := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `DELETE FROM unsync_contexts WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `DELETE FROM session_archives WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE reflog SET old=new WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE repos SET default_branch='after-baseline' WHERE id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE repository_revisions SET pending=pending+1 WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='after-baseline' WHERE repo_id=$1`, string(repo))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, resumed := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	if !reflect.DeepEqual(got, want) || resumed != before {
		t.Fatalf("concurrent mutations changed opened baseline: got=%+v want=%+v checkpoint=%+v", got, want, resumed)
	}
	deltaFirst := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before, Limit: 1})
	if deltaFirst.NextCursor == "" || deltaFirst.Through != before.Sequence+1 {
		t.Fatalf("fixture must split one committed transaction: %+v", deltaFirst)
	}
	if _, err := storage.SetPendingDismissed(ctx, repo, fixture.pending.SessionID, false); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='newer-than-delta' WHERE repo_id=$1`, string(repo))
	delta, checkpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, deltaFirst, 1)
	if len(delta) != 7 || checkpoint.Sequence != before.Sequence+1 {
		t.Fatalf("fixed delta lost or extended a transaction: %+v %+v", delta, checkpoint)
	}
	for _, entry := range delta {
		if entry.Sequence != checkpoint.Sequence {
			t.Fatalf("newer write leaked into split delta: %+v", entry)
		}
	}
	if image := catalogJSONPG[domain.Pending](t, repositoryViewSourceEntryPG(t, delta, "pending", fixture.pending.SessionID).Value); !image.Dismissed {
		t.Fatalf("split delta read mutable pending: %+v", image)
	}
	if image := catalogJSONPG[domain.Snapshot](t, repositoryViewSourceEntryPG(t, delta, "snapshot", string(fixture.snapshot.ID)).Value); image.Message != "after-baseline" {
		t.Fatalf("split delta read mutable snapshot: %+v", image)
	}
	for kind, key := range map[string]string{"unsync": `["source-user","feature/source"]`, "session_archive": string(fixture.archive.Key)} {
		if entry := repositoryViewSourceEntryPG(t, delta, kind, key); !entry.Deleted {
			t.Fatalf("concurrent deletion missing: %+v", entry)
		}
	}
	next, end := repositoryViewSourceDeltaPG(t, ctx, storage, repo, checkpoint)
	if len(next) != 2 || end.Sequence != checkpoint.Sequence+2 {
		t.Fatalf("follow-up delta lost newer writes: %+v %+v", next, end)
	}
	empty := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &end})
	if len(empty.Entries) != 0 || empty.Checkpoint == nil || *empty.Checkpoint != end {
		t.Fatalf("empty source delta failed to acknowledge committed head: %+v", empty)
	}
}

func TestPGRepositoryViewSourceSavepointsAndOuterRollback(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `SAVEPOINT first_allocation`)
	catalogExecPG(t, ctx, tx, `UPDATE pending_contexts SET data=jsonb_set(data,'{branch}','"discarded"') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='discarded' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `ROLLBACK TO SAVEPOINT first_allocation`)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='retained' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE pending_contexts SET data=jsonb_set(data,'{branch}','"retained"') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `SAVEPOINT overlay`)
	catalogExecPG(t, ctx, tx, `DELETE FROM pending_contexts WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='discarded overlay' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `ROLLBACK TO SAVEPOINT overlay`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, checkpoint := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
	if len(entries) != 2 || checkpoint.Sequence != before.Sequence+1 {
		t.Fatalf("savepoint left a sequence gap or stale entries: %+v %+v", entries, checkpoint)
	}
	if image := catalogJSONPG[domain.Pending](t, repositoryViewSourceEntryPG(t, entries, "pending", fixture.pending.SessionID).Value); image.Branch != "retained" {
		t.Fatalf("pending savepoint image not restored: %+v", image)
	}
	if image := catalogJSONPG[domain.Snapshot](t, repositoryViewSourceEntryPG(t, entries, "snapshot", string(fixture.snapshot.ID)).Value); image.Message != "retained" {
		t.Fatalf("public savepoint image not restored: %+v", image)
	}
	abort := errors.New("source outer rollback")
	err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.SetPendingDismissed(bound, repo, fixture.pending.SessionID, true); err != nil {
			return err
		}
		if err := storage.AdvanceRepositoryRevision(bound, repo, true); err != nil {
			return err
		}
		catalogExecPG(t, bound, storage.db(bound), `UPDATE snapshots SET message='outer rollback' WHERE repo_id=$1`, string(repo))
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("outer rollback error=%v", err)
	}
	entries, after := repositoryViewSourceDeltaPG(t, ctx, storage, repo, checkpoint)
	if len(entries) != 0 || after != checkpoint {
		t.Fatalf("outer rollback retained source journal or sequence: %+v %+v", entries, after)
	}
	if rows := catalogRowsPG(t, ctx, storage, repo, checkpoint.Sequence); len(rows) != 0 {
		t.Fatalf("outer rollback retained public journal: %+v", rows)
	}
}

func TestPGRepositoryViewSourceAllowlistAndRawSQLNoOps(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	repositoryViewSourceSeedPG(t, ctx, storage, repo)
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	for _, query := range []string{
		`UPDATE pending_contexts SET data=jsonb_set(data || '{"future_secret":"pending secret"}', '{author,future_secret}', '"author secret"') WHERE repo_id=$1`,
		`UPDATE unsync_contexts SET data=jsonb_set(data || '{"future_secret":"unsync secret"}', '{author,future_secret}', '"author secret"') WHERE repo_id=$1`,
		`UPDATE pending_contexts SET data=data WHERE repo_id=$1`,
		`UPDATE unsync_contexts SET data=data WHERE repo_id=$1`,
		`UPDATE session_archives SET archived_at=archived_at,archived_by=archived_by WHERE repo_id=$1`,
		`UPDATE reflog SET old=old,new=new,created_at=created_at WHERE repo_id=$1`,
		`UPDATE repository_revisions SET graph=graph,pending=pending,evidence=evidence WHERE repo_id=$1`,
		`UPDATE repos SET protect_default=protect_default,default_branch=default_branch,team='outside-source-contract' WHERE id=$1`,
		`UPDATE snapshots SET message=message WHERE repo_id=$1`,
		`UPDATE refs SET version=version+1,updated_at=updated_at+interval '1 second' WHERE repo_id=$1`,
		`DELETE FROM pending_contexts WHERE repo_id=$1 AND session_id='absent'`,
	} {
		catalogExecPG(t, ctx, storage.pool, query, string(repo))
	}
	entries, checkpoint := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
	if len(entries) != 0 || checkpoint != before {
		t.Fatalf("no-op or unknown fields advanced source sequence: %+v %+v", entries, checkpoint)
	}
	for _, test := range []struct {
		kind, table, column, fields string
	}{
		{"repository", "repos", "id", "id remote_url remote_url_supported default_branch repository_id git_remote_url git_remote_url_supported context_protocol required_doc_identity protect_default description website topics"},
		{"pending", "pending_contexts", "repo_id", "repo_id session_id branch provider target author updated_at activity_at dismissed"},
		{"unsync", "unsync_contexts", "repo_id", "repo_id user branch target author updated_at"},
		{"session_archive", "session_archives", "repo_id", "repo_id key snapshot_id provider session_id archived_at archived_by"},
		{"reflog", "reflog", "repo_id", "kind name old new created_at"},
		{"revision", "repository_revisions", "repo_id", "graph pending evidence"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			var raw []byte
			query := `SELECT cxt_repository_view_source_image($1,to_jsonb(source) || '{"future_secret":"table secret"}'::jsonb) FROM ` + test.table + ` source WHERE ` + test.column + `=$2 LIMIT 1`
			if err := storage.pool.QueryRow(ctx, query, test.kind, string(repo)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			image := catalogJSONPG[map[string]json.RawMessage](t, raw)
			fields := strings.Fields(test.fields)
			if len(image) != len(fields) || strings.Contains(string(raw), "future_secret") || strings.Contains(string(raw), "secret") {
				t.Fatalf("source projection is not explicitly allowlisted: %s", raw)
			}
			for _, field := range fields {
				if _, exists := image[field]; !exists {
					t.Errorf("source %s image omits %s: %s", test.kind, field, raw)
				}
			}
			if authorRaw, exists := image["author"]; exists {
				author := catalogJSONPG[map[string]string](t, authorRaw)
				if len(author) != 3 || author["name"] != "Source" || author["email"] != "source@example.test" || author["team"] != "fixture" {
					t.Fatalf("nested author is not allowlisted: %s", authorRaw)
				}
			}
		})
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE unsync_contexts SET data=jsonb_set(data,'{author,name}','"Known field changed"') WHERE repo_id=$1`, string(repo))
	entries, _ = repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
	if len(entries) != 2 {
		t.Fatalf("known field updates did not produce images: %+v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(string(entry.Value), "future_secret") || strings.Contains(string(entry.Value), "secret") {
			t.Fatalf("unknown JSON fields leaked into committed source image: %s", entry.Value)
		}
	}
}

func TestPGRepositoryViewSourceRepositoryPolicyAndIdentity(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	protect := true
	if err := storage.UpdateRepoConfig(ctx, repo, nil, &protect); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	entries, checkpoint := repositoryViewSourceDeltaPG(t, compatible, storage, repo, before)
	if len(entries) != 2 || checkpoint.Sequence != before.Sequence+2 {
		t.Fatalf("repository policy writes missing: %+v %+v", entries, checkpoint)
	}
	for index, entry := range entries {
		image := catalogJSONPG[domain.Repo](t, entry.Value)
		if entry.Kind != "repository" || entry.Key != string(repo) || !image.ProtectDefault || image.ID != repo {
			t.Fatalf("repository policy image missing: %+v image=%+v", entry, image)
		}
		if index == 1 && image.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
			t.Fatalf("document identity policy missing: %+v", image)
		}
	}
	if rows := catalogRowsPG(t, ctx, storage, repo, before.Sequence); len(rows) != 0 {
		t.Fatalf("private policy changes leaked into public catalog: %+v", rows)
	}
}

func TestPGRepositoryViewSourcePruneInvalidatesCursorsAndRetainsAnchors(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	old := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	for _, branch := range []string{"obsolete", "floor-anchor"} {
		catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{branch}',to_jsonb($2::text)) WHERE repo_id=$1`, string(repo), branch)
	}
	if err := storage.DeleteUnsync(ctx, repo, fixture.unsync.User, fixture.unsync.Branch); err != nil {
		t.Fatal(err)
	}
	floor := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='after floor' WHERE repo_id=$1`, string(repo))
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	delta := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &floor, Limit: 1})
	if first.NextCursor == "" || delta.NextCursor == "" {
		t.Fatal("prune fixture requires baseline and delta continuations")
	}
	want, checkpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	countRows := func() int {
		t.Helper()
		var count int
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_view_source_changes WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	beforeCount := countRows()
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `SELECT cxt_prune_catalog($1,$2)`, string(repo), floor.Sequence)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows() != beforeCount {
		t.Fatal("rolled-back prune removed private journal rows")
	}
	if got, end := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1); !reflect.DeepEqual(got, want) || end != checkpoint {
		t.Fatalf("rolled-back prune invalidated a source traversal: %+v %+v", got, end)
	}
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), floor.Sequence)
	if countRows() >= beforeCount {
		t.Fatal("prune did not remove obsolete private images")
	}
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, After: &old}, {Version: 1, Cursor: first.NextCursor}, {Version: 1, Cursor: delta.NextCursor}} {
		repositoryViewSourceErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	got, end := repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}), 1)
	if !reflect.DeepEqual(got, want) || end != checkpoint {
		t.Fatalf("prune changed live baseline or resurrected a deletion: %+v %+v", got, end)
	}
	var anchor []byte
	var deleted bool
	if err := storage.pool.QueryRow(ctx, `SELECT after_image FROM repository_view_source_changes WHERE repo_id=$1 AND entity_kind='pending' AND seq<=$2`, string(repo), floor.Sequence).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if image := catalogJSONPG[domain.Pending](t, anchor); image.Branch != "floor-anchor" || image.Dismissed {
		t.Fatalf("prune did not retain exact private floor anchor: %+v", image)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT deleted FROM repository_view_source_changes WHERE repo_id=$1 AND entity_kind='unsync' AND seq<=$2`, string(repo), floor.Sequence).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("private tombstone anchor lost: deleted=%v err=%v", deleted, err)
	}
	changes, final := repositoryViewSourceDeltaPG(t, ctx, storage, repo, floor)
	if len(changes) != 2 || final != checkpoint {
		t.Fatalf("delta from valid floor lost public/private changes: %+v %+v", changes, final)
	}
}

func TestPGRepositoryViewSourceEpochResetReseedsBothJournals(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	if err := storage.DeleteUnsync(ctx, repo, fixture.unsync.User, fixture.unsync.Branch); err != nil {
		t.Fatal(err)
	}
	other := domain.HashContent([]byte(string(repo) + "other-source"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	otherBefore := repositoryViewSourceCheckpointPG(t, ctx, storage, other)
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	want, before := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	publicFirst := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	publicWant, publicBefore := catalogDrainPG(t, ctx, storage, repo, publicFirst, 1)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `SELECT cxt_reset_catalog($1)`, string(repo))
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got, checkpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1); !reflect.DeepEqual(got, want) || checkpoint != before {
		t.Fatalf("rolled-back reset changed private baseline: %+v %+v", got, checkpoint)
	}
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
	repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: first.NextCursor}, domain.ErrCatalogResetRequired)
	repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before}, domain.ErrCatalogResetRequired)
	catalogExpectErrorPG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Cursor: publicFirst.NextCursor}, domain.ErrCatalogResetRequired)
	catalogExpectErrorPG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicBefore}, domain.ErrCatalogResetRequired)
	for index := range want {
		want[index].Sequence = 0
	}
	for index := range publicWant {
		publicWant[index].Sequence = 0
	}
	got, after := repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}), 1)
	if !reflect.DeepEqual(got, want) || after.Epoch == before.Epoch || after.Sequence != 0 {
		t.Fatalf("reset did not reseed exact current private/public images at zero: %+v %+v", got, after)
	}
	publicGot, publicAfter := catalogDrainPG(t, ctx, storage, repo, catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	if !reflect.DeepEqual(publicGot, publicWant) || publicAfter.Epoch != after.Epoch || publicAfter.Sequence != 0 {
		t.Fatalf("reset changed old public catalog image contract: %+v %+v", publicGot, publicAfter)
	}
	if otherAfter := repositoryViewSourceCheckpointPG(t, ctx, storage, other); otherAfter != otherBefore {
		t.Fatalf("reset crossed repositories: before=%+v after=%+v", otherBefore, otherAfter)
	}
	if _, err := storage.SetPendingDismissed(ctx, repo, fixture.pending.SessionID, true); err != nil {
		t.Fatal(err)
	}
	changes, end := repositoryViewSourceDeltaPG(t, ctx, storage, repo, after)
	if len(changes) != 1 || changes[0].Kind != "pending" || changes[0].Sequence != 1 || end.Epoch != after.Epoch {
		t.Fatalf("first post-reset private mutation missing: %+v %+v", changes, end)
	}
}

func TestPGRepositoryViewSourcePrivateBaselinePreservesPublicContinuations(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	repositoryViewSourceSeedPG(t, ctx, storage, repo)
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	_, old := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	publicFirst := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	publicWant, publicBefore := catalogDrainPG(t, ctx, storage, repo, publicFirst, 1)
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='upgrade boundary' WHERE repo_id=$1`, string(repo))
	boundary := catalogReadStatePG(t, ctx, storage, repo)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `DELETE FROM repository_view_source_changes WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE repository_view_source_state SET baseline_seq=$2 WHERE repo_id=$1`, string(repo), boundary.head)
	catalogExecPG(t, ctx, tx, `SELECT cxt_seed_repository_view_source($1)`, string(repo))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &old}, domain.ErrCatalogResetRequired)
	repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: first.NextCursor}, domain.ErrCatalogResetRequired)
	publicGot, publicEnd := catalogDrainPG(t, ctx, storage, repo, publicFirst, 1)
	if !reflect.DeepEqual(publicGot, publicWant) || publicEnd != publicBefore {
		t.Fatalf("private seeding invalidated old public continuation: %+v %+v", publicGot, publicEnd)
	}
	publicDelta := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicBefore})
	if len(publicDelta.Entries) != 1 || publicDelta.Entries[0].Kind != "snapshot" || publicDelta.Through != boundary.head {
		t.Fatalf("private baseline hid retained public history: %+v", publicDelta)
	}
	if checkpoint := repositoryViewSourceCheckpointPG(t, ctx, storage, repo); checkpoint.Sequence != boundary.head || checkpoint.Epoch != boundary.epoch {
		t.Fatalf("private baseline invented a new public epoch or sequence: %+v", checkpoint)
	}
}

func TestPGRepositoryViewSourceRejectsCrossRepositoryAndScopeCursors(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	repositoryViewSourceSeedPG(t, ctx, storage, repo)
	other := domain.HashContent([]byte(string(repo) + "cursor-other"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	_, checkpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, first, 1)
	if first.NextCursor == "" {
		t.Fatal("cross-repository fixture requires a continuation")
	}
	repositoryViewSourceErrorPG(t, ctx, storage, other, domain.RepositoryViewSourceRequest{Version: 1, Cursor: first.NextCursor}, domain.ErrCatalogResetRequired)
	repositoryViewSourceErrorPG(t, ctx, storage, other, domain.RepositoryViewSourceRequest{Version: 1, After: &checkpoint}, domain.ErrCatalogResetRequired)
	for _, test := range []struct {
		name   string
		change func(*domain.RepositoryViewSourceCheckpoint)
	}{
		{"scope", func(checkpoint *domain.RepositoryViewSourceCheckpoint) { checkpoint.Scope = "sync-metadata-v1" }},
		{"version", func(checkpoint *domain.RepositoryViewSourceCheckpoint) { checkpoint.Version++ }},
		{"future", func(checkpoint *domain.RepositoryViewSourceCheckpoint) { checkpoint.Sequence++ }},
		{"epoch", func(checkpoint *domain.RepositoryViewSourceCheckpoint) {
			checkpoint.Epoch = "11111111-1111-1111-1111-111111111111"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := checkpoint
			test.change(&bad)
			repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &bad}, domain.ErrCatalogResetRequired)
		})
	}
	for _, request := range []domain.RepositoryViewSourceRequest{
		{Version: 2},
		{Version: 1, Limit: -1},
		{Version: 1, Limit: 1001},
		{Version: 1, Cursor: "not-a-cursor"},
		{Version: 1, After: &checkpoint, Cursor: first.NextCursor},
	} {
		repositoryViewSourceErrorPG(t, ctx, storage, repo, request, domain.ErrValidation)
	}
	public := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	repositoryViewSourceErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: public.NextCursor}, domain.ErrValidation)
	catalogExpectErrorPG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Cursor: first.NextCursor}, domain.ErrValidation)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	repositoryViewSourceErrorPG(t, cancelled, storage, repo, domain.RepositoryViewSourceRequest{Version: 1}, context.Canceled)
}

func TestPGRepositoryViewSourceReadOnlySnapshotsExposeCommittedStateOnly(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	fixture := repositoryViewSourceSeedPG(t, ctx, storage, repo)
	before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	writer := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, writer, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if !storage.InReadOnlyTransaction(read) {
			return errors.New("fixture did not establish a read-only snapshot")
		}
		first := repositoryViewSourcePagePG(t, read, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before})
		if len(first.Entries) != 0 || first.Checkpoint == nil || *first.Checkpoint != before {
			t.Fatalf("source reader exposed uncommitted private write: %+v", first)
		}
		if err := writer.Commit(ctx); err != nil {
			return err
		}
		again := repositoryViewSourcePagePG(t, read, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before})
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("source read escaped caller's repeatable-read snapshot: first=%+v next=%+v", first, again)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, committed := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
	if len(entries) != 1 || committed.Sequence != before.Sequence+1 || !catalogJSONPG[domain.Pending](t, entries[0].Value).Dismissed {
		t.Fatalf("fresh reader did not observe committed pending: %+v %+v", entries, committed)
	}
	abort := errors.New("discard source write context")
	err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.SetPendingDismissed(bound, repo, fixture.pending.SessionID, false); err != nil {
			return err
		}
		repositoryViewSourceErrorPG(t, bound, storage, repo, domain.RepositoryViewSourceRequest{Version: 1}, domain.ErrConflict)
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("caller-write rollback error=%v", err)
	}
	if checkpoint := repositoryViewSourceCheckpointPG(t, ctx, storage, repo); checkpoint != committed {
		t.Fatalf("rejected caller write leaked a committed source checkpoint: %+v", checkpoint)
	}
}

func TestPGRepositoryViewSourceWritersSerializeAcrossBothJournals(t *testing.T) {
	for _, privateFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("private_first_%t", privateFirst), func(t *testing.T) {
			storage, ctx, repo := catalogPG(t)
			repositoryViewSourceSeedPG(t, ctx, storage, repo)
			before := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
			first := catalogTxPG(t, ctx, storage)
			second := catalogTxPG(t, ctx, storage)
			var firstPID, secondPID int
			if err := first.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
				t.Fatal(err)
			}
			if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID); err != nil {
				t.Fatal(err)
			}
			privateSQL := `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`
			publicSQL := `UPDATE snapshots SET message='ordered public writer' WHERE repo_id=$1`
			firstSQL, secondSQL := publicSQL, privateSQL
			firstKind, secondKind := "snapshot", "pending"
			if privateFirst {
				firstSQL, secondSQL = privateSQL, publicSQL
				firstKind, secondKind = "pending", "snapshot"
			}
			catalogExecPG(t, ctx, first, firstSQL, string(repo))
			writerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			done := make(chan error, 1)
			go func() {
				defer close(done)
				_, err := second.Exec(writerCtx, secondSQL, string(repo))
				done <- err
			}()
			defer func() {
				cancel()
				_ = first.Rollback(context.Background())
				<-done
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var blocked bool
				if err := storage.pool.QueryRow(writerCtx, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, firstPID, secondPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("second journal writer escaped uncommitted predecessor: %v", err)
				case <-writerCtx.Done():
					t.Fatal(writerCtx.Err())
				case <-tick.C:
				}
			}
			if checkpoint := repositoryViewSourceCheckpointPG(t, ctx, storage, repo); checkpoint != before {
				t.Fatalf("blocked writers exposed in-flight source sequence: %+v", checkpoint)
			}
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-writerCtx.Done():
				t.Fatal(writerCtx.Err())
			}
			entries, intermediate := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
			if len(entries) != 1 || entries[0].Kind != firstKind || intermediate.Sequence != before.Sequence+1 {
				t.Fatalf("uncommitted second writer leaked across journals: %+v %+v", entries, intermediate)
			}
			if err := second.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			entries, end := repositoryViewSourceDeltaPG(t, ctx, storage, repo, before)
			if len(entries) != 2 || entries[0].Kind != firstKind || entries[0].Sequence != before.Sequence+1 || entries[1].Kind != secondKind || entries[1].Sequence != before.Sequence+2 || end.Sequence != before.Sequence+2 {
				t.Fatalf("source sequences do not follow cross-journal commit order: %+v %+v", entries, end)
			}
		})
	}
}

func TestPGRepositoryViewSourceSequencesAreJSONStringsWithoutChangingPublicCatalog(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	repositoryViewSourceSeedPG(t, ctx, storage, repo)
	const large int64 = 9007199254740993
	catalogExecPG(t, ctx, storage.pool, `UPDATE repository_catalog_state SET head_seq=$2,writer_xid=NULL WHERE repo_id=$1`, string(repo), large)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='large source sequence' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE repository_revisions SET graph=$2,pending=$2,evidence=$2 WHERE repo_id=$1`, string(repo), large)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	if page.Checkpoint == nil || page.Through != large+1 {
		t.Fatalf("precision fixture must have a complete large-sequence page: %+v", page)
	}
	marshalObject := func(value any) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return catalogJSONPG[map[string]json.RawMessage](t, raw)
	}
	assertString := func(raw json.RawMessage, want int64) {
		t.Helper()
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != strconv.FormatInt(want, 10) {
			t.Fatalf("source sequence is not a lossless decimal JSON string: raw=%s want=%d err=%v", raw, want, err)
		}
	}
	object := marshalObject(page)
	assertString(object["through"], page.Through)
	assertString(marshalObject(page.Checkpoint)["sequence"], page.Checkpoint.Sequence)
	for _, entry := range page.Entries {
		assertString(marshalObject(entry)["sequence"], entry.Sequence)
		if entry.Kind == "revision" {
			for _, raw := range catalogJSONPG[map[string]json.RawMessage](t, entry.Value) {
				assertString(raw, large)
			}
		}
	}
	first := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	rawCursor, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor := catalogJSONPG[map[string]json.RawMessage](t, rawCursor)
	for _, field := range []string{"floor", "baseline", "after", "sequence"} {
		var decimal string
		if err := json.Unmarshal(cursor[field], &decimal); err != nil {
			t.Fatalf("cursor %s is not a JSON string: %s: %v", field, rawCursor, err)
		}
		if _, err := strconv.ParseInt(decimal, 10, 64); err != nil {
			t.Fatalf("cursor %s is not a decimal sequence: %q", field, decimal)
		}
	}
	assertString(catalogJSONPG[map[string]json.RawMessage](t, cursor["checkpoint"])["sequence"], large+1)
	public := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1})
	publicObject := marshalObject(public)
	if string(publicObject["through"]) != strconv.FormatInt(large+1, 10) || publicObject["scope"] != nil {
		t.Fatalf("old CLI catalog envelope changed: %+v", publicObject)
	}
	if raw := marshalObject(public.Checkpoint)["sequence"]; string(raw) != strconv.FormatInt(large+1, 10) {
		t.Fatalf("old CLI checkpoint sequence changed JSON type: %s", raw)
	}
	for _, entry := range public.Entries {
		if entry.Kind != "snapshot" && entry.Kind != "ref" && entry.Kind != "history" && entry.Kind != "protocol" {
			t.Fatalf("private kind leaked to old CLI catalog: %+v", entry)
		}
		if raw := marshalObject(entry)["sequence"]; string(raw) != strconv.FormatInt(entry.Sequence, 10) {
			t.Fatalf("old CLI entry sequence changed JSON type: %s", raw)
		}
	}
	publicFirst := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	publicRawCursor, err := base64.RawURLEncoding.DecodeString(publicFirst.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	publicCursor := catalogJSONPG[map[string]json.RawMessage](t, publicRawCursor)
	if string(publicCursor["through"]) != strconv.FormatInt(large+1, 10) || catalogJSONPG[string](t, publicCursor["scope"]) != "sync-metadata-v1" {
		t.Fatalf("old CLI cursor wire contract changed: %s", publicRawCursor)
	}
}
