package gitctx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// BranchSummaryGiven keeps, beside the collector's files in Git's own
// directory, one small file for each session: the branch the session was last
// given a summary for. The worktrees of a repository share it.
type BranchSummaryGiven struct{}

// NewBranchSummaryGiven creates the adapter.
func NewBranchSummaryGiven() *BranchSummaryGiven { return &BranchSummaryGiven{} }

const (
	branchSummaryGivenDir = "summary-given-v1"
	// A session that was not given anything for this long is over.
	branchSummaryGivenAge = 60 * 24 * time.Hour
	branchSummaryGivenMax = 4 << 10
)

func branchSummaryGivenFile(ctx context.Context, cwd string, provider domain.ProviderKind, sessionID string) (string, error) {
	if (provider != domain.ProviderClaude && provider != domain.ProviderCodex) || !domain.ValidAnchoredSessionID(sessionID) {
		return "", domain.ErrInvalidRef
	}
	dir, err := collectorDir(ctx, cwd)
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, branchSummaryGivenDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, string(provider)+"-"+sessionID), nil
}

// GivenBranch returns the branch the session was last given a summary for,
// empty when there is none.
func (*BranchSummaryGiven) GivenBranch(ctx context.Context, cwd string, provider domain.ProviderKind, sessionID string) (string, error) {
	file, err := branchSummaryGivenFile(ctx, cwd, provider, sessionID)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) || len(raw) > branchSummaryGivenMax {
		return "", nil
	}
	return strings.TrimSpace(string(raw)), err
}

// SetGivenBranch remembers the branch, and forgets the sessions that were
// given nothing for a long time.
func (*BranchSummaryGiven) SetGivenBranch(ctx context.Context, cwd string, provider domain.ProviderKind, sessionID, branch string) error {
	file, err := branchSummaryGivenFile(ctx, cwd, provider, sessionID)
	if err != nil {
		return err
	}
	if branch == "" || len(branch) > branchSummaryGivenMax || strings.ContainsAny(branch, "\n\x00") {
		return domain.ErrInvalidRef
	}
	pending, err := os.CreateTemp(filepath.Dir(file), ".pending-*")
	if err != nil {
		return err
	}
	_, err = pending.WriteString(branch + "\n")
	if closeErr := pending.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(pending.Name(), file)
	}
	if err != nil {
		_ = os.Remove(pending.Name())
		return err
	}
	if entries, listErr := os.ReadDir(filepath.Dir(file)); listErr == nil {
		for _, entry := range entries {
			if info, infoErr := entry.Info(); infoErr == nil && !info.IsDir() && time.Since(info.ModTime()) > branchSummaryGivenAge {
				_ = os.Remove(filepath.Join(filepath.Dir(file), entry.Name()))
			}
		}
	}
	return nil
}
