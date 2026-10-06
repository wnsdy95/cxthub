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

func TestPGRepositoryInitializationBranchEvidenceGuards(t *testing.T) {
	for _, mode := range []string{"same_ordinary", "foreign_ordinary", "birth", "previous_name", "archived", "selected_identity_elsewhere", "reflog_only", "old_lifecycle_tag", "dependent_conflicting_identity"} {
		t.Run(mode, func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			r := initializationPGBegin(t, st, repo)
			snap := initializationPGSnapshot(t, st, repo.ID, "checkpoint")
			in := initializationPGAnchor(r, snap)
			e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), Branch: "main", BranchID: in.Anchor.Ref.BranchID, Kind: "position", CreatedAt: time.Now().UTC()}
			switch mode {
			case "foreign_ordinary":
				e.BranchID = "another-main"
			case "birth":
				e.Kind = "birth"
				e.BranchID = "modern-main"
			case "previous_name":
				e.Kind = "rename"
				e.Branch = "renamed"
				e.PreviousBranch = "main"
			case "archived":
				e.Kind = "archive"
			case "selected_identity_elsewhere":
				e.Kind = "birth"
				e.Branch = "other"
			case "reflog_only":
				if _, err := st.pool.Exec(ctx, `INSERT INTO reflog(repo_id,kind,name,old,new) VALUES($1,'branch','main','',$2)`, repo.ID, snap.ID); err != nil {
					t.Fatal(err)
				}
			case "old_lifecycle_tag":
				tag, err := domain.NewBranchLifecycleRef(repo.ID, "main", snap.ID, 1, domain.BranchArchived)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.CompareAndSwapRef(ctx, repo.ID, tag, ""); err != nil {
					t.Fatal(err)
				}
			case "dependent_conflicting_identity":
				// Stored ordinary evidence supports a foreign birth's origin, but cannot
				// authorize replacing the name with a deterministic legacy identity.
				e.BranchID = "previous-main"
				if err := st.ApplyHistoryEvent(ctx, e); err != nil {
					t.Fatal(err)
				}
				e = domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(repo.ID), Branch: "feature", BranchID: "foreign-feature", Kind: "birth", CreatedAt: time.Now().UTC(), Creation: &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature"}, StartRef: "HEAD", OriginBranch: "main", OriginBranchID: "previous-main"}}
			}
			if mode == "previous_name" || mode == "archived" {
				birth := domain.HistoryEvent{ID: strings.Repeat("0", 32), RepoID: string(repo.ID), Branch: "main", BranchID: "historical-main", Kind: "birth", Source: snap.ID, Target: snap.ID, CreatedAt: time.Now().UTC()}
				if err := st.ApplyHistoryEvent(ctx, birth); err != nil {
					t.Fatal("fixture birth", err)
				}
				e.BranchID = birth.BranchID
				e.BindingParent = birth.ID
				e.Source = snap.ID
				e.Target = snap.ID
			}
			if mode != "reflog_only" && mode != "old_lifecycle_tag" {
				if err := st.ApplyHistoryEvent(ctx, e); err != nil {
					t.Fatal("fixture history", err)
				}
			}
			err := initializationPGFinalize(st, repo.ID, in)
			if mode == "same_ordinary" {
				if err != nil {
					t.Fatal("ordinary evidence blocked", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
				t.Fatal("unsafe branch observation", err)
			}
			if _, err := st.GetRepositoryInitializationAnchor(ctx, repo.ID, "main"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("rejection accepted anchor", err)
			}
		})
	}
}

func TestPGRepositoryInitializationMutableMetadataContinuity(t *testing.T) {
	for _, mode := range []string{"default", "protect", "filled_origin", "different_original_origin", "binding", "protocol"} {
		t.Run(mode, func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			if mode == "different_original_origin" {
				repo.GitRemoteURL = "https://git.test/original"
			}
			r := initializationPGBegin(t, st, repo)
			snap := initializationPGSnapshot(t, st, repo.ID, "checkpoint")
			in := initializationPGAnchor(r, snap)
			statement := ""
			switch mode {
			case "default":
				statement = `UPDATE repos SET default_branch='trunk' WHERE id=$1`
			case "protect":
				statement = `UPDATE repos SET protect_default=true WHERE id=$1`
			case "filled_origin", "different_original_origin":
				statement = `UPDATE repos SET git_remote_url='https://git.test/new' WHERE id=$1`
			case "binding":
				statement = `UPDATE repos SET repository_id=NULL WHERE id=$1`
			case "protocol":
				statement = `UPDATE repos SET context_protocol=0 WHERE id=$1`
			}
			if _, err := st.pool.Exec(ctx, statement, repo.ID); err != nil {
				t.Fatal(err)
			}
			err := initializationPGFinalize(st, repo.ID, in)
			unsafe := mode == "different_original_origin" || mode == "binding" || mode == "protocol"
			if unsafe {
				if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
					t.Fatal("changed authority accepted", err)
				}
			} else if err != nil {
				t.Fatal("mutable metadata stranded branch", err)
			}
		})
	}
}
