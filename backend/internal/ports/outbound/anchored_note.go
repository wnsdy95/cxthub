package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredNoteStore keeps the manual notes members wrote about commits,
// branches and worktrees. A note is stored whole and replaced whole.
type AnchoredNoteStore interface {
	PutAnchoredNote(context.Context, domain.AnchoredNote) error
	// GetAnchoredNote answers ErrNotFound for an identifier nothing is stored under.
	GetAnchoredNote(context.Context, domain.ContentHash, string) (domain.AnchoredNote, error)
	// ListRepositoryAnchoredNotes lists every author's notes, oldest first.
	ListRepositoryAnchoredNotes(context.Context, domain.ContentHash) ([]domain.AnchoredNote, error)
	// DeleteAnchoredNote answers ErrNotFound for an identifier nothing is stored under.
	DeleteAnchoredNote(context.Context, domain.ContentHash, string) error
}

// ReportedWorktreeStore keeps, per member, the worktrees that member's CLIs
// listed. The member's whole list is replaced at once.
type ReportedWorktreeStore interface {
	// ListReportedWorktrees lists one account's worktrees by machine, then name.
	ListReportedWorktrees(context.Context, domain.ContentHash, string) ([]domain.ReportedWorktree, error)
	PutReportedWorktrees(context.Context, domain.ContentHash, string, []domain.ReportedWorktree) error
}
