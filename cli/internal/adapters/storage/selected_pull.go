package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *FileStore) appliedPullPath(repo, remote string) (string, error) {
	if s.worktreeID == "" || remote == "" || domain.ValidateContentHash(domain.ContentHash(repo)) != nil {
		return "", domain.ErrInvalidRef
	}
	key := domain.HashContent([]byte(repo + "\x00" + remote + "\x00" + s.worktreeID))
	return filepath.Join(s.storeDir(), "applied-pulls", hexOf(key)+".json"), nil
}

func (s *FileStore) ReadAppliedPull(ctx context.Context, repo, remote string) (outbound.SelectedPullReceipt, error) {
	var receipt outbound.SelectedPullReceipt
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	path, err := s.appliedPullPath(repo, remote)
	if err != nil {
		return receipt, err
	}
	raw, err := readCxtFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return receipt, domain.ErrNotFound
	}
	if err != nil {
		return receipt, err
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Plan.Version != 1 || receipt.Plan.RepoID != repo || receipt.Plan.Remote != remote || receipt.Plan.Expected.Position == nil || receipt.Plan.Expected.Position.WorktreeID != s.worktreeID || receipt.Plan.ID != outbound.SelectedPullPlanID(receipt.Plan) || receipt.AppliedAt.IsZero() {
		return receipt, domain.ErrHashMismatch
	}
	return receipt, nil
}

func (s *FileStore) ApplySelectedPull(ctx context.Context, plan outbound.SelectedPullPlan) (outbound.SelectedPullReceipt, error) {
	var result outbound.SelectedPullReceipt
	path, err := s.appliedPullPath(plan.RepoID, plan.Remote)
	if err != nil {
		return result, err
	}
	if plan.Version != 1 || plan.Expected.Position == nil || plan.Expected.Position.WorktreeID != s.worktreeID || plan.Expected.Position.RepoID != plan.RepoID || plan.Selection.CodeCommit != plan.Expected.Position.GitCommit || plan.Selection.Position != string(plan.Expected.Position.Snapshot) || plan.Selection.Branch != plan.Expected.Position.Branch || plan.ID != outbound.SelectedPullPlanID(plan) {
		return result, domain.ErrHashMismatch
	}
	err = s.withRefMutationLock(ctx, func() error {
		state, err := s.ReadCheckoutState(ctx, plan.RepoID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state, plan.Expected) {
			return domain.ErrSelectionChanged
		}
		previous, err := s.ReadAppliedPull(ctx, plan.RepoID, plan.Remote)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if previous.Plan.ID == plan.ID {
			result = previous
			return nil
		}
		if previous.Plan.ID != plan.Previous {
			return domain.ErrSelectionChanged
		}
		result = outbound.SelectedPullReceipt{Plan: plan, AppliedAt: time.Now().UTC()}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		// Keep the immutable receipt before atomically publishing the active one.
		// A crash can leave an unused history entry, never a partial application.
		if err := writeAtomic(filepath.Join(s.storeDir(), "pull-receipts", hexOf(plan.ID)+".json"), raw); err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
	return result, err
}

var _ outbound.SelectedPullStore = (*FileStore)(nil)
