package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capturejournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Mutations CAS the receipt under the same journal lock as capture resolutions.
// Expensive input/projection work never holds this global lock.
func (p *commitCapturePass) replace(ctx context.Context, root string, next commitCapturePass) error {
	if err := next.validate(); err != nil {
		return err
	}
	lock, err := branchjournal.Open(ctx, root)
	if err != nil {
		return err
	}
	return lock.Transaction(ctx, func() error {
		if err := ensureCaptureUnresolved(root, p); err != nil {
			return err
		}
		raw, err := providerfs.ReadRepoFile(root, p.relativePath())
		if err != nil {
			return err
		}
		var current domain.CaptureAttempt
		if json.Unmarshal(raw, &current) != nil || current.Validate() != nil {
			return domain.ErrHashMismatch
		}
		if current.Fingerprint() != domain.CaptureAttempt(*p).Fingerprint() {
			return domain.ErrSyncConflict
		}
		if err := next.write(root); err != nil {
			return err
		}
		*p = next
		return nil
	})
}

func freezeCommitInputs(ctx context.Context, c *Container, cwd, root, message string, p *commitCapturePass) error {
	next := *p
	next.Message, next.Author = message, &c.Identity
	var err error
	next.Settings, err = c.CommitCapture.FreezeSettings(ctx, cwd)
	if err != nil {
		return p.pending(err)
	}
	if err = p.replace(ctx, root, next); err != nil {
		return err
	}
	// Retain the exact selection before projection or subsequent worktree moves.
	baseline := p.Proof
	baseline.ID = domain.CaptureBaselineObservationID(p.Proof.ID)
	baseline.Source, baseline.Target = p.Initial, p.Initial
	if err := c.History.RecordHistory(ctx, baseline); err != nil {
		return err
	}
	var failures []error
	for i, o := range p.Outcomes {
		claimed := o.Provider == domain.ProviderCodex && (providerfs.ValidSessionID(os.Getenv("CODEX_THREAD_ID")) || providerfs.ValidSessionID(os.Getenv("CODEX_SESSION_ID")))
		if owner, managed := supervisedProvider(ctx, cwd); managed && owner == o.Provider {
			claimed = true
		}
		selected, err := commandCapture(ctx, cwd, o.Provider)
		if err != nil {
			state := "failed"
			if errors.Is(err, domain.ErrNoActiveSession) && !claimed {
				state = "absent"
			} else {
				failures = append(failures, err)
			}
			if err := p.recordOutcome(ctx, root, i, "", state, inbound.SaveOutput{}, err); err != nil {
				return err
			}
			continue
		}
		if err := p.checkGit(cwd); err != nil {
			return p.pending(err)
		}
		input, err := c.CommitCapture.FreezeInput(ctx, cwd, o.Provider, selected.SessionPath)
		if err != nil {
			if selected.SessionPath == "" && !claimed && errors.Is(err, domain.ErrNoActiveSession) {
				if err := p.recordOutcome(ctx, root, i, "", "absent", inbound.SaveOutput{}, err); err != nil {
					return err
				}
				continue
			}
			// An owned source disappearing is a failure, never an absent provider.
			if werr := p.recordOutcome(ctx, root, i, selected.SessionPath, "failed", inbound.SaveOutput{}, err); werr != nil {
				return errors.Join(err, werr)
			}
			failures = append(failures, err)
			continue
		}
		next = *p
		next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
		next.Outcomes[i].Input = &input
		next.Outcomes[i].SessionPath = input.SourcePath
		next.Outcomes[i].SessionID = selected.SessionID
		if next.Outcomes[i].SessionID == "" {
			next.Outcomes[i].SessionID = input.SessionID
		}
		if err := p.replace(ctx, root, next); err != nil {
			return err
		}
	}
	if err := errors.Join(failures...); err != nil {
		return p.pending(err)
	}
	if err := p.checkGit(cwd); err != nil {
		return p.pending(err)
	}
	// Sealing is the only admission to automatic replay. An interrupted freeze
	// cannot rediscover missing providers from a later conversation.
	next = *p
	next.InputsReady = true
	return p.replace(ctx, root, next)
}

type captureRetry = domain.CaptureRetryState

func SpawnCommitCapture(cwd string) {
	state := gitctx.InspectContextRoot(context.Background(), cwd)
	if !state.Initialized {
		return
	}
	root := state.Root
	id, err := branchjournal.NewID()
	if err != nil {
		return
	}
	if err := providerfs.WriteRepoFileDurable(root, filepath.Join(".cxt", "capture", "replay-wake"), []byte(id), 0600); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "git-hook", "capture-replay")
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// One repository-local worker serializes input jobs. flock ownership dies with
// the process; receipts, output observations and retry schedules survive it.
func runCommitCaptureWorker(ctx context.Context, c *Container, cwd string) error {
	if c.CommitCapture == nil || c.History == nil {
		return nil
	}
	state := gitctx.InspectContextRoot(ctx, cwd)
	if !state.Initialized {
		return nil
	}
	root := state.Root
	release, err := claimCaptureWorker(root)
	if err != nil {
		return err
	}
	if release == nil {
		return nil
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	repo, err := resolvePublicationRepo(ctx, c, cwd)
	if err != nil {
		return err
	}
	journal := capturejournal.New(root, cwd)
	for {
		wake, err := readCaptureWake(root)
		if err != nil {
			return err
		}
		attempts, err := journal.ListCaptureAttempts(ctx, repo.ID)
		if err != nil {
			return err
		}
		sort.Slice(attempts, func(i, j int) bool {
			if attempts[i].Proof.CreatedAt.Equal(attempts[j].Proof.CreatedAt) {
				return attempts[i].Proof.ID < attempts[j].Proof.ID
			}
			return attempts[i].Proof.CreatedAt.Before(attempts[j].Proof.CreatedAt)
		})
		var nextWake time.Time
		// History is shared by this scan. Reloading the complete ledger for every
		// already-published attempt makes idle replay quadratic in capture count.
		accepted, err := publicationHistory(ctx, c, repo.ID)
		if err != nil {
			return err
		}
		for _, attempt := range attempts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if attempt.Version != 2 || !attempt.InputsReady {
				continue
			}
			resolution, err := journal.ReadCaptureResolution(ctx, attempt)
			if err != nil {
				return err
			}
			if resolution != nil {
				continue
			}
			// Published attempts are complete even if the worktree moved meanwhile.
			if _, ok := accepted[attempt.Publication().ID]; attempt.Complete && (ok || attempt.Proof.Target == "") {
				continue
			}
			retryRel := filepath.Join(".cxt", "worktrees", attempt.Proof.WorktreeID, "capture-retries", attempt.Proof.ID+".json")
			retry := captureRetry{Version: 1, Attempt: attempt.Proof.ID}
			if raw, err := providerfs.ReadRepoFile(root, retryRel); err == nil {
				if json.Unmarshal(raw, &retry) != nil || retry.Version != 1 || retry.Attempt != attempt.Proof.ID || retry.Tries < 0 {
					return domain.ErrHashMismatch
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			if retry.Tries >= 8 {
				continue
			}
			if time.Now().Before(retry.Next) {
				if nextWake.IsZero() || retry.Next.Before(nextWake) {
					nextWake = retry.Next
				}
				continue
			}
			// Persist the claim/backoff before expensive work. A killed worker consumes
			// an attempt, retains input, and cannot turn a crash into a tight retry loop.
			retry.Tries++
			retry.Next = time.Now().Add(time.Duration(1<<min(retry.Tries, 8)) * time.Second)
			raw, _ := json.Marshal(retry)
			if err := providerfs.WriteRepoFileDurable(root, retryRel, raw, 0600); err != nil {
				return err
			}
			jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			p := commitCapturePass(attempt)
			err = processFrozenCapture(jobCtx, c, cwd, root, &p, func(where string) {
				spawnBranchStateSync(where)
				wakeHistoricalSync(c, where)
			})
			cancel()
			retry.Error = ""
			if err != nil {
				retry.Error = err.Error()
			}
			raw, _ = json.Marshal(retry)
			if werr := providerfs.WriteRepoFileDurable(root, retryRel, raw, 0600); werr != nil {
				return errors.Join(err, werr)
			}
			if err != nil && retry.Tries < 8 {
				if nextWake.IsZero() || retry.Next.Before(nextWake) {
					nextWake = retry.Next
				}
			}
		}
		if nextWake.IsZero() {
			// Release before checking generation: a later notifier can now acquire the
			// lock; an earlier notifier that lost the lock changed this durable marker.
			release()
			release = nil
			after, err := readCaptureWake(root)
			if err != nil {
				return err
			}
			if bytes.Equal(wake, after) {
				return nil
			}
			release, err = claimCaptureWorker(root)
			if err != nil {
				return err
			}
			if release == nil {
				return nil
			}
			continue
		}
		timer := time.NewTimer(min(max(time.Until(nextWake), time.Second), 5*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func processFrozenCapture(ctx context.Context, c *Container, cwd, root string, p *commitCapturePass, wake ...func(string)) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Version != 2 || !p.InputsReady {
		return fmt.Errorf("capture input was not durably sealed")
	}
	if !p.Complete {
		for i, o := range p.Outcomes {
			if o.State == "absent" || o.State == "saved" {
				continue
			}
			if o.MemoryPlan == nil {
				plan, err := c.CommitCapture.PrepareFrozenMemory(ctx, cwd, domain.CaptureAttempt(*p), i)
				if err != nil {
					return err
				}
				if plan != nil {
					next := *p
					next.Outcomes = append([]domain.CaptureOutcome(nil), p.Outcomes...)
					next.Outcomes[i].MemoryPlan = plan
					if err := p.replace(ctx, root, next); err != nil {
						return err
					}
				}
			}
			out, err := c.CommitCapture.SaveFrozen(ctx, cwd, domain.CaptureAttempt(*p), i)
			if err != nil {
				return err
			}
			if out.Branch != p.Proof.Branch || domain.ValidateContentHash(out.SnapshotID) != nil {
				return domain.ErrHashMismatch
			}
			if err := p.recordOutcome(ctx, root, i, o.SessionPath, "saved", out, nil); err != nil {
				return err
			}
		}
		if p.FinalMemory == nil {
			selected := *p
			if err := prepareCommitProof(ctx, c, &selected); err != nil {
				return err
			}
			plan, err := c.CommitCapture.PrepareFrozenMemory(ctx, cwd, domain.CaptureAttempt(selected), -1)
			if err != nil {
				return err
			}
			if plan != nil {
				next := *p
				next.FinalMemory = plan
				if err := p.replace(ctx, root, next); err != nil {
					return err
				}
			}
		}
		if p.FinalMemory != nil {
			if _, err := c.CommitCapture.FinishFrozenMemory(ctx, cwd, domain.CaptureAttempt(*p)); err != nil {
				return err
			}
		}
		if !p.MemoryFinalized {
			next := *p
			next.MemoryFinalized = true
			if err := p.replace(ctx, root, next); err != nil {
				return err
			}
		}
		accepted, err := publicationHistory(ctx, c, p.Proof.RepoID)
		if err != nil {
			return err
		}
		if err := recoverCommitCapture(ctx, c, cwd, root, p, accepted); err != nil {
			return err
		}
	}
	// Durable completion is enough to schedule delivery even if applying the
	// optional cursor or writing the publication subsequently fails.
	for _, notify := range wake {
		if notify != nil {
			notify(cwd)
		}
	}
	if err := c.CommitCapture.ApplyFrozen(ctx, cwd, domain.CaptureAttempt(*p)); err != nil {
		return err
	}
	return publishCommitCapture(ctx, c, cwd, p, nil)
}

// A user resolution and a worker outcome are mutually exclusive journal writes.
func ensureCaptureUnresolved(root string, p *commitCapturePass) error {
	rel := filepath.Join(".cxt", "worktrees", p.Proof.WorktreeID, "capture-resolutions", p.Proof.ID+".json")
	_, err := providerfs.ReadRepoFile(root, rel)
	if err == nil {
		return domain.ErrSyncConflict
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func claimCaptureWorker(root string) (func(), error) {
	path, err := providerfs.PrepareRepoFile(root, filepath.Join(".cxt", "capture", "replay.lock"), 0700)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }, nil
}

// Keep the normal small-capture path immediate, but leave time for the hook to
// return and wake replay. The same process lock excludes background projection.
func finishFrozenCaptureInline(ctx context.Context, c *Container, cwd, root string, p *commitCapturePass, wake ...func(string)) (int, error) {
	var size int64
	for _, o := range p.Outcomes {
		if o.Input != nil {
			size += o.Input.Size
		}
	}
	if size > domain.FrozenCaptureChunkBytes {
		return 0, nil // Heavy normalization belongs to replay, not Git's deadline.
	}
	release, err := claimCaptureWorker(root)
	if err != nil {
		return 0, err
	}
	if release == nil {
		return 0, nil
	}
	defer release()
	deadline := time.Now().Add(55 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Add(-5*time.Second).Before(deadline) {
		deadline = end.Add(-5 * time.Second)
	}
	work, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := processFrozenCapture(work, c, cwd, root, p, wake...); err != nil {
		return 0, p.pending(err)
	}
	saved := 0
	for _, out := range p.Outcomes {
		if out.State == "saved" {
			saved++
		}
	}
	return saved, nil
}

func readCaptureWake(root string) ([]byte, error) {
	raw, err := providerfs.ReadRepoFile(root, filepath.Join(".cxt", "capture", "replay-wake"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return raw, err
}
