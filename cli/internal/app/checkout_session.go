package app

import (
	"context"
	"errors"
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
//  2. If NewBranch != "", then ForkSession.Fork(ID → NewBranch) for branching (equivalent to checkout -b).
//  3. LoadSession.Load(ref=ID, TargetProvider, Mode, Cwd) for restoration (including cross-provider conversion).
//  4. Return CheckoutOutput{Branch, Head, WrittenPath, ResumeCmd, Fidelity}.
type CheckoutSessionService struct {
	fork  inbound.ForkSession
	load  inbound.LoadSession
	store outbound.SessionStore
}

// NewCheckoutSessionService creates and injects dependencies for CheckoutSessionService.
func NewCheckoutSessionService(fork inbound.ForkSession, load inbound.LoadSession, store outbound.SessionStore) *CheckoutSessionService {
	return &CheckoutSessionService{fork: fork, load: load, store: store}
}

// Checkout restores to the target provider session (branching if necessary).
func (s *CheckoutSessionService) Checkout(ctx context.Context, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
	snapID, err := resolveRef(ctx, s.store, in.RepoID, in.From)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}

	// Output branch label determination: -b for new branch, otherwise only an
	// actual branch ref from From. Tags and direct hashes are detached restores
	// and must not become symbolic HEAD values.
	branch := in.NewBranch
	var missingBranchTarget domain.ContentHash
	if branch == "" && in.From != "" && in.From != "HEAD" && !strings.HasPrefix(in.From, "sha256:") {
		if _, branchErr := s.store.GetRef(ctx, in.RepoID, domain.RefBranch, in.From); branchErr == nil {
			branch = in.From
		} else if event, ok, lifecycleErr := branchLifecycleByName(ctx, s.store, in.RepoID, in.From); lifecycleErr != nil {
			return inbound.CheckoutOutput{}, lifecycleErr
		} else if ok {
			branch = in.From
			missingBranchTarget = event.Target
		}
	}

	if in.NewBranch != "" {
		if _, err := s.fork.Fork(ctx, inbound.ForkInput{
			RepoID:       in.RepoID,
			FromSnapshot: snapID,
			NewBranch:    in.NewBranch,
		}); err != nil {
			return inbound.CheckoutOutput{}, err
		}
	}

	lo := inbound.LoadOutput{}
	if !in.SkipMaterialize {
		lo, err = s.load.Load(ctx, inbound.LoadInput{
			RepoID:         in.RepoID,
			Ref:            string(snapID),
			TargetProvider: in.TargetProvider,
			Mode:           in.Mode,
			Cwd:            in.Cwd,
		})
		if err != nil {
			return inbound.CheckoutOutput{}, err
		}
	}
	// Loading is normally the preflight for an archived (or crash-interrupted)
	// branch projection. Desktop-app mode deliberately preserves a live session
	// and separately preflights its bounded hook handoff, so resolving the target
	// snapshot is sufficient and no provider session file is created.
	if missingBranchTarget != "" {
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
	if branch != "" {
		if err := s.store.PutRef(ctx, domain.Ref{
			Kind: domain.RefHEAD, Name: "HEAD", RepoID: in.RepoID, Symbolic: branch,
		}); err != nil {
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
