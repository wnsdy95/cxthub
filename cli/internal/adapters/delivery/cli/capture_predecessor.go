package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capturejournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Code may advance while a pending capture still owns the same context cursor.
// Any explicit memory/selection/branch change breaks this continuation claim.
func sameCaptureCursor(a, b *domain.WorkingPosition) bool {
	if a == nil || b == nil {
		return false
	}
	x, y := *a, *b
	x.GitCommit, y.GitCommit = "", ""
	return reflect.DeepEqual(x, y)
}

func selectCapturePredecessor(ctx context.Context, cwd, root string, p *commitCapturePass) error {
	// A Git commit's first parent is immutable evidence, unlike timestamps or
	// whichever capture happened to finish last. Root commits have no parent.
	raw, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-list", "--parents", "-n", "1", p.Proof.GitAfter).Output()
	if err != nil {
		return err
	}
	line := strings.Fields(string(raw))
	if len(line) < 2 {
		return nil
	}
	if line[0] != p.Proof.GitAfter || !validNonZeroGitOID(line[1]) {
		return domain.ErrHashMismatch
	}
	parent := line[1]
	attempts, err := capturejournal.New(root, cwd).ListCaptureAttempts(ctx, p.Proof.RepoID)
	if err != nil {
		return err
	}
	for _, a := range attempts {
		if a.Version != 2 || a.Proof.GitAfter != parent || a.Proof.WorktreeID != p.Proof.WorktreeID || a.Proof.BranchID != p.Proof.BranchID || a.Proof.Branch != p.Proof.Branch || !sameCaptureCursor(a.FrozenPosition, p.FrozenPosition) {
			continue
		}
		// A completed/absent capture with no queued predecessor adds no input.
		hasInput := a.Predecessor != nil
		for _, o := range a.Outcomes {
			hasInput = hasInput || o.State != "absent"
		}
		if !hasInput || a.Complete && a.Proof.Target == "" {
			continue
		}
		if p.Predecessor != nil {
			return fmt.Errorf("multiple captures at the Git parent require explicit selection")
		}
		p.Predecessor = &domain.CapturePredecessor{AttemptID: a.Proof.ID, GitCommit: parent}
	}
	return nil
}

func resolveCapturePredecessor(ctx context.Context, c *Container, cwd, root string, p *commitCapturePass) error {
	dep := p.Predecessor
	if dep == nil {
		return nil
	}
	rel := filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-passes", dep.AttemptID+".json")
	raw, err := providerfs.ReadRepoFile(root, rel)
	if err != nil {
		return err
	}
	var a domain.CaptureAttempt
	if json.Unmarshal(raw, &a) != nil || a.Validate() != nil || a.Proof.ID != dep.AttemptID || a.Proof.RepoID != p.Proof.RepoID || a.Proof.GitAfter != dep.GitCommit || !sameCaptureCursor(a.FrozenPosition, p.FrozenPosition) {
		return domain.ErrHashMismatch
	}
	if !a.Complete || !a.MemoryFinalized || a.Observation == nil {
		return fmt.Errorf("preceding Git capture %s is not finalized", dep.AttemptID)
	}
	journal := capturejournal.New(root, root)
	if resolution, err := journal.ReadCaptureResolution(ctx, a); err != nil {
		return err
	} else if resolution != nil {
		return domain.ErrSyncConflict
	}
	accepted, err := publicationHistory(ctx, c, p.Proof.RepoID)
	if err != nil {
		return err
	}
	want := *a.Observation
	if got, ok := accepted[want.ID]; !ok || !reflect.DeepEqual(got, want) {
		return fmt.Errorf("preceding capture has no immutable memory observation")
	}
	if p.PredecessorObservation == nil {
		next := *p
		next.PredecessorObservation = &want
		if err := p.replace(ctx, root, next); err != nil {
			return err
		}
	} else if !reflect.DeepEqual(*p.PredecessorObservation, want) {
		return domain.ErrSyncConflict
	}
	// The application retains the exact continuation and any required immutable
	// root-selection witness. No attachment is copied from today's snapshot.
	return c.CommitCapture.RecordFrozenContinuation(ctx, cwd, domain.CaptureAttempt(*p))
}
