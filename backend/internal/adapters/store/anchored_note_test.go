package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func anchoredNoteStoreNote(repo domain.ContentHash, letter, author string, minute int, target domain.AnchoredNoteTarget) domain.AnchoredNote {
	at := time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC)
	return domain.AnchoredNote{RepoID: repo, ID: "note_" + strings.Repeat(letter, 32), Target: target, Author: author,
		Body: "note " + letter, CreatedAt: at, UpdatedAt: at}
}

// checkAnchoredNotePersistence is the contract both stores keep.
func checkAnchoredNotePersistence(t *testing.T, ctx context.Context, storage outbound.AnchoredNoteStore, repo domain.ContentHash) {
	t.Helper()
	if listed, err := storage.ListAnchoredNotes(ctx, repo, "member-a"); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no notes yet: %#v %v", listed, err)
	}
	commit := anchoredNoteStoreNote(repo, "b", "member-a", 2, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: strings.Repeat("a", 40)})
	branch := anchoredNoteStoreNote(repo, "a", "member-a", 5, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteBranch, Branch: "feature", LedgerID: strings.Repeat("c", 32)})
	worktree := anchoredNoteStoreNote(repo, "c", "member-b", 1, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteWorktree, Worktree: "app", Machine: "laptop"})
	for _, note := range []domain.AnchoredNote{branch, commit, worktree} {
		if err := storage.PutAnchoredNote(ctx, note); err != nil {
			t.Fatal(err)
		}
	}
	// One author's notes, oldest first; another author's are not among them.
	if listed, err := storage.ListAnchoredNotes(ctx, repo, "member-a"); err != nil || !reflect.DeepEqual(listed, []domain.AnchoredNote{commit, branch}) {
		t.Fatalf("member-a's notes:\n got %+v\nwant %+v\n%v", listed, []domain.AnchoredNote{commit, branch}, err)
	}
	if got, err := storage.GetAnchoredNote(ctx, repo, worktree.ID); err != nil || !reflect.DeepEqual(got, worktree) {
		t.Fatalf("one note: %+v %v", got, err)
	}
	// Every author's notes, oldest first.
	if listed, err := storage.ListRepositoryAnchoredNotes(ctx, repo); err != nil || !reflect.DeepEqual(listed, []domain.AnchoredNote{worktree, commit, branch}) {
		t.Fatalf("the repository's notes:\n got %+v\nwant %+v\n%v", listed, []domain.AnchoredNote{worktree, commit, branch}, err)
	}
	if listed, err := storage.ListRepositoryAnchoredNotes(ctx, domain.HashContent([]byte(string(repo)+"/elsewhere"))); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("the notes of another repository: %#v %v", listed, err)
	}
	// A note is replaced as a whole.
	commit.Body, commit.UpdatedAt = "rewritten", commit.UpdatedAt.Add(time.Hour)
	if err := storage.PutAnchoredNote(ctx, commit); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetAnchoredNote(ctx, repo, commit.ID); err != nil || !reflect.DeepEqual(got, commit) {
		t.Fatalf("a rewritten note: %+v %v", got, err)
	}
	if listed, _ := storage.ListAnchoredNotes(ctx, repo, "member-a"); len(listed) != 2 {
		t.Fatalf("rewriting added a note: %+v", listed)
	}
	// A malformed note is refused and changes nothing.
	broken := commit
	broken.Body = ""
	if err := storage.PutAnchoredNote(ctx, broken); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a note without text: %v", err)
	}
	if err := storage.DeleteAnchoredNote(ctx, repo, branch.ID); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteAnchoredNote(ctx, repo, branch.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleting a note twice: %v", err)
	}
	if _, err := storage.GetAnchoredNote(ctx, repo, branch.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a deleted note: %v", err)
	}
	if listed, err := storage.ListAnchoredNotes(ctx, repo, "member-a"); err != nil || len(listed) != 1 || listed[0].Body != "rewritten" {
		t.Fatalf("after deleting: %+v %v", listed, err)
	}
}

// checkReportedWorktreePersistence is the contract both stores keep.
func checkReportedWorktreePersistence(t *testing.T, ctx context.Context, storage outbound.ReportedWorktreeStore, repo domain.ContentHash) {
	t.Helper()
	if listed, err := storage.ListReportedWorktrees(ctx, repo, "member-a"); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no worktrees yet: %#v %v", listed, err)
	}
	at := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	own := []domain.ReportedWorktree{
		{Machine: "laptop", Name: "app-fix", Branch: "fix/typo", Removed: true, FirstSeenAt: at, LastSeenAt: at.Add(time.Hour)},
		{Machine: "desktop", Name: "app", Branch: "main", FirstSeenAt: at, LastSeenAt: at},
		{Machine: "laptop", Name: "app", FirstSeenAt: at, LastSeenAt: at},
	}
	if err := storage.PutReportedWorktrees(ctx, repo, "member-a", own); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutReportedWorktrees(ctx, repo, "member-b", []domain.ReportedWorktree{{Name: "elsewhere", FirstSeenAt: at, LastSeenAt: at}}); err != nil {
		t.Fatal(err)
	}
	want := []domain.ReportedWorktree{own[1], own[2], own[0]}
	if listed, err := storage.ListReportedWorktrees(ctx, repo, "member-a"); err != nil || !reflect.DeepEqual(listed, want) {
		t.Fatalf("member-a's worktrees, by machine then name:\n got %+v\nwant %+v\n%v", listed, want, err)
	}
	// The member's whole list is replaced at once.
	if err := storage.PutReportedWorktrees(ctx, repo, "member-a", own[1:2]); err != nil {
		t.Fatal(err)
	}
	if listed, err := storage.ListReportedWorktrees(ctx, repo, "member-a"); err != nil || !reflect.DeepEqual(listed, own[1:2]) {
		t.Fatalf("a replaced list: %+v %v", listed, err)
	}
	if listed, err := storage.ListReportedWorktrees(ctx, repo, "member-b"); err != nil || len(listed) != 1 || listed[0].Name != "elsewhere" {
		t.Fatalf("another member's list changed: %+v %v", listed, err)
	}
	for name, refused := range map[string][]domain.ReportedWorktree{
		"a path":               {{Name: "work/app", FirstSeenAt: at, LastSeenAt: at}},
		"one worktree twice":   {own[1], own[1]},
		"seen before it began": {{Name: "app", FirstSeenAt: at, LastSeenAt: at.Add(-time.Hour)}},
	} {
		if err := storage.PutReportedWorktrees(ctx, repo, "member-a", refused); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was stored: %v", name, err)
		}
	}
	if err := storage.PutReportedWorktrees(ctx, repo, " ", own[1:2]); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a list without its account was stored: %v", err)
	}
}

func TestFSAnchoredNotePersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	checkAnchoredNotePersistence(t, context.Background(), storage, repo)

	// A note changed on disk into one that is not well formed is an
	// integrity failure, not another note.
	path := storage.anchoredNotePath(repo)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"kind":"commit"`, `"kind":"elsewhere"`, 1)
	if tampered == string(raw) {
		t.Fatal("the stored list does not hold the target to change")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListAnchoredNotes(context.Background(), repo, "member-a"); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a malformed note on disk: %v", err)
	}
}

func TestFSReportedWorktreePersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	checkReportedWorktreePersistence(t, context.Background(), storage, repo)
	// No account identifier becomes a file name.
	entries, err := os.ReadDir(storage.repoDir(repo) + "/reported-worktrees")
	if err != nil || len(entries) != 2 {
		t.Fatalf("stored lists: %v %v", entries, err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "member") {
			t.Errorf("an account identifier is a file name: %s", entry.Name())
		}
	}
}
