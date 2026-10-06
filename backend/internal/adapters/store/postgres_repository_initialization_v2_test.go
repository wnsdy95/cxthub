//go:build postgres

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGRepositoryInitializationCreationAtomicAndOldClient(t *testing.T) {
	st, repo := initializationPG(t)
	ctx := context.Background()
	sentinel := errors.New("rollback protected creation")
	err := st.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
		r, err := st.BeginRepositoryInitialization(tx, repo)
		if err != nil {
			return err
		}
		if r.Repo.ContextProtocol != 1 {
			return errors.New("unprotected receipt")
		}
		if _, err := st.GetRepo(ctx, repo.ID); !errors.Is(err, domain.ErrNotFound) {
			return errors.New("uncommitted repo visible")
		}
		if _, err := initializationPGReadReceipt(st, ctx, repo.ID, "main"); !errors.Is(err, domain.ErrNotFound) {
			return errors.New("uncommitted receipt visible")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := st.GetRepo(ctx, repo.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rollback left repo", err)
	}
	if _, err := initializationPGReadReceipt(st, ctx, repo.ID, "main"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rollback left receipt", err)
	}
	receipt := initializationPGBegin(t, st, repo)
	snap := initializationPGSnapshot(t, st, repo.ID, "first")
	in := initializationPGAnchor(receipt, snap)
	if err := st.CompareAndSwapRef(ctx, repo.ID, in.Anchor.Ref, ""); err == nil {
		t.Fatal("generic CAS permits protected initial legacy")
	}
	anonymous := in.Anchor.Ref
	anonymous.BranchID = ""
	if err := st.CompareAndSwapRef(ctx, repo.ID, anonymous, ""); err == nil {
		t.Fatal("old client anonymous write accepted")
	}
	if err := initializationPGFinalize(st, repo.ID, in); err != nil {
		t.Fatal(err)
	}
	log, err := st.ReadReflog(ctx, repo.ID)
	if err != nil || len(log) != 1 || log[0].Old != "" || log[0].New != snap.ID {
		t.Fatal("initial reflog semantics", log, err)
	}
}

func TestPGRepositoryInitializationExistingProtectedWithoutReceipt(t *testing.T) {
	st, repo := initializationPG(t)
	ctx := context.Background()
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableContextProtocol(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	err := st.WithinRepository(ctx, repo.ID, func(tx context.Context) error { _, err := st.BeginRepositoryInitialization(tx, repo); return err })
	if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("empty protected existing inferred new", err)
	}
}

func TestPGRepositoryInitializationProofRechecksOwnedRows(t *testing.T) {
	for _, mode := range []string{"doc_bytes", "doc_grant", "doc_chunk_bytes", "doc_chunk_grant", "immutable_snapshot_field", "nil_proof", "other_anchor"} {
		t.Run(mode, func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			r := initializationPGBegin(t, st, repo)
			snap := initializationPGSnapshot(t, st, repo.ID, strings.Repeat("synthetic fixture content ", 12000))
			in := initializationPGAnchor(r, snap)
			proof, err := initializationPGPrepare(st, repo.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			var mutation string
			var hash domain.ContentHash
			switch mode {
			case "doc_bytes":
				mutation = `UPDATE blobs SET bytes=bytes WHERE hash=$1`
				hash = snap.DocHash
			case "doc_grant":
				mutation = `DELETE FROM repo_blobs WHERE hash=$1 AND kind='doc'`
				hash = snap.DocHash
			case "doc_chunk_bytes", "doc_chunk_grant":
				for key := range proof.(*initializationProofPG).blobs {
					if key.kind == "chunk" {
						hash = key.hash
						break
					}
				}
				if hash == "" {
					t.Fatal("chunked fixture required")
				}
				if mode == "doc_chunk_bytes" {
					mutation = `UPDATE blobs SET bytes=bytes WHERE hash=$1`
				} else {
					mutation = `DELETE FROM repo_blobs WHERE hash=$1 AND kind='chunk'`
				}
			case "immutable_snapshot_field":
				mutation = `UPDATE snapshots SET session_id='changed' WHERE id=$1`
				hash = snap.ID
			case "nil_proof":
				proof = nil
			case "other_anchor":
				in.Anchor.Ref.Name = "other"
				in.Anchor.Ref.BranchID = domain.LegacyContextBranchID(string(repo.ID), "other")
			}
			if mutation != "" {
				if _, err := st.pool.Exec(ctx, mutation, hash); err != nil {
					t.Fatal(err)
				}
			}
			err = st.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
				_, err := st.FinalizeRepositoryInitialization(tx, repo.ID, in, proof)
				return err
			})
			if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
				t.Fatal("stale/foreign proof accepted", err)
			}
			refs, _ := st.ListRefs(ctx, repo.ID)
			log, _ := st.ReadReflog(ctx, repo.ID)
			got, _ := initializationPGReadReceipt(st, ctx, repo.ID, "main")
			if len(refs) != 0 || len(log) != 0 || got.Anchor != nil {
				t.Fatal("rejected proof left writes")
			}
		})
	}
}

func TestPGRepositoryInitializationLockedObjectDefers(t *testing.T) {
	st, repo := initializationPG(t)
	ctx := context.Background()
	r := initializationPGBegin(t, st, repo)
	snap := initializationPGSnapshot(t, st, repo.ID, "first")
	in := initializationPGAnchor(r, snap)
	proof, err := initializationPGPrepare(st, repo.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var hash string
	if err := tx.QueryRow(ctx, `SELECT hash FROM blobs WHERE hash=$1 FOR UPDATE`, snap.DocHash).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err = st.WithinRepository(bounded, repo.ID, func(write context.Context) error {
		_, err := st.FinalizeRepositoryInitialization(write, repo.ID, in, proof)
		return err
	})
	if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("body writer lock did not explicitly defer", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := initializationPGFinalize(st, repo.ID, in); err != nil {
		t.Fatal("fresh retry after release", err)
	}
}

func TestPGRepositoryInitializationBirthAnchorSerialOrder(t *testing.T) {
	for _, birthFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "anchor_first", true: "birth_first"}[birthFirst], func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			r := initializationPGBegin(t, st, repo)
			snap := initializationPGSnapshot(t, st, repo.ID, "first")
			in := initializationPGAnchor(r, snap)
			proof, err := initializationPGPrepare(st, repo.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo.ID), Kind: "birth", Branch: "main", BranchID: "actual-birth", Source: snap.ID, Target: snap.ID, CreatedAt: time.Now().UTC()}
			entered, release := make(chan struct{}), make(chan struct{})
			first, second := make(chan error, 1), make(chan error, 1)
			apply := func(tx context.Context, birthMode bool) error {
				if birthMode {
					return st.ApplyHistoryEvent(tx, birth)
				}
				_, err := st.FinalizeRepositoryInitialization(tx, repo.ID, in, proof)
				return err
			}
			go func() {
				first <- st.WithinRepository(ctx, repo.ID, func(tx context.Context) error { err := apply(tx, birthFirst); close(entered); <-release; return err })
			}()
			<-entered
			go func() {
				second <- st.WithinRepository(ctx, repo.ID, func(tx context.Context) error { return apply(tx, !birthFirst) })
			}()
			close(release)
			firstErr, secondErr := <-first, <-second
			if firstErr != nil {
				t.Fatal("first writer", firstErr)
			}
			if secondErr == nil {
				t.Fatal("incompatible second writer accepted")
			}
			receipt, _ := initializationPGReadReceipt(st, ctx, repo.ID, "main")
			history, _ := st.ListHistoryEvents(ctx, repo.ID)
			if birthFirst {
				if receipt.Anchor != nil || len(history) != 1 {
					t.Fatal("birth winner corrupted")
				}
			} else {
				if receipt.Anchor == nil || len(history) != 0 {
					t.Fatal("anchor winner corrupted")
				}
			}
		})
	}
}

func TestPGRepositoryInitializationMemoryAndSettingsProof(t *testing.T) {
	for _, mode := range []string{"memory_bytes", "ancestor_grant", "memory_chunk_bytes", "memory_chunk_grant", "settings_changed", "settings_removed"} {
		t.Run(mode, func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			r := initializationPGBegin(t, st, repo)
			snap := initializationPGSnapshot(t, st, repo.ID, "memory settings fixture")
			ancestor, err := st.PutMemory(ctx, repo.ID, domain.MemoryDigest{SnapshotID: snap.ID, Summary: strings.Repeat("synthetic ancestral memory ", 5000)})
			if err != nil {
				t.Fatal(err)
			}
			root, err := st.PutMemory(ctx, repo.ID, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "current memory", PreviousMemoryHash: ancestor})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CompareAndSwapSnapshotMemory(ctx, repo.ID, snap.ID, "", root); err != nil {
				t.Fatal(err)
			}
			bundle := domain.SettingsBundle{Kind: "claude"}
			setting, err := domain.SettingsObjectHash(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutSettingsObject(ctx, repo.ID, setting, bundle); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE snapshots SET claude_settings=$3 WHERE repo_id=$1 AND id=$2`, repo.ID, snap.ID, setting); err != nil {
				t.Fatal(err)
			}
			snap, err = st.GetSnapshot(ctx, repo.ID, snap.ID)
			if err != nil {
				t.Fatal(err)
			}
			in := initializationPGAnchor(r, snap)
			proof, err := initializationPGPrepare(st, repo.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			var query string
			hash := ancestor
			switch mode {
			case "memory_bytes":
				query = `UPDATE blobs SET bytes=bytes WHERE hash=$1`
			case "ancestor_grant":
				query = `DELETE FROM repo_blobs WHERE hash=$1 AND kind='memory'`
			case "memory_chunk_bytes", "memory_chunk_grant":
				hash = ""
				for key := range proof.(*initializationProofPG).blobs {
					if key.kind == "memory_chunk" {
						hash = key.hash
						break
					}
				}
				if hash == "" {
					t.Fatal("chunked memory fixture required")
				}
				if mode == "memory_chunk_bytes" {
					query = `UPDATE blobs SET bytes=bytes WHERE hash=$1`
				} else {
					query = `DELETE FROM repo_blobs WHERE hash=$1 AND kind='memory_chunk'`
				}
			case "settings_changed":
				hash = setting
				query = `UPDATE settings_objects SET data=data WHERE hash=$1`
			case "settings_removed":
				hash = setting
				query = `DELETE FROM settings_objects WHERE hash=$1`
			}
			if _, err := st.pool.Exec(ctx, query, hash); err != nil {
				t.Fatal(err)
			}
			err = st.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
				_, err := st.FinalizeRepositoryInitialization(tx, repo.ID, in, proof)
				return err
			})
			if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
				t.Fatal("changed evidence accepted", err)
			}
			accepted, _ := initializationPGReadReceipt(st, ctx, repo.ID, "main")
			if accepted.Anchor != nil {
				t.Fatal("failed evidence left receipt")
			}
		})
	}
}
