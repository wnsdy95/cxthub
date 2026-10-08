package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.RepositoryInitialization = (*Service)(nil)
var _ inbound.RepositoryInitializationQuery = (*Service)(nil)

func (s *Service) GetRepositoryInitializationView(ctx context.Context, id domain.ContentHash, branch string) (domain.Repo, bool, error) {
	if branch != "" && domain.ValidateBranchName(branch) != nil {
		return domain.Repo{}, false, domain.ErrValidation
	}
	var repo domain.Repo
	available, err := repositoryRead(ctx, s, func(tx context.Context) (bool, error) {
		var err error
		repo, err = s.meta.GetRepo(tx, id)
		if err != nil {
			return false, err
		}
		// Basic discovery may return metadata to an old peer, but no state query.
		if err := repo.RequiredDocIdentity.Validate(); err != nil {
			return false, err
		}
		if !hasDocumentIdentity(s.DocumentIdentitiesSupported(), repo.RequiredDocIdentity) || !hasDocumentIdentity(inbound.DocumentIdentities(tx), repo.RequiredDocIdentity) {
			return false, nil
		}
		if branch == "" {
			return false, nil
		}
		st, err := s.initializationStore()
		if errors.Is(err, domain.ErrRepositoryInitializationUnsupported) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		receipt, err := st.GetRepositoryInitialization(tx, id)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if domain.ValidateRepositoryInitializationContinuity(receipt.Repo, repo) != nil {
			return false, nil
		}
		if _, err := st.GetRepositoryInitializationAnchor(tx, id, branch); err == nil {
			return false, nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return false, err
		}
		refs, err := s.meta.ListRefs(tx, id)
		if err != nil {
			return false, err
		}
		history, ok := s.meta.(outbound.HistoryStore)
		if !ok {
			return false, domain.ErrRepositoryInitializationUnsupported
		}
		events, err := history.ListHistoryEvents(tx, id)
		if err != nil {
			return false, err
		}
		log, err := s.meta.ReadReflog(tx, id)
		if err != nil {
			return false, err
		}
		err = domain.ValidateRepositoryInitializationBranch(id, branch, refs, events, log)
		if errors.Is(err, domain.ErrRepositoryInitializationConflict) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return domain.Repo{}, false, err
	}
	return repo, available, nil
}

func (s *Service) initializationStore() (outbound.RepositoryInitializationStore, error) {
	st, ok := s.meta.(outbound.RepositoryInitializationStore)
	_, transactional := s.meta.(outbound.RepositoryTransactions)
	if !ok || !transactional {
		return nil, domain.ErrRepositoryInitializationUnsupported
	}
	return st, nil
}

// Receipt discovery is privileged and coherent, but does not advance graph
// revisions or audit a read as a repository mutation.
func (s *Service) GetRepositoryInitialization(ctx context.Context, id domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	st, err := s.initializationStore()
	if err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	var receipt domain.RepositoryInitializationReceipt
	err = s.meta.(outbound.RepositoryTransactions).WithinRepository(writeAction(ctx, "manage"), id, func(tx context.Context) error {
		if err := s.authorizeRepositoryWrite(tx, id); err != nil {
			return err
		}
		if err := s.checkDocumentIdentity(tx, id, false); err != nil {
			return err
		}
		var err error
		receipt, err = st.GetRepositoryInitialization(tx, id)
		return err
	})
	if err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	return receipt, nil
}

func (s *Service) BeginRepositoryInitialization(ctx context.Context, actor string, id domain.ContentHash, in domain.RepositoryInitializationRequest) (domain.RepositoryInitializationReceipt, error) {
	st, err := s.initializationStore()
	if err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	if actor == "" {
		return domain.RepositoryInitializationReceipt{}, domain.ErrUnauthorized
	}
	repo := domain.Repo{ID: id, RemoteURL: in.RemoteURL, GitRemoteURL: in.GitRemoteURL, DefaultBranch: in.DefaultBranch}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}
	ctx = writeAction(inbound.WithRepositoryActor(ctx, actor), "register")
	ctx = context.WithValue(ctx, revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, id, func(tx context.Context) (domain.RepositoryInitializationReceipt, error) {
		var zero domain.RepositoryInitializationReceipt
		repo, err := s.resolveRepoRegistrationIdentity(tx, actor, repo)
		if err != nil {
			return zero, err
		}
		locker, ok := s.repositories.(outbound.RepositoryAccessLocker)
		if !ok {
			return zero, domain.ErrForbidden
		}
		if err := locker.LockRepositoryAccess(tx, repo.RepositoryID, actor); err != nil {
			return zero, err
		}
		record, err := s.repositories.GetRepository(tx, repo.RepositoryID)
		if err != nil {
			return zero, err
		}
		role, ok, err := repositoryRoleFor(tx, s.repositories, record, actor)
		if err != nil {
			return zero, err
		}
		if !ok || record.Archived || !role.AtLeast(domain.RoleMaintainer) {
			return zero, domain.ErrForbidden
		}
		return st.BeginRepositoryInitialization(tx, repo)
	})
}

func (s *Service) FinalizeRepositoryInitialization(ctx context.Context, id domain.ContentHash, in domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error) {
	st, err := s.initializationStore()
	if err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	if domain.ValidateRepositoryInitializationID(in.CreationID) != nil || in.Anchor.Validate(id) != nil {
		return domain.RepositoryInitializationReceipt{}, domain.ErrValidation
	}
	// A short authorized preflight handles exact accepted replay before touching
	// today's object bodies. Current authority is checked again at final apply.
	manage := writeAction(ctx, "manage")
	receipt, err := repositoryWrite(context.WithValue(manage, revisionScopeKey{}, "none"), s, id, func(tx context.Context) (domain.RepositoryInitializationReceipt, error) {
		r, err := initializationReceiptForBranch(tx, st, id, in.Anchor.Ref.Name)
		if err != nil {
			return r, err
		}
		if r.CreationID != in.CreationID || (r.Anchor != nil && !r.Anchor.Equal(in.Anchor)) {
			return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
		}
		return r, nil
	})
	if err != nil || receipt.Anchor != nil {
		return receipt, err
	}
	proof, verificationErr := repositoryReadForRepo(ctx, s, id, func(read context.Context) (outbound.RepositoryInitializationProof, error) {
		state, ok := s.meta.(outbound.RepositoryTransactionState)
		if !ok || !state.InReadOnlyTransaction(read) {
			return nil, domain.ErrRepositoryInitializationUnsupported
		}
		evidence, err := s.verifyInitializationAnchor(read, receipt.Repo, in.Anchor)
		if err != nil {
			return nil, err
		}
		return st.CaptureRepositoryInitialization(read, id, in.Anchor, evidence)
	})
	return repositoryWrite(manage, s, id, func(tx context.Context) (domain.RepositoryInitializationReceipt, error) {
		// Another exact request may have completed while validation ran. Its receipt
		// wins even if current objects changed and our redundant verification failed.
		r, err := initializationReceiptForBranch(tx, st, id, in.Anchor.Ref.Name)
		if err != nil {
			return r, err
		}
		if r.CreationID != in.CreationID || (r.Anchor != nil && !r.Anchor.Equal(in.Anchor)) {
			return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
		}
		if r.Anchor != nil {
			return r, nil
		}
		if verificationErr != nil {
			return domain.RepositoryInitializationReceipt{}, verificationErr
		}
		return st.FinalizeRepositoryInitialization(tx, id, in, proof)
	})
}

func initializationReceiptForBranch(ctx context.Context, st outbound.RepositoryInitializationStore, id domain.ContentHash, branch string) (domain.RepositoryInitializationReceipt, error) {
	r, err := st.GetRepositoryInitialization(ctx, id)
	if err != nil {
		return r, err
	}
	a, err := st.GetRepositoryInitializationAnchor(ctx, id, branch)
	if errors.Is(err, domain.ErrNotFound) {
		return r, nil
	}
	if err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	r.Anchor = &a
	return r, nil
}

func (s *Service) verifyInitializationAnchor(ctx context.Context, repo domain.Repo, a domain.RepositoryInitializationAnchor) (outbound.RepositoryInitializationEvidence, error) {
	var evidence outbound.RepositoryInitializationEvidence
	snaps := make([]domain.Snapshot, 0, len(a.SnapshotStates))
	byID := make(map[domain.ContentHash]domain.Snapshot, len(a.SnapshotStates))
	for id := range a.SnapshotStates {
		snap, err := s.meta.GetSnapshot(ctx, repo.ID, id)
		if err != nil {
			return evidence, err
		}
		snaps = append(snaps, snap)
		byID[id] = snap
	}
	// Reuse the pure dependency planner with the proposed single ref. It reads
	// no remote state and does not assert a birth or adopt the proposed pointer.
	plan, err := domain.SelectBranchPullDependencies(ctx, repo, domain.BranchPullRequest{Version: 1, Branch: a.Ref.Name}, []domain.Ref{a.Ref}, nil, snaps)
	if err != nil {
		return evidence, err
	}
	if !reflect.DeepEqual(plan.SnapshotStates, a.SnapshotStates) {
		return evidence, domain.ErrRepositoryInitializationConflict
	}
	for _, setting := range plan.SettingsObjects {
		bundle, err := s.meta.GetSettingsObject(ctx, repo.ID, setting.Hash)
		if err != nil {
			return evidence, err
		}
		if err := domain.ValidateSettingsBundle(setting.Kind, setting.Hash, bundle); err != nil {
			return evidence, err
		}
	}
	for _, id := range plan.SnapshotIndex {
		snap := byID[id]
		if err := s.verifyStoredSnapshotDoc(ctx, repo.ID, snap); err != nil {
			return evidence, err
		}
		memories, err := s.verifyInitializationMemory(ctx, repo.ID, id, snap.MemoryHash)
		if err != nil {
			return evidence, err
		}
		evidence.Memories = append(evidence.Memories, memories...)
	}
	evidence.Snapshots = snaps
	return evidence, nil
}

func (s *Service) verifyInitializationMemory(ctx context.Context, repo, owner, root domain.ContentHash) ([]domain.ContentHash, error) {
	var verified []domain.ContentHash
	seen := map[domain.ContentHash]bool{}
	for id, depth := root, 0; id != ""; depth++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if depth >= 1024 || seen[id] {
			return nil, domain.ErrIntegrity
		}
		seen[id] = true
		memory, err := s.blobs.GetMemory(ctx, repo, id)
		if err != nil {
			return nil, err
		}
		hash, err := domain.MemoryDigestHash(memory)
		if err != nil {
			return nil, err
		}
		if hash != id || memory.SnapshotID != owner || domain.ValidateOptionalContentHash(memory.PreviousMemoryHash) != nil {
			return nil, domain.ErrIntegrity
		}
		verified = append(verified, id)
		id = memory.PreviousMemoryHash
	}
	return verified, nil
}
