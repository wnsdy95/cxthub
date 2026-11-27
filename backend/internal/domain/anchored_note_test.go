package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAnchoredNoteTargetNamesExactlyOneThing(t *testing.T) {
	ledger := strings.Repeat("a", 32)
	for name, target := range map[string]AnchoredNoteTarget{
		"a commit":              {Kind: AnchoredNoteCommit, Commit: anchoredTestSHA("a")},
		"a branch":              {Kind: AnchoredNoteBranch, Branch: "feature/login"},
		"a lifetime of a name":  {Kind: AnchoredNoteBranch, Branch: "feature/login", LedgerID: ledger},
		"a worktree":            {Kind: AnchoredNoteWorktree, Worktree: "app-fix", Machine: "work laptop"},
		"a worktree, unlabeled": {Kind: AnchoredNoteWorktree, Worktree: "app-fix"},
	} {
		if err := target.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, target := range map[string]AnchoredNoteTarget{
		"no kind":                     {Commit: anchoredTestSHA("a")},
		"an unknown kind":             {Kind: "pull", Commit: anchoredTestSHA("a")},
		"a short commit number":       {Kind: AnchoredNoteCommit, Commit: "abc1234"},
		"a commit that also names":    {Kind: AnchoredNoteCommit, Commit: anchoredTestSHA("a"), Branch: "main"},
		"a branch without a name":     {Kind: AnchoredNoteBranch},
		"a branch name Git refuses":   {Kind: AnchoredNoteBranch, Branch: "bad..name"},
		"a ledger ID in another form": {Kind: AnchoredNoteBranch, Branch: "main", LedgerID: "first"},
		"a branch on a machine":       {Kind: AnchoredNoteBranch, Branch: "main", Machine: "laptop"},
		"a worktree path":             {Kind: AnchoredNoteWorktree, Worktree: "work/app"},
		"a worktree without a name":   {Kind: AnchoredNoteWorktree, Machine: "laptop"},
		"a worktree with a branch":    {Kind: AnchoredNoteWorktree, Worktree: "app", Branch: "main"},
	} {
		if err := target.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

func TestAnchoredNoteBodyIsTrimmedBoundedAndMasked(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	body, err := NormalizeAnchoredNoteBody("  Rotated the key.\r\nOld one: " + token + "\n\n")
	if err != nil || body != "Rotated the key.\nOld one: «redacted:github-token»" {
		t.Fatalf("body = %q, %v", body, err)
	}
	for name, text := range map[string]string{
		"empty": "", "only space": " \n\t ", "a NUL": "a\x00b", "broken text": "a\xffb", "too long": strings.Repeat("x", AnchoredNoteBodyLimit+1),
	} {
		if _, err := NormalizeAnchoredNoteBody(text); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	if kept, err := NormalizeAnchoredNoteBody(strings.Repeat("x", AnchoredNoteBodyLimit)); err != nil || len(kept) != AnchoredNoteBodyLimit {
		t.Errorf("a note at the limit: %d %v", len(kept), err)
	}
}

func TestAnchoredNoteValidatesWhatIsStored(t *testing.T) {
	at := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	note := AnchoredNote{RepoID: HashContent([]byte("repo")), ID: NewAnchoredNoteID(), Target: AnchoredNoteTarget{Kind: AnchoredNoteCommit, Commit: anchoredTestSHA("a")},
		Author: "member", Body: "why this commit exists", CreatedAt: at, UpdatedAt: at.Add(time.Minute)}
	if err := note.Validate(); err != nil {
		t.Fatal(err)
	}
	if !ValidAnchoredNoteID(note.ID) || ValidAnchoredNoteID("note_1") || ValidAnchoredNoteID(strings.Repeat("a", 37)) {
		t.Error("note identifiers")
	}
	for name, change := range map[string]func(*AnchoredNote){
		"another form of ID":  func(n *AnchoredNote) { n.ID = "note" },
		"no author":           func(n *AnchoredNote) { n.Author = " " },
		"no text":             func(n *AnchoredNote) { n.Body = "" },
		"changed before made": func(n *AnchoredNote) { n.UpdatedAt = at.Add(-time.Second) },
		"a target of no kind": func(n *AnchoredNote) { n.Target.Kind = "" },
		"no repository":       func(n *AnchoredNote) { n.RepoID = "" },
		"text over the limit": func(n *AnchoredNote) { n.Body = strings.Repeat("x", AnchoredNoteBodyLimit+1) },
		"no creation time":    func(n *AnchoredNote) { n.CreatedAt = time.Time{} },
	} {
		changed := note
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}
