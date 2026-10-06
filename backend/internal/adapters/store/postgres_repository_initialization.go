//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryInitializationStore = (*PostgresStore)(nil)

func (s *PostgresStore) requireInitializationTransaction(ctx context.Context, repo domain.ContentHash) error {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || tx.readOnly || tx.repo != repo {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) GetRepositoryInitialization(ctx context.Context, repo domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	var receipt domain.RepositoryInitializationReceipt
	if err := domain.ValidateContentHash(repo); err != nil {
		return receipt, err
	}
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, "SELECT creation FROM repository_initializations WHERE repo_id=$1", repo).Scan(&raw)
	if err != nil {
		return receipt, mapNoRows(err)
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Repo.ID != repo || receipt.Anchor != nil {
		return receipt, domain.ErrIntegrity
	}
	return receipt, receipt.Validate()
}

func (s *PostgresStore) GetRepositoryInitializationAnchor(ctx context.Context, repo domain.ContentHash, branch string) (domain.RepositoryInitializationAnchor, error) {
	var anchor domain.RepositoryInitializationAnchor
	if domain.ValidateContentHash(repo) != nil || domain.ValidateBranchName(branch) != nil {
		return anchor, domain.ErrValidation
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, "SELECT anchor FROM repository_initialization_anchors WHERE repo_id=$1 AND name=$2", repo, branch).Scan(&raw); err != nil {
		return anchor, mapNoRows(err)
	}
	if json.Unmarshal(raw, &anchor) != nil || anchor.Ref.Name != branch || anchor.Validate(repo) != nil {
		return anchor, domain.ErrIntegrity
	}
	return anchor, nil
}

func (s *PostgresStore) BeginRepositoryInitialization(ctx context.Context, repo domain.Repo) (domain.RepositoryInitializationReceipt, error) {
	var zero domain.RepositoryInitializationReceipt
	if err := s.requireInitializationTransaction(ctx, repo.ID); err != nil {
		return zero, err
	}
	// The caller cannot choose policy; protected creation is this operation's fixed policy.
	repo.ContextProtocol = 1
	prior, err := s.GetRepositoryInitialization(ctx, repo.ID)
	if err == nil {
		if !reflect.DeepEqual(prior.Repo, repo) {
			return zero, domain.ErrRepositoryInitializationConflict
		}
		return prior, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return zero, err
	}
	if _, err := s.GetRepo(ctx, repo.ID); !errors.Is(err, domain.ErrNotFound) {
		if err != nil {
			return zero, err
		}
		return zero, domain.ErrRepositoryInitializationConflict
	}
	// Absence was established under the very same graph transaction as all
	// ordinary registration. PutRepo's upsert result is never used as that proof.
	repo, err = s.PutRepo(ctx, repo)
	if err != nil {
		return zero, err
	}
	if err := s.EnableContextProtocol(ctx, repo.ID); err != nil {
		return zero, err
	}
	repo.ContextProtocol = 1
	receipt := domain.RepositoryInitializationReceipt{Version: 1, CreationID: domain.NewID("init_"), Repo: repo}
	if err := receipt.Validate(); err != nil {
		return zero, err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return zero, err
	}
	if _, err = s.db(ctx).Exec(ctx, "INSERT INTO repository_initializations(repo_id,creation) VALUES($1,$2)", repo.ID, raw); err != nil {
		return zero, storageWriteError(err)
	}
	return receipt, nil
}

func (s *PostgresStore) FinalizeRepositoryInitialization(ctx context.Context, repo domain.ContentHash, in domain.RepositoryInitializationFinalize, proof outbound.RepositoryInitializationProof) (domain.RepositoryInitializationReceipt, error) {
	var zero domain.RepositoryInitializationReceipt
	if err := s.requireInitializationTransaction(ctx, repo); err != nil {
		return zero, err
	}
	if domain.ValidateRepositoryInitializationID(in.CreationID) != nil || in.Anchor.Validate(repo) != nil {
		return zero, domain.ErrValidation
	}
	receipt, err := s.GetRepositoryInitialization(ctx, repo)
	if err != nil {
		return zero, err
	}
	if receipt.CreationID != in.CreationID {
		return zero, domain.ErrRepositoryInitializationConflict
	}
	accepted, err := s.GetRepositoryInitializationAnchor(ctx, repo, in.Anchor.Ref.Name)
	if err == nil {
		if !accepted.Equal(in.Anchor) {
			return zero, domain.ErrRepositoryInitializationConflict
		}
		receipt.Anchor = &accepted
		return receipt, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return zero, err
	}
	current, err := s.GetRepo(ctx, repo)
	if err != nil {
		return zero, err
	}
	if domain.ValidateRepositoryInitializationContinuity(receipt.Repo, current) != nil {
		return zero, domain.ErrRepositoryInitializationConflict
	}
	if err := s.requireUnpublishedInitializationBranch(ctx, repo, in.Anchor.Ref.Name); err != nil {
		return zero, err
	}
	// Recheck frozen metadata and retain the exact verified object/ownership rows.
	// Full body verification occurred in a prior coherent read snapshot.
	prepared, ok := proof.(*initializationProofPG)
	if !ok || prepared.owner != s || prepared.repo != repo || !prepared.anchor.Equal(in.Anchor) {
		return zero, domain.ErrRepositoryInitializationConflict
	}
	if err := prepared.recheck(ctx); err != nil {
		return zero, err
	}
	snaps := make([]domain.Snapshot, 0, len(in.Anchor.SnapshotStates))
	for id := range in.Anchor.SnapshotStates {
		snap, err := s.GetSnapshot(ctx, repo, id)
		if err != nil {
			return zero, err
		}
		snaps = append(snaps, snap)
	}
	plan, err := domain.SelectBranchPullDependencies(ctx, current, domain.BranchPullRequest{Version: 1, Branch: in.Anchor.Ref.Name}, []domain.Ref{in.Anchor.Ref}, nil, snaps)
	if err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(plan.SnapshotStates, in.Anchor.SnapshotStates) {
		return zero, domain.ErrRepositoryInitializationConflict
	}
	// Only this receipt-backed, unclaimed branch name gets the legacy
	// exception. Generic CAS remains strict. Match CAS's initial reflog semantics.
	ref := in.Anchor.Ref
	tag, err := s.db(ctx).Exec(ctx, `INSERT INTO refs(repo_id,kind,name,target,symbolic,branch_id)
 VALUES($1,'branch',$2,$3,'',$4) ON CONFLICT (repo_id,kind,name) DO NOTHING`, repo, ref.Name, ref.Target, ref.BranchID)
	if err != nil {
		return zero, storageWriteError(err)
	}
	if tag.RowsAffected() != 1 {
		return zero, domain.ErrRepositoryInitializationConflict
	}
	if _, err := s.db(ctx).Exec(ctx, `INSERT INTO reflog(repo_id,kind,name,old,new) VALUES($1,'branch',$2,'',$3)`, repo, ref.Name, ref.Target); err != nil {
		return zero, err
	}
	raw, err := json.Marshal(in.Anchor)
	if err != nil {
		return zero, err
	}
	tag, err = s.db(ctx).Exec(ctx, "INSERT INTO repository_initialization_anchors(repo_id,name,anchor) VALUES($1,$2,$3) ON CONFLICT (repo_id,name) DO NOTHING", repo, ref.Name, raw)
	if err != nil {
		return zero, storageWriteError(err)
	}
	if tag.RowsAffected() != 1 {
		return zero, domain.ErrRepositoryInitializationConflict
	}
	receipt.Anchor = &in.Anchor
	return receipt, nil
}

func (s *PostgresStore) requireUnpublishedInitializationBranch(ctx context.Context, repo domain.ContentHash, branch string) error {
	refs, err := s.ListRefs(ctx, repo)
	if err != nil {
		return err
	}
	history, err := s.ListHistoryEvents(ctx, repo)
	if err != nil {
		return err
	}
	log, err := s.ReadReflog(ctx, repo)
	if err != nil {
		return err
	}
	return domain.ValidateRepositoryInitializationBranch(repo, branch, refs, history, log)
}
