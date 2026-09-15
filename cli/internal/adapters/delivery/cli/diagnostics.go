package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
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

// RunDiagnostics is dispatched before the ordinary composition root and its
// automatic replay. It remains available with a missing or corrupt .cxt and
// never authenticates, writes, contacts the server, or controls a provider.
func RunDiagnostics(ctx context.Context, cwd string, args []string, w io.Writer) error {
	operations, opErr := inspectBranchOperations(ctx, cwd)
	if args[0] == "branch" {
		if opErr != nil {
			return opErr
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
	state := gitctx.InspectContextRoot(ctx, cwd)
	issues := []string{}
	if opErr != nil {
		issues = append(issues, "Git journal: "+opErr.Error())
	}
	registered := false
	if j, err := branchjournal.Open(ctx, cwd); err == nil {
		registered, err = j.Status()
		if err != nil {
			issues = append(issues, "Git registration: "+err.Error())
		}
	}
	if registered && !state.Initialized {
		issues = append(issues, "registered Git repository has no initialized .cxt replica; preserve damaged files and recover a verified copy before replay")
	}
	inspection := storage.ReplicaInspection{Issues: []string{}}
	if state.Exists {
		inspection = storage.NewFileStore(state.Root).InspectReplica(ctx)
		issues = append(issues, inspection.Issues...)
	}
	for _, op := range operations {
		if op.LocalState != "applied" && op.LocalState != "aborted" {
			issues = append(issues, fmt.Sprintf("branch operation %s: %s", op.ID, op.LocalState))
		}
	}
	report := struct {
		GitRepository  bool                    `json:"git_repository"`
		Registered     bool                    `json:"registered"`
		ReplicaPresent bool                    `json:"replica_present"`
		Initialized    bool                    `json:"initialized"`
		Snapshots      int                     `json:"snapshots"`
		HistoryEvents  int                     `json:"history_events"`
		Operations     []branchOperationStatus `json:"operations"`
		Issues         []string                `json:"issues"`
	}{state.GitRepository, registered, state.Exists, state.Initialized, inspection.Snapshots, inspection.HistoryEvents, operations, issues}
	if flagPresent(args, "--json") {
		if err := json.NewEncoder(w).Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "Local replica: present=%t initialized=%t · %d snapshots · %d history events\n", state.Exists, state.Initialized, inspection.Snapshots, inspection.HistoryEvents)
		for _, issue := range issues {
			fmt.Fprintf(w, "- %s\n", issue)
		}
		if len(issues) == 0 {
			fmt.Fprintln(w, "No failures found in local snapshot references, documents, memory attachments, or branch journal.")
		}
		fmt.Fprintln(w, "Server integrity: cxt fsck. Queued operations: cxt branch operations. Verified replay: cxt branch replay.")
	}
	if len(issues) > 0 {
		return fmt.Errorf("local inspection found %d issue(s); no files changed", len(issues))
	}
	return nil
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
