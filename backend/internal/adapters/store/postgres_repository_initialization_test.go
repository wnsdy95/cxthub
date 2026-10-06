//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func initializationPG(t *testing.T) (*PostgresStore, domain.Repo) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("init%d", time.Now().UnixNano())
	user := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: user.ID, OwnerUsername: name, Name: name, Slug: "code", CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, record); err != nil {
		t.Fatal(err)
	}
	return st, domain.Repo{ID: domain.HashContent([]byte(name)), RemoteURL: "https://host.test/" + name + "/code", DefaultBranch: "main", RepositoryID: record.ID}
}
func initializationPGSnapshot(t *testing.T, st *PostgresStore, repo domain.ContentHash, text string, parents ...domain.ContentHash) domain.Snapshot {
	t.Helper()
	ctx := context.Background()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + text}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Parents: parents}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	return snap
}
func initializationPGAnchor(r domain.RepositoryInitializationReceipt, snaps ...domain.Snapshot) domain.RepositoryInitializationFinalize {
	a := domain.RepositoryInitializationAnchor{Ref: domain.Ref{RepoID: r.Repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: domain.LegacyContextBranchID(string(r.Repo.ID), "main"), Target: snaps[len(snaps)-1].ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
	for _, s := range snaps {
		a.SnapshotStates[s.ID], _ = domain.SnapshotStateHash(s)
	}
	return domain.RepositoryInitializationFinalize{CreationID: r.CreationID, Anchor: a}
}
func initializationPGBegin(t *testing.T, st *PostgresStore, repo domain.Repo) domain.RepositoryInitializationReceipt {
	t.Helper()
	var r domain.RepositoryInitializationReceipt
	if err := st.WithinRepository(context.Background(), repo.ID, func(tx context.Context) error {
		var err error
		r, err = st.BeginRepositoryInitialization(tx, repo)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}
func initializationPGPrepare(st *PostgresStore, repo domain.ContentHash, in domain.RepositoryInitializationFinalize) (outbound.RepositoryInitializationProof, error) {
	var proof outbound.RepositoryInitializationProof
	err := st.WithinReadSnapshot(context.Background(), func(read context.Context) error {
		var evidence outbound.RepositoryInitializationEvidence
		for id := range in.Anchor.SnapshotStates {
			snap, err := st.GetSnapshot(read, repo, id)
			if err != nil {
				return err
			}
			doc, err := st.VerifyStoredDoc(read, repo, snap.DocHash)
			if err != nil {
				return err
			}
			if !doc.Matches(snap) {
				return domain.ErrIntegrity
			}
			evidence.Snapshots = append(evidence.Snapshots, snap)
			for hash, depth := snap.MemoryHash, 0; hash != ""; depth++ {
				if depth >= 1024 {
					return domain.ErrIntegrity
				}
				memory, err := st.GetMemory(read, repo, hash)
				if err != nil {
					return err
				}
				got, err := domain.MemoryDigestHash(memory)
				if err != nil {
					return err
				}
				if got != hash || memory.SnapshotID != snap.ID {
					return domain.ErrIntegrity
				}
				evidence.Memories = append(evidence.Memories, hash)
				hash = memory.PreviousMemoryHash
			}
			for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
				if hash != "" {
					if _, err := st.GetSettingsObject(read, repo, hash); err != nil {
						return err
					}
				}
			}
		}
		var err error
		proof, err = st.CaptureRepositoryInitialization(read, repo, in.Anchor, evidence)
		return err
	})
	return proof, err
}
func initializationPGFinalize(st *PostgresStore, repo domain.ContentHash, in domain.RepositoryInitializationFinalize) error {
	proof, err := initializationPGPrepare(st, repo, in)
	if err != nil {
		return err
	}
	return st.WithinRepository(context.Background(), repo, func(tx context.Context) error {
		_, err := st.FinalizeRepositoryInitialization(tx, repo, in, proof)
		return err
	})
}

func TestPGRepositoryInitializationPendingCommitReplay(t *testing.T) {
	st, repo := initializationPG(t)
	ctx := context.Background()
	receipt := initializationPGBegin(t, st, repo)
	// A normal pending/object registration cannot erase the prepared receipt.
	input := repo
	input.ContextProtocol = 1
	input.DefaultBranch = "must-not-replace-main"
	if _, err := st.PutRepo(ctx, input); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetRepo(ctx, repo.ID)
	if err != nil || (current.ContextProtocol != 1 || current.DefaultBranch != "main") {
		t.Fatal("registration upgraded or replaced default", err)
	}
	pendingSnap := initializationPGSnapshot(t, st, repo.ID, "live capture")
	p := domain.Pending{RepoID: repo.ID, SessionID: "fixture-session", Branch: "main", Provider: domain.ProviderClaude, Target: pendingSnap.ID, UpdatedAt: time.Now().UTC()}
	if err := st.PutPending(ctx, repo.ID, p); err != nil {
		t.Fatal(err)
	}
	commit := initializationPGSnapshot(t, st, repo.ID, "initial commit", pendingSnap.ID)
	u := domain.Unsync{RepoID: repo.ID, User: "fixture-user", Branch: "main", Target: commit.ID, UpdatedAt: time.Now().UTC()}
	if err := st.PutUnsync(ctx, repo.ID, u); err != nil {
		t.Fatal(err)
	}
	settings := domain.SettingsBundle{Kind: "claude"}
	if err := st.PutSettingsBundle(ctx, repo.ID, settings); err != nil {
		t.Fatal(err)
	}
	beforeSettings, err := st.GetSettingsBundle(ctx, repo.ID, "claude")
	if err != nil {
		t.Fatal(err)
	}
	beforeP, _ := st.ListPendings(ctx, repo.ID)
	beforeU, _ := st.ListUnsyncs(ctx, repo.ID)
	final := initializationPGAnchor(receipt, pendingSnap, commit)
	if err := initializationPGFinalize(st, repo.ID, final); err != nil {
		t.Fatal(err)
	}
	afterSettings, err := st.GetSettingsBundle(ctx, repo.ID, "claude")
	if err != nil || !reflect.DeepEqual(beforeSettings, afterSettings) {
		t.Fatal("settings changed", err)
	}
	afterP, _ := st.ListPendings(ctx, repo.ID)
	afterU, _ := st.ListUnsyncs(ctx, repo.ID)
	if !reflect.DeepEqual(beforeP, afterP) || !reflect.DeepEqual(beforeU, afterU) {
		t.Fatal("initialization reconciled pending/unsync")
	}
	current, err = st.GetRepo(ctx, repo.ID)
	if err != nil || current.ContextProtocol != 1 {
		t.Fatal("missing protocol 1", err)
	}
	history, err := st.ListHistoryEvents(ctx, repo.ID)
	if err != nil || len(history) != 0 {
		t.Fatal("fabricated birth", err)
	}
	later := initializationPGSnapshot(t, st, repo.ID, "later", commit.ID)
	next := final.Anchor.Ref
	next.Target = later.ID
	if err := st.CompareAndSwapRef(ctx, repo.ID, next, commit.ID); err != nil {
		t.Fatal(err)
	}
	logBefore, _ := st.ReadReflog(ctx, repo.ID)
	if err := initializationPGFinalize(st, repo.ID, final); err != nil {
		t.Fatal("accepted replay", err)
	}
	logAfter, _ := st.ReadReflog(ctx, repo.ID)
	got, _ := st.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if got.Target != later.ID || len(logBefore) != len(logAfter) {
		t.Fatal("retry overwrote advanced ref")
	}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	current, _ = st.GetRepo(ctx, repo.ID)
	if current.ContextProtocol != 1 {
		t.Fatal("registration downgraded completion")
	}
	replay := initializationPGBegin(t, st, repo)
	if replay.CreationID != receipt.CreationID || replay.Anchor != nil {
		t.Fatal("receipt not recoverable after completion")
	}
	changed := initializationPGAnchor(receipt, pendingSnap, commit, later)
	if err := initializationPGFinalize(st, repo.ID, changed); !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("changed completion", err)
	}
	// The schema itself protects immutable receipts against accidental updates.
	if _, err := st.pool.Exec(ctx, "UPDATE repository_initialization_anchors SET anchor='{}'::jsonb WHERE repo_id=$1", repo.ID); err == nil {
		t.Fatal("schema accepted receipt removal")
	}
}

func TestPGRepositoryInitializationRollbackAndVisibility(t *testing.T) {
	st, repo := initializationPG(t)
	ctx := context.Background()
	receipt := initializationPGBegin(t, st, repo)
	snap := initializationPGSnapshot(t, st, repo.ID, "initial")
	final := initializationPGAnchor(receipt, snap)
	proof, err := initializationPGPrepare(st, repo.ID, final)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("fixture failure after all writes")
	err = st.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
		if _, err := st.FinalizeRepositoryInitialization(tx, repo.ID, final, proof); err != nil {
			return err
		}
		// Separate pool reads cannot see the uncommitted protocol, anchor or receipt.
		got, err := st.GetRepo(ctx, repo.ID)
		if err != nil {
			return err
		}
		if got.ContextProtocol != 1 {
			return errors.New("protocol leaked")
		}
		if _, err := st.GetRef(ctx, repo.ID, domain.RefBranch, "main"); !errors.Is(err, domain.ErrNotFound) {
			return errors.New("anchor leaked")
		}
		r, err := initializationPGReadReceipt(st, ctx, repo.ID, "main")
		if err != nil {
			return err
		}
		if r.Anchor != nil {
			return errors.New("completion leaked")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	r, err := initializationPGReadReceipt(st, ctx, repo.ID, "main")
	if err != nil || r.Anchor != nil {
		t.Fatal("completion survived rollback", err)
	}
	got, err := st.GetRepo(ctx, repo.ID)
	if err != nil || got.ContextProtocol != 1 {
		t.Fatal("protocol survived rollback", err)
	}
	if _, err := st.GetRef(ctx, repo.ID, domain.RefBranch, "main"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("ref survived rollback", err)
	}
	log, _ := st.ReadReflog(ctx, repo.ID)
	if len(log) != 0 {
		t.Fatal("reflog survived rollback")
	}
	if err := initializationPGFinalize(st, repo.ID, final); err != nil {
		t.Fatal("retry after rollback", err)
	}
}

func TestPGRepositoryInitializationExistingAndConcurrentIntents(t *testing.T) {
	t.Run("existing_empty", func(t *testing.T) {
		st, repo := initializationPG(t)
		ctx := context.Background()
		if _, err := st.PutRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
		err := st.WithinRepository(ctx, repo.ID, func(tx context.Context) error { _, err := st.BeginRepositoryInitialization(tx, repo); return err })
		if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
			t.Fatal("existing empty upgraded", err)
		}
	})
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_%v", same), func(t *testing.T) {
			st, repo := initializationPG(t)
			start := make(chan struct{})
			results := make(chan error, 2)
			receipts := make(chan domain.RepositoryInitializationReceipt, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				in := repo
				if i == 1 && !same {
					in.DefaultBranch = "other"
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					var r domain.RepositoryInitializationReceipt
					err := st.WithinRepository(context.Background(), repo.ID, func(tx context.Context) error {
						var err error
						r, err = st.BeginRepositoryInitialization(tx, in)
						return err
					})
					results <- err
					receipts <- r
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			close(receipts)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
					t.Fatal(err)
				}
			}
			want := 1
			if same {
				want = 2
			}
			if successes != want {
				t.Fatalf("successes=%d want=%d", successes, want)
			}
			var creation string
			for r := range receipts {
				if r.CreationID == "" {
					continue
				}
				if creation != "" && creation != r.CreationID {
					t.Fatal("multiple creation receipts")
				}
				creation = r.CreationID
			}
		})
	}
}

func TestPGRepositoryInitializationFinalizeRaces(t *testing.T) {
	for _, mode := range []string{"same_finalize", "different_finalize", "writer_first", "registration_first"} {
		t.Run(mode, func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			if mode == "registration_first" {
				entered, release := make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- st.WithinRepository(ctx, repo.ID, func(tx context.Context) error { _, err := st.PutRepo(tx, repo); close(entered); <-release; return err })
				}()
				<-entered
				begin := make(chan error, 1)
				go func() {
					begin <- st.WithinRepository(ctx, repo.ID, func(tx context.Context) error { _, err := st.BeginRepositoryInitialization(tx, repo); return err })
				}()
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if err := <-begin; !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
					t.Fatal("raced registration inferred newness", err)
				}
				return
			}
			receipt := initializationPGBegin(t, st, repo)
			snap := initializationPGSnapshot(t, st, repo.ID, "initial")
			first := initializationPGAnchor(receipt, snap)
			if mode == "writer_first" {
				entered, release := make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- st.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
						r := first.Anchor.Ref
						r.Name = "competitor"
						r.Kind = domain.RefTag
						r.BranchID = ""
						err := st.CompareAndSwapRef(tx, repo.ID, r, "")
						close(entered)
						<-release
						return err
					})
				}()
				<-entered
				finish := make(chan error, 1)
				go func() { finish <- initializationPGFinalize(st, repo.ID, first) }()
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if err := <-finish; err != nil {
					t.Fatal("unrelated tag blocked observation", err)
				}
				got, _ := st.GetRepo(ctx, repo.ID)
				if got.ContextProtocol != 1 {
					t.Fatal("competing writer upgraded")
				}
				return
			}
			second := initializationPGAnchor(receipt, snap)
			if mode == "different_finalize" {
				second.Anchor.Ref.Name = "selected-other"
				second.Anchor.Ref.BranchID = domain.LegacyContextBranchID(string(repo.ID), second.Anchor.Ref.Name)
			}
			start := make(chan struct{})
			done := make(chan error, 2)
			var wg sync.WaitGroup
			for _, in := range []domain.RepositoryInitializationFinalize{first, second} {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; done <- initializationPGFinalize(st, repo.ID, in) }()
			}
			close(start)
			wg.Wait()
			close(done)
			successes := 0
			for err := range done {
				if err == nil {
					successes++
				} else if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
					t.Fatal(err)
				}
			}
			want := 2
			if successes != want {
				t.Fatal("wrong winners", successes)
			}
			refs, _ := st.ListRefs(ctx, repo.ID)
			wantRefs := 1
			if mode == "different_finalize" {
				wantRefs = 2
			}
			if len(refs) != wantRefs {
				t.Fatal("wrong branch observations", len(refs))
			}
		})
	}
}

// Tests explicitly compose the response; production Begin never returns Anchor.
func initializationPGReadReceipt(st *PostgresStore, ctx context.Context, repo domain.ContentHash, branch string) (domain.RepositoryInitializationReceipt, error) {
	r, err := st.GetRepositoryInitialization(ctx, repo)
	if err != nil {
		return r, err
	}
	a, err := st.GetRepositoryInitializationAnchor(ctx, repo, branch)
	if errors.Is(err, domain.ErrNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.Anchor = &a
	return r, nil
}
