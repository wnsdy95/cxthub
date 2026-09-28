package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type syncStatus struct {
	ObservedAt       time.Time                 `json:"observed_at"`
	Repository       string                    `json:"repository"`
	ServerChecked    bool                      `json:"server_checked"`
	IntegrityChecked bool                      `json:"integrity_checked"`
	Ready            int                       `json:"ready"`
	RetryWaiting     int                       `json:"retry_waiting"`
	Historical       []domain.SnapshotBackfill `json:"historical_uploads"`
	Branches         []branchOperationStatus   `json:"branch_operations"`
	Issues           []string                  `json:"issues"`
}

// RunSyncStatus reads only durable queue metadata. No document/memory bodies,
// network, replay, provider discovery, mutation locks or repair are involved.
// A zero queue is not a claim of server acknowledgement or replica integrity.
func RunSyncStatus(ctx context.Context, cwd string, asJSON bool, w io.Writer) error {
	report := syncStatus{ObservedAt: time.Now().UTC(), Historical: []domain.SnapshotBackfill{}, Branches: []branchOperationStatus{}, Issues: []string{}}
	state := gitctx.InspectContextRoot(ctx, cwd)
	if !state.Initialized {
		report.Issues = append(report.Issues, "no initialized local replica; use cxt doctor to inspect registration and damage")
	} else {
		repo, err := remotecfg.Wrap(state.Root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
		if err != nil {
			report.Issues = append(report.Issues, "repository identity: "+err.Error())
		} else {
			report.Repository = string(repo.ID)
			jobs, err := storage.NewFileStore(state.Root).ListBackfills(ctx, report.Repository)
			if err != nil {
				report.Issues = append(report.Issues, "historical upload queue: "+err.Error())
			} else {
				report.Historical = append(report.Historical, jobs...)
				for _, job := range jobs {
					if job.NextAttempt.After(report.ObservedAt) {
						report.RetryWaiting++
					} else {
						report.Ready++
					}
				}
			}
		}
	}
	branches, err := inspectBranchOperations(ctx, cwd)
	if err != nil {
		report.Issues = append(report.Issues, "branch journal: "+err.Error())
	} else {
		for _, op := range branches {
			if op.LocalState != "applied" && op.LocalState != "aborted" {
				report.Branches = append(report.Branches, op)
			}
		}
	}
	if asJSON {
		if err := json.NewEncoder(w).Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(w, "Local synchronization queues (server and full object integrity not checked):")
		fmt.Fprintf(w, "Retained history: %d pending; %d eligible now; %d waiting for retry.\n", len(report.Historical), report.Ready, report.RetryWaiting)
		for _, job := range report.Historical {
			reason := job.Reason
			if reason == "" {
				reason = "queued"
			}
			fmt.Fprintf(w, "  %s  %s  attempts=%d  next=%s\n", shortHash(job.Snapshot), reason, job.Attempts, job.NextAttempt.Format(time.RFC3339))
		}
		fmt.Fprintf(w, "Unresolved branch operations: %d (cxt branch operations --json).\n", len(report.Branches))
		for _, issue := range report.Issues {
			fmt.Fprintf(w, "- %s\n", issue)
		}
		fmt.Fprintln(w, "Retry uploads: cxt push. Wait for retained history: cxt push --wait-history. Full local audit: cxt doctor. Server audit: cxt fsck.")
	}
	if len(report.Issues) > 0 {
		return fmt.Errorf("sync status could not read %d metadata source(s); no files changed", len(report.Issues))
	}
	return nil
}
