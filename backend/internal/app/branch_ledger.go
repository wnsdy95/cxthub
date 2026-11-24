package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) branchLedgerStore() (outbound.BranchLedgerStore, error) {
	store, ok := s.meta.(outbound.BranchLedgerStore)
	if !ok {
		return nil, fmt.Errorf("%w: branch ledger storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// newBranchLedgerID makes the ID of a lifetime the server opens itself.
func newBranchLedgerID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// branchLedgerLookup reads the two lifetimes the rule looks at: the one alive
// under the name, and the one that holds the ID.
func branchLedgerLookup(ctx context.Context, store outbound.BranchLedgerStore, repo domain.ContentHash, name, id string) (alive, holder *domain.BranchLifetime, err error) {
	if found, err := store.AliveBranchLifetime(ctx, repo, name); err == nil {
		alive = &found
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, err
	}
	if id == "" {
		return alive, nil, nil
	}
	if found, err := store.BranchLifetimeHolding(ctx, repo, id); err == nil {
		holder = &found
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, err
	}
	return alive, holder, nil
}

// RegisterBranchIDs applies the ledger's one rule to the branches a member's
// CLI reports as present on the remote. A report is a claim by a member, not
// proof: it can open a lifetime or join one, and it never closes one.
//
// A registration moves the repository revision: the Sessions tab shows the
// lifetimes of branch names.
func (s *Service) RegisterBranchIDs(ctx context.Context, repo domain.ContentHash, branches []domain.BranchRegistration) ([]domain.BranchRegistrationResult, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if len(branches) > domain.BranchRegistrationLimit {
		return nil, fmt.Errorf("%w: too many branches in one report", domain.ErrValidation)
	}
	for _, branch := range branches {
		if err := branch.Validate(); err != nil {
			return nil, err
		}
	}
	ctx = auditOperation(ctx, "branch_ledger.registered")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) ([]domain.BranchRegistrationResult, error) {
		store, err := s.branchLedgerStore()
		if err != nil {
			return nil, err
		}
		actor, system := inbound.RepositoryActor(bound)
		if system || actor == "" {
			return nil, domain.ErrForbidden
		}
		by := domain.BranchEvidence{Kind: domain.BranchEvidenceRegistered, Account: actor}
		now := time.Now().UTC()
		results := make([]domain.BranchRegistrationResult, 0, len(branches))
		for _, branch := range branches {
			alive, holder, err := branchLedgerLookup(bound, store, repo, branch.Name, branch.LocalID)
			if err != nil {
				return nil, err
			}
			var failure error
			lifetime, outcome, changed := domain.RegisterBranchID(repo, branch.Name, branch.LocalID, alive, holder, by, now, func() string {
				id, err := newBranchLedgerID()
				failure = err
				return id
			})
			if failure != nil {
				return nil, failure
			}
			if changed {
				if err := store.PutBranchLifetime(bound, lifetime); err != nil {
					return nil, err
				}
			}
			results = append(results, domain.BranchRegistrationResult{Name: branch.Name, LocalID: branch.LocalID, LedgerID: lifetime.LedgerID, Outcome: outcome})
		}
		return results, nil
	})
}

// ListBranchLifetimes lists the lifetimes of the repository's branch names,
// of one name when name is not empty, oldest first.
func (s *Service) ListBranchLifetimes(ctx context.Context, repo domain.ContentHash, name string) ([]domain.BranchLifetime, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if name != "" {
		if err := domain.ValidateBranchLedgerName(name); err != nil {
			return nil, err
		}
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.BranchLifetime, error) {
		store, err := s.branchLedgerStore()
		if err != nil {
			return nil, err
		}
		return store.ListBranchLifetimes(bound, repo, name)
	})
}

var _ inbound.BranchLedger = (*Service)(nil)

// recordBranchLedgerObservation carries a stored push observation into the
// ledger, inside the transaction that stored it: a push that leaves the name
// with a commit shows the name alive, a push that leaves it without one is
// its deletion. A store without a ledger records nothing.
//
// An observation that already opened or closed a lifetime of the name changes
// nothing when its delivery is processed again.
func recordBranchLedgerObservation(tx context.Context, meta any, observation domain.GitRefObservation, at time.Time) error {
	store, ok := meta.(outbound.BranchLedgerStore)
	if !ok || !strings.HasPrefix(observation.Ref, "refs/heads/") || observation.ID == "" {
		return nil
	}
	name := strings.TrimPrefix(observation.Ref, "refs/heads/")
	if domain.ValidateBranchLedgerName(name) != nil {
		return nil // a name the ledger cannot hold is left out of it
	}
	lifetimes, err := store.ListBranchLifetimes(tx, observation.RepoID, name)
	if err != nil {
		return err
	}
	var alive *domain.BranchLifetime
	for index := range lifetimes {
		if lifetimes[index].SawObservation(observation.ID) {
			return nil
		}
		if lifetimes[index].Alive() {
			alive = &lifetimes[index]
		}
	}
	by := domain.BranchEvidence{Kind: domain.BranchEvidenceHostingPush, Source: observation.ID}
	var lifetime domain.BranchLifetime
	var changed bool
	if strings.Trim(observation.After, "0") == "" {
		lifetime, _, changed = domain.ObserveBranchDeleted(alive, by, at)
	} else {
		var failure error
		lifetime, _, changed = domain.ObserveBranchOnRemote(observation.RepoID, name, alive, by, at, func() string {
			id, err := newBranchLedgerID()
			failure = err
			return id
		})
		if failure != nil {
			return failure
		}
	}
	if !changed {
		return nil
	}
	return store.PutBranchLifetime(tx, lifetime)
}
