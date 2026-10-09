package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RunCaptureRecovery has no implicit replay, remote operation or live provider
// capture. Only retry may publish, using this attempt's frozen outcomes.
func RunCaptureRecovery(ctx context.Context, c *Container, cwd, repo string, args []string, w io.Writer) error {
	if c.CaptureRecovery == nil {
		return fmt.Errorf("capture recovery is unavailable")
	}
	pos := positionals(args)
	if len(pos) == 0 {
		return fmt.Errorf("usage: %s", commandArgSpecs["capture"].usage)
	}
	if pos[0] == "list" || pos[0] == "show" {
		states, err := c.CaptureRecovery.Inspect(ctx, repo)
		if err != nil {
			return err
		}
		selected := []domain.CaptureRecoveryStatus{}
		for _, st := range states {
			if pos[0] == "show" {
				if st.ID == pos[1] {
					selected = append(selected, st)
				}
				continue
			}
			if st.State != "completed" || flagPresent(args, "--all") {
				selected = append(selected, st)
			}
		}
		if pos[0] == "show" && len(selected) == 0 {
			return domain.ErrNotFound
		}
		if flagPresent(args, "--json") {
			return json.NewEncoder(w).Encode(selected)
		}
		for _, st := range selected {
			fmt.Fprintf(w, "%s  %s  %s  Git %s\n  expect: %s\n", st.ID, st.State, st.Branch, st.CodeCommit, st.Fingerprint)
			if st.Retry != nil {
				fmt.Fprintf(w, "  Replay attempts: %d; last error: %s\n", st.Retry.Tries, st.Retry.Error)
			}
			if st.Resolution != nil {
				fmt.Fprintf(w, "  Recorded decision: %s\n", st.Resolution.Kind)
			}
			if st.ReplacementID != "" {
				fmt.Fprintf(w, "  Replacement: %s; publication: %s\n", st.ReplacementID, st.PublicationID)
			}
			if pos[0] == "show" {
				for _, o := range st.Outcomes {
					fmt.Fprintf(w, "  %s: %s (%s)\n", o.Provider, o.State, o.Error)
				}
			}
			if st.Resolution == nil {
				switch st.State {
				case "superseded":
					fmt.Fprintf(w, "  Resolve: cxt capture resolve %s --expect %s\n", st.ID, st.Fingerprint)
				case "capture-retry-paused", "ready-to-capture", "ready-to-retry", "ready-to-publish":
					fmt.Fprintf(w, "  Retry: cxt capture retry %s --expect %s\n", st.ID, st.Fingerprint)
				case "needs-review":
					fmt.Fprintf(w, "  Inspect: cxt capture show %s. Missing capture evidence cannot be inferred from the current session.\n", st.ID)
				}
			}
		}
		if len(selected) == 0 {
			fmt.Fprintln(w, "No unresolved capture attempts.")
		}
		return nil
	}
	expect := domain.ContentHash(flagVal(args, "--expect"))
	if pos[0] == "retry" {
		attempt, err := c.CaptureRecovery.RetryAttempt(ctx, repo, pos[1], expect)
		if err != nil {
			return err
		}
		pass := commitCapturePass(attempt)
		if pass.Version == 2 && pass.InputsReady {
			if c.CommitCapture == nil {
				return fmt.Errorf("frozen-input replay unavailable")
			}
			root := cxtRepoRoot(ctx, cwd)
			release, err := claimCaptureWorker(root)
			if err != nil {
				return err
			}
			if release == nil {
				return fmt.Errorf("capture worker is busy; retry after it finishes")
			}
			defer release()
			// Another worker may have completed after the initial inspection.
			attempt, err = c.CaptureRecovery.RetryAttempt(ctx, repo, pos[1], expect)
			if err != nil {
				return err
			}
			pass = commitCapturePass(attempt)
			jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			if err := processFrozenCapture(jobCtx, c, cwd, root, &pass); err != nil {
				return err
			}
			fmt.Fprintln(w, "Capture publication recovered from frozen input. Run cxt push to deliver it to the server.")
			return nil
		}
		accepted, err := publicationHistory(ctx, c, repo)
		if err != nil {
			return err
		}
		if !pass.Complete {
			if err := recoverCommitCapture(ctx, c, cwd, cxtRepoRoot(ctx, cwd), &pass, accepted); err != nil {
				return err
			}
		}
		if err := publishCommitCapture(ctx, c, cwd, &pass, accepted); err != nil {
			return err
		}
		fmt.Fprintln(w, "Capture publication recovered from recorded evidence. Run cxt push to deliver it to the server.")
		return nil
	}
	resolution, err := c.CaptureRecovery.Resolve(ctx, repo, pos[1], expect, flagVal(args, "--reason"), pos[0] == "acknowledge")
	if err != nil {
		return err
	}
	if flagPresent(args, "--json") {
		return json.NewEncoder(w).Encode(resolution)
	}
	fmt.Fprintf(w, "Recorded %s for %s. Original capture outcomes are unchanged.\n", resolution.Kind, resolution.AttemptID)
	return nil
}
