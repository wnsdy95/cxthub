package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type branchOperationStatus struct {
	ID          string `json:"id"`
	Branch      string `json:"branch"`
	Kind        string `json:"kind"`
	LocalState  string `json:"local_state"`
	ServerState string `json:"server_state"`
	Error       string `json:"error,omitempty"`
}

func inspectBranchOperations(ctx context.Context, cwd string) ([]branchOperationStatus, error) {
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return nil, err
	}
	ops, err := j.List()
	if err != nil {
		return nil, err
	}
	out := make([]branchOperationStatus, 0, len(ops))
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state := op.Phase
		if state == "prepared" {
			witness, err := hasBranchCommitWitness(op)
			if err != nil {
				return nil, err
			}
			if witness {
				state = "ready-to-replay"
			} else {
				state = "needs-git-evidence"
			}
		}
		out = append(out, branchOperationStatus{ID: op.Event.ID, Branch: strings.TrimPrefix(op.GitRef, "refs/heads/"), Kind: op.Event.Kind, LocalState: state, ServerState: "not-checked", Error: op.LastError})
	}
	return out, nil
}

type diagnosticsReport struct {
	Completed                  bool                           `json:"completed"`
	ReplicaInspectionCompleted bool                           `json:"replica_inspection_completed"`
	DocumentsChecked           int                            `json:"documents_checked"`
	HistoricalUploadsObserved  bool                           `json:"historical_uploads_observed"`
	Unchecked                  []string                       `json:"unchecked"`
	Backfills                  []domain.SnapshotBackfill      `json:"historical_uploads"`
	Captures                   []domain.CaptureRecoveryStatus `json:"captures"`
	GitRepository              bool                           `json:"git_repository"`
	Registered                 bool                           `json:"registered"`
	ReplicaPresent             bool                           `json:"replica_present"`
	Initialized                bool                           `json:"initialized"`
	Snapshots                  int                            `json:"snapshots"`
	HistoryEvents              int                            `json:"history_events"`
	Operations                 []branchOperationStatus        `json:"operations"`
	Issues                     []string                       `json:"issues"`
}

// RunDiagnostics is dispatched before the ordinary composition root and its
// automatic replay. It remains available with a missing or corrupt .cxt and
// never authenticates, writes, contacts the server, or controls a provider.
func RunDiagnostics(ctx context.Context, cwd string, args []string, w io.Writer, captureChecks ...func(context.Context, string) ([]domain.CaptureRecoveryStatus, error)) error {
	if args[0] == "branch" {
		if err := ctx.Err(); err != nil {
			return err
		}
		operations, err := inspectBranchOperations(ctx, cwd)
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		if err != nil {
			return err
		}
		if flagPresent(args, "--json") {
			return json.NewEncoder(w).Encode(operations)
		}
		fmt.Fprintln(w, "Local branch operations (server acknowledgement not checked):")
		for _, op := range operations {
			fmt.Fprintf(w, "%s  %-22s %s (%s)\n", op.ID, op.LocalState, op.Branch, op.Kind)
			if op.Error != "" {
				fmt.Fprintf(w, "  Cause: %s\n", op.Error)
			}
		}
		if len(operations) == 0 {
			fmt.Fprintln(w, "No recorded operations.")
		}
		return nil
	}
	report := collectDiagnostics(ctx, cwd, captureChecks)
	if flagPresent(args, "--json") {
		if err := json.NewEncoder(w).Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "Local replica: present=%t initialized=%t · %d snapshots · %d documents checked · %d history events\n", report.ReplicaPresent, report.Initialized, report.Snapshots, report.DocumentsChecked, report.HistoryEvents)
		if !report.Completed {
			fmt.Fprintf(w, "Inspection incomplete; unchecked phases: %s. No files changed.\n", strings.Join(report.Unchecked, ", "))
		}
		if slices.Contains(report.Unchecked, "historical uploads") || !report.HistoricalUploadsObserved {
			fmt.Fprintln(w, "Retained historical uploads: not checked.")
		} else {
			fmt.Fprintf(w, "Retained historical uploads: %d queued. Retry with cxt push; wait for all records with cxt push --wait-history.\n", len(report.Backfills))
		}
		for _, job := range report.Backfills {
			fmt.Fprintf(w, "  %s  attempts=%d reason=%s next=%s\n", job.Snapshot, job.Attempts, job.Reason, job.NextAttempt.Format(time.RFC3339))
		}
		for _, issue := range report.Issues {
			fmt.Fprintf(w, "- %s\n", issue)
		}
		if report.Completed && len(report.Issues) == 0 {
			fmt.Fprintln(w, "No failures found in local snapshot references, documents, memory attachments, or branch journal.")
		}
		fmt.Fprintln(w, "Capture history (including acknowledged gaps): cxt capture list. Server integrity: cxt fsck. Queued operations: cxt branch operations. Verified replay: cxt branch replay.")
	}
	// Cancellation is not corruption. Preserve the typed cause and emit the
	// partial report first; the command boundary maps SIGINT to exit 130.
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(report.Issues) > 0 {
		return fmt.Errorf("local inspection found %d issue(s); no files changed", len(report.Issues))
	}
	return nil
}

func collectDiagnostics(ctx context.Context, cwd string, captureChecks []func(context.Context, string) ([]domain.CaptureRecoveryStatus, error)) diagnosticsReport {
	r := diagnosticsReport{
		Unchecked: []string{"Git journal", "Git registration", "replica inspection", "historical uploads", "capture recovery"},
		Backfills: []domain.SnapshotBackfill{}, Captures: []domain.CaptureRecoveryStatus{},
		Operations: []branchOperationStatus{}, Issues: []string{},
	}
	stop := func() bool {
		if err := ctx.Err(); err != nil {
			r.Issues = append(r.Issues, "local inspection incomplete: "+err.Error())
			return true
		}
		return false
	}
	phaseDone := func() { r.Unchecked = r.Unchecked[1:] }
	if stop() {
		return r
	}
	operations, opErr := inspectBranchOperations(ctx, cwd)
	if stop() {
		return r
	}
	r.Operations = operations
	if opErr != nil {
		r.Issues = append(r.Issues, "Git journal: "+opErr.Error())
	}
	for _, op := range operations {
		if stop() {
			return r
		}
		if op.LocalState != "applied" && op.LocalState != "aborted" {
			r.Issues = append(r.Issues, fmt.Sprintf("branch operation %s: %s", op.ID, op.LocalState))
		}
	}
	phaseDone()
	state := gitctx.InspectContextRoot(ctx, cwd)
	if stop() {
		return r
	}
	r.GitRepository, r.ReplicaPresent, r.Initialized = state.GitRepository, state.Exists, state.Initialized
	if j, err := branchjournal.Open(ctx, cwd); err == nil {
		r.Registered, err = j.Status()
		if stop() {
			return r
		}
		if err != nil {
			r.Issues = append(r.Issues, "Git registration: "+err.Error())
		}
	}
	if stop() {
		return r
	}
	if r.Registered && !state.Initialized {
		r.Issues = append(r.Issues, "registered Git repository has no initialized .cxt replica; preserve damaged files and recover a verified copy before replay")
	}
	phaseDone()
	if state.Exists {
		store := storage.NewFileStore(state.Root)
		inspection := store.InspectReplica(ctx)
		r.Snapshots, r.HistoryEvents, r.DocumentsChecked = inspection.Snapshots, inspection.HistoryEvents, inspection.DocumentsChecked
		r.ReplicaInspectionCompleted = inspection.Completed
		r.Issues = append(r.Issues, inspection.Issues...)
		if stop() {
			return r
		}
	}
	phaseDone()
	if state.Exists {
		store := storage.NewFileStore(state.Root)
		repo, err := remotecfg.Wrap(state.Root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
		if stop() {
			return r
		}
		if err != nil {
			r.Issues = append(r.Issues, "Historical uploads: cannot establish repository identity: "+err.Error())
		} else {
			jobs, err := store.ListBackfills(ctx, string(repo.ID))
			if stop() {
				return r
			}
			if err != nil {
				r.Issues = append(r.Issues, "Historical uploads: "+err.Error())
			} else {
				r.HistoricalUploadsObserved = true
				r.Backfills = jobs
				for _, job := range jobs {
					if stop() {
						return r
					}
					if job.Reason != "" {
						r.Issues = append(r.Issues, fmt.Sprintf("historical upload %s: %s; retained for retry", job.Snapshot, job.Reason))
					}
				}
			}
		}
	}
	phaseDone()
	for _, check := range captureChecks {
		if stop() {
			return r
		}
		states, err := check(ctx, cwd)
		if stop() {
			return r
		}
		if err != nil {
			r.Issues = append(r.Issues, "Capture recovery: "+err.Error())
			continue
		}
		for _, st := range states {
			if stop() {
				return r
			}
			if st.State == "completed" {
				continue
			}
			r.Captures = append(r.Captures, st)
			if st.Resolution == nil {
				r.Issues = append(r.Issues, fmt.Sprintf("capture %s: %s; inspect with cxt capture show %s", st.ID, st.State, st.ID))
			}
		}
	}
	if stop() {
		return r
	}
	phaseDone()
	r.Completed = true
	return r
}

func replayBranchCommand(ctx context.Context, c *Container, cwd string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := replayBranchOperations(ctx, c, cwd); err != nil {
		return err
	}
	operations, err := inspectBranchOperations(ctx, cwd)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.LocalState != "applied" && op.LocalState != "aborted" {
			return fmt.Errorf("operation %s remains %s; inspect with cxt branch operations (no Git state was guessed)", op.ID, op.LocalState)
		}
	}
	if c.Sync != nil {
		if _, err := os.Stat(filepath.Join(gitOut(cwd, "rev-parse", "--show-toplevel"), ".cxt")); err == nil {
			spawnBranchStateSync(cwd)
		}
	}
	fmt.Println("Verified local operations replayed. Server synchronization is queued; use cxt push to wait for acknowledgement.")
	return nil
}
