package app

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// CheckoutSessionService implements the CheckoutSession inbound port use-case service (_RECONCILIATION section).
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

	// Output branch label determination: -b for new branch, otherwise the branch name from From.
	branch := in.NewBranch
	if branch == "" && in.From != "" && in.From != "HEAD" && !strings.HasPrefix(in.From, "sha256:") {
		branch = in.From
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

	lo, err := s.load.Load(ctx, inbound.LoadInput{
		RepoID:         in.RepoID,
		Ref:            string(snapID),
		TargetProvider: in.TargetProvider,
		Mode:           in.Mode,
		Cwd:            in.Cwd,
	})
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}

	return inbound.CheckoutOutput{
		Branch:      branch,
		Head:        snapID,
		WrittenPath: lo.WrittenPath,
		ResumeCmd:   lo.ResumeCmd,
		Fidelity:    lo.Fidelity,
	}, nil
}

// Ensure CheckoutSessionService implements inbound.CheckoutSession.
var _ inbound.CheckoutSession = (*CheckoutSessionService)(nil)
