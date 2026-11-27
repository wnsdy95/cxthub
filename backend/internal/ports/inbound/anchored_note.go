package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredNotes reads and writes the manual notes of the signed-in member.
type AnchoredNotes interface {
	ListAnchoredNotes(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredNote, error)
	CreateAnchoredNote(ctx context.Context, repo domain.ContentHash, target domain.AnchoredNoteTarget, body string) (domain.AnchoredNote, error)
	UpdateAnchoredNote(ctx context.Context, repo domain.ContentHash, id, body string) (domain.AnchoredNote, error)
	DeleteAnchoredNote(ctx context.Context, repo domain.ContentHash, id string) error
}

// ReportedWorktrees takes the worktree list a member's CLI sends for one
// machine, and reads that member's worktrees back.
type ReportedWorktrees interface {
	ReportWorktrees(ctx context.Context, repo domain.ContentHash, report domain.WorktreeReport) ([]domain.ReportedWorktree, error)
	ListReportedWorktrees(ctx context.Context, repo domain.ContentHash) ([]domain.ReportedWorktree, error)
}
