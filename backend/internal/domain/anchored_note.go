package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// A manual note is what a person wrote about a commit, a branch or a worktree
// that no session explains. It is kept apart from captured sessions, carries
// its author and time, and stays when a session is linked to the same target
// later.

const (
	AnchoredNoteCommit   = "commit"
	AnchoredNoteBranch   = "branch"
	AnchoredNoteWorktree = "worktree"

	// AnchoredNoteBodyLimit bounds the text of one note, in bytes.
	AnchoredNoteBodyLimit = 16 << 10
)

var anchoredNoteID = regexp.MustCompile(`^note_[0-9a-f]{32}$`)

// AnchoredNoteTarget is what a note is attached to. A branch is named, and
// also carries the ledger ID of the lifetime the note belongs to when the
// remote ever held the name; a worktree is named with its machine label,
// never by a path.
type AnchoredNoteTarget struct {
	Kind     string `json:"kind"`
	Commit   string `json:"commit,omitempty"`
	Branch   string `json:"branch,omitempty"`
	LedgerID string `json:"ledger_id,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Machine  string `json:"machine,omitempty"`
}

// AnchoredNote is one stored manual note.
type AnchoredNote struct {
	RepoID    ContentHash        `json:"repo_id"`
	ID        string             `json:"id"`
	Target    AnchoredNoteTarget `json:"target"`
	Author    string             `json:"author"`
	Body      string             `json:"body"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// NewAnchoredNoteID returns an identifier for a new note.
func NewAnchoredNoteID() string { return NewID("note_") }

// ValidAnchoredNoteID reports whether id can name a stored note.
func ValidAnchoredNoteID(id string) bool { return anchoredNoteID.MatchString(id) }

// Validate checks that a target names exactly one thing of its kind.
func (t AnchoredNoteTarget) Validate() error {
	invalid := func(what string) error { return fmt.Errorf("%w: note target: %s", ErrValidation, what) }
	switch t.Kind {
	case AnchoredNoteCommit:
		if !ValidAnchoredCommitSHA(t.Commit) || t.Branch != "" || t.LedgerID != "" || t.Worktree != "" || t.Machine != "" {
			return invalid("a commit is named by its full number alone")
		}
	case AnchoredNoteBranch:
		if t.Commit != "" || t.Worktree != "" || t.Machine != "" || t.Branch == "" || ValidateBranchLedgerName(t.Branch) != nil {
			return invalid("a branch is named by its name")
		}
		if t.LedgerID != "" && !ValidBranchLedgerID(t.LedgerID) {
			return invalid("a branch lifetime is named by its ledger ID")
		}
	case AnchoredNoteWorktree:
		if t.Commit != "" || t.Branch != "" || t.LedgerID != "" || !anchoredText(t.Worktree, anchoredNameLimit, true) ||
			strings.ContainsAny(t.Worktree, `/\`) || !anchoredText(t.Machine, anchoredNameLimit, false) {
			return invalid("a worktree is named by its name, not a path, and its machine label")
		}
	default:
		return invalid("unknown kind")
	}
	return nil
}

// NormalizeAnchoredNoteBody returns the text a note is stored with: trimmed,
// and with known secret formats masked the way a captured session is.
func NormalizeAnchoredNoteBody(body string) (string, error) {
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return "", fmt.Errorf("%w: note text must be plain text", ErrValidation)
	}
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if body == "" {
		return "", fmt.Errorf("%w: note text is empty", ErrValidation)
	}
	if len(body) > AnchoredNoteBodyLimit {
		return "", fmt.Errorf("%w: note text is longer than %d bytes", ErrValidation, AnchoredNoteBodyLimit)
	}
	body = ScrubSecretPatterns(body)
	if len(body) > AnchoredNoteBodyLimit {
		return "", fmt.Errorf("%w: note text is longer than %d bytes", ErrValidation, AnchoredNoteBodyLimit)
	}
	return body, nil
}

// Validate checks a stored note.
func (n AnchoredNote) Validate() error {
	if ValidateContentHash(n.RepoID) != nil || !ValidAnchoredNoteID(n.ID) {
		return fmt.Errorf("%w: note key", ErrValidation)
	}
	if err := n.Target.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(n.Author) == "" || n.CreatedAt.IsZero() || n.UpdatedAt.IsZero() || n.UpdatedAt.Before(n.CreatedAt) {
		return fmt.Errorf("%w: note attribution required", ErrValidation)
	}
	if n.Body == "" || len(n.Body) > AnchoredNoteBodyLimit || !utf8.ValidString(n.Body) {
		return fmt.Errorf("%w: note text", ErrValidation)
	}
	return nil
}
