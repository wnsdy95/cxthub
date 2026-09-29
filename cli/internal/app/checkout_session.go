package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// CheckoutSessionService implements the CheckoutSession inbound port use-case service (compatibility rules).
//
// Dependencies: ForkSession (branching), LoadSession (restoration), SessionStore (ref interpretation). checkout is the integration of fork (+load).
//
// Checkout sequence:
//  1. resolveRef(From) → snapshot ID resolution.
//  2. Freeze the expected worktree selection and validate the source.
//  3. Prepare provider input without publishing an active ref.
//  4. Journal and CAS the branch/HEAD transition, then return the selected ID.
type CheckoutSessionService struct {
	fork  inbound.ForkSession
	load  inbound.LoadSession
	store outbound.SessionStore
	code  outbound.CodePosition
}

// NewCheckoutSessionService creates and injects dependencies for CheckoutSessionService.
func NewCheckoutSessionService(fork inbound.ForkSession, load inbound.LoadSession, store outbound.SessionStore) *CheckoutSessionService {
	return &CheckoutSessionService{fork: fork, load: load, store: store}
}

// WithCodePosition fences Git movement during provider preparation.
func (s *CheckoutSessionService) WithCodePosition(code outbound.CodePosition) *CheckoutSessionService {
	s.code = code
	return s
}

// Checkout restores to the target provider session (branching if necessary).
func (s *CheckoutSessionService) Checkout(ctx context.Context, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
	return withRetainedObjects(ctx, s.store, func() (inbound.CheckoutOutput, error) { return s.checkout(ctx, in, "", "") })
}

func (s *CheckoutSessionService) checkout(ctx context.Context, in inbound.CheckoutInput, expectedTarget domain.ContentHash, expectedCode string) (inbound.CheckoutOutput, error) {
	if s.code != nil {
		actual, err := s.code.CurrentCommit(ctx, in.Cwd)
		if err != nil {
			return inbound.CheckoutOutput{}, err
		}
		if expectedCode != "" && actual != expectedCode {
			return inbound.CheckoutOutput{}, domain.ErrSelectionChanged
		}
		expectedCode = actual
	}
	snapID, err := resolveRef(ctx, s.store, in.RepoID, in.From)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	if expectedTarget != "" && snapID != expectedTarget {
		return inbound.CheckoutOutput{}, domain.ErrSyncConflict
	}

	// Output branch label determination: -b for new branch, otherwise only an
	// actual branch ref from From. Tags and direct hashes are detached restores
	// and must not become symbolic HEAD values.
	branch := in.NewBranch
	var missingBranchTarget domain.ContentHash
	if branch == "" && in.From != "" && in.From != "HEAD" && !strings.HasPrefix(in.From, "sha256:") {
		canonical := in.From
		if bindings, ok := s.store.(outbound.LocalBranchStore); ok {
			binding, err := bindings.ResolveLocalBranch(ctx, in.RepoID, in.From)
			if err != nil {
				return inbound.CheckoutOutput{}, err
			}
			canonical = binding.Branch
		}
		if _, branchErr := s.store.GetRef(ctx, in.RepoID, domain.RefBranch, canonical); branchErr == nil {
			branch = in.From
		} else if event, ok, lifecycleErr := branchLifecycleByName(ctx, s.store, in.RepoID, in.From); lifecycleErr != nil {
			return inbound.CheckoutOutput{}, lifecycleErr
		} else if ok {
			branch = in.From
			missingBranchTarget = event.Target
		}
	}

	// Freeze the worktree selection before preparing provider files. A concurrent
	// writer must not be overwritten when preparation eventually finishes.
	var state outbound.CheckoutState
	tx, transactional := s.store.(outbound.CheckoutTransactionStore)
	if transactional {
		state, err = tx.ReadCheckoutState(ctx, in.RepoID)
		if err != nil {
			return inbound.CheckoutOutput{}, err
		}
	}
	if in.NewBranch != "" {
		if err := domain.ValidateBranchName(in.NewBranch); err != nil {
			return inbound.CheckoutOutput{}, err
		}
		if _, err := s.store.GetRef(ctx, in.RepoID, domain.RefBranch, in.NewBranch); err == nil {
			return inbound.CheckoutOutput{}, domain.ErrBranchExists
		} else if !errors.Is(err, domain.ErrNotFound) {
			return inbound.CheckoutOutput{}, err
		}
	}
	snapshot, err := s.store.GetSnapshot(ctx, snapID)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	if in.RepoID != "" && snapshot.RepoID != in.RepoID {
		return inbound.CheckoutOutput{}, domain.ErrHashMismatch
	}
	sourceBranch, err := loadAgentSourceBranch(ctx, s.store, in.RepoID, in.From, "", snapID)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	var memoryPin *domain.AgentMemoryPin
	if p := state.Position; p != nil && p.Snapshot == snapID && p.Rewound {
		memoryPin = &domain.AgentMemoryPin{}
		if p.MemoryHash != "" {
			memoryPin.MemoryHash, memoryPin.SnapshotID = p.MemoryHash, p.MemorySource
			if memoryPin.SnapshotID == "" {
				memoryPin.SnapshotID = snapID
			}
		}
	}

	lo := inbound.LoadOutput{}
	if !in.SkipMaterialize {
		lo, err = s.load.Load(ctx, inbound.LoadInput{
			RepoID:         in.RepoID,
			Ref:            string(snapID),
			Branch:         sourceBranch,
			MemoryPin:      memoryPin,
			TargetProvider: in.TargetProvider,
			Mode:           in.Mode,
			Cwd:            in.Cwd,
		})
		if err != nil {
			return inbound.CheckoutOutput{}, err
		}
	}
	if s.code != nil {
		actual, err := s.code.CurrentCommit(ctx, in.Cwd)
		if err != nil {
			return inbound.CheckoutOutput{}, err
		}
		if actual != expectedCode {
			return inbound.CheckoutOutput{}, domain.ErrSelectionChanged
		}
	}
	// Loading is normally the preflight for an archived (or crash-interrupted)
	// branch projection. Desktop-app mode deliberately preserves a live session
	// and separately preflights its bounded hook handoff, so resolving the target
	// snapshot is sufficient and no provider session file is created.
	if missingBranchTarget != "" && !transactional {
		if err := recordRestoredBranch(ctx, s.store, in.RepoID, branch, missingBranchTarget); err != nil {
			return inbound.CheckoutOutput{}, err
		}
		_, createErr := s.store.CreateBranchRef(ctx, domain.Ref{
			Kind: domain.RefBranch, Name: branch, RepoID: in.RepoID, Target: missingBranchTarget,
		})
		if errors.Is(createErr, domain.ErrBranchExists) {
			current, currentErr := s.store.GetRef(ctx, in.RepoID, domain.RefBranch, branch)
			if currentErr == nil && current.Target == missingBranchTarget {
				createErr = nil // concurrent idempotent restore won
			}
		}
		if createErr != nil {
			return inbound.CheckoutOutput{}, createErr
		}
	}
	head := domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: in.RepoID, Target: snapID}
	if branch != "" {
		head.Symbolic, head.Target = branch, ""
	}
	if transactional {
		transition := outbound.CheckoutTransition{RepoID: in.RepoID, Expected: state, ExpectedGitCommit: expectedCode, ExpectedMemoryHash: snapshot.MemoryHash, MemoryPin: memoryPin, Head: head, CreateBranch: in.NewBranch != "" || missingBranchTarget != ""}
		if branch != "" {
			ref := domain.Ref{Kind: domain.RefBranch, Name: branch, RepoID: in.RepoID, Target: snapID}
			if !transition.CreateBranch {
				canonical := branch
				if bindings, ok := s.store.(outbound.LocalBranchStore); ok {
					binding, err := bindings.ResolveLocalBranch(ctx, in.RepoID, branch)
					if err != nil {
						return inbound.CheckoutOutput{}, err
					}
					canonical = binding.Branch
				}
				ref, err = s.store.GetRef(ctx, in.RepoID, domain.RefBranch, canonical)
				if err != nil {
					return inbound.CheckoutOutput{}, err
				}
				if ref.Target != snapID {
					return inbound.CheckoutOutput{}, domain.ErrSyncConflict
				}
				transition.Head.Symbolic = canonical
			}
			if missingBranchTarget != "" {
				event, err := prepareCheckoutRestore(ctx, s.store, in.RepoID, branch, missingBranchTarget)
				if err != nil {
					return inbound.CheckoutOutput{}, err
				}
				transition.RestoreEvent = event
				if event != nil {
					ref.BranchID = event.BranchID
				}
			}
			transition.Branch = &ref
		}
		if err := tx.CommitCheckout(ctx, transition); err != nil {
			return inbound.CheckoutOutput{}, err
		}
	} else {
		// Compatibility stores still prepare first. Production FileStore journals
		// creation and worktree selection together through CommitCheckout.
		if in.NewBranch != "" {
			if _, err := s.fork.Fork(ctx, inbound.ForkInput{RepoID: in.RepoID, FromSnapshot: snapID, NewBranch: in.NewBranch}); err != nil {
				return inbound.CheckoutOutput{}, err
			}
		}
		if err := s.store.PutRef(ctx, head); err != nil {
			return inbound.CheckoutOutput{}, err
		}
	}

	return inbound.CheckoutOutput{
		Branch:          branch,
		Head:            snapID,
		WrittenPath:     lo.WrittenPath,
		ResumeCmd:       lo.ResumeCmd,
		Fidelity:        lo.Fidelity,
		ActivatedBranch: missingBranchTarget != "",
	}, nil
}

// Ensure CheckoutSessionService implements inbound.CheckoutSession.
var _ inbound.CheckoutSession = (*CheckoutSessionService)(nil)

// CheckoutAtCode refuses to turn a historical reference into the current work
// selection without an exact code binding. Load remains the read-only alternative.
func (s *CheckoutSessionService) CheckoutAtCode(ctx context.Context, in inbound.CheckoutInput, code string) (inbound.CheckoutOutput, error) {
	if code == "" {
		return inbound.CheckoutOutput{}, fmt.Errorf("code_position_mismatch: current Git commit is unavailable; use load for reference-only input")
	}
	id, err := resolveRef(ctx, s.store, in.RepoID, in.From)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	history, ok := s.store.(outbound.HistoryStore)
	if !ok {
		return inbound.CheckoutOutput{}, fmt.Errorf("code_position_mismatch: snapshot has no verified code binding; use load")
	}
	repo := in.RepoID
	if repo == "" {
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return inbound.CheckoutOutput{}, err
		}
		repo = snap.RepoID
	}
	events, err := history.ListHistoryEvents(ctx, repo)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	matched := false
	for _, event := range events {
		if event.Target == id && event.GitAfter == code {
			matched = true
			break
		}
	}
	if !matched {
		return inbound.CheckoutOutput{}, fmt.Errorf("code_position_mismatch: context %s is not bound to Git %s; switch Git first or use load", id, code)
	}
	return withRetainedObjects(ctx, s.store, func() (inbound.CheckoutOutput, error) { return s.checkout(ctx, in, id, code) })
}
