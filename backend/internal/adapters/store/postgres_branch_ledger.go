//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.BranchLedgerStore = (*PostgresStore)(nil)

func scanBranchLifetime(row pgx.Row, repo domain.ContentHash) (domain.BranchLifetime, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		return domain.BranchLifetime{}, err
	}
	var lifetime domain.BranchLifetime
	if err := json.Unmarshal(body, &lifetime); err != nil {
		return domain.BranchLifetime{}, fmt.Errorf("%w: invalid stored branch lifetime: %v", domain.ErrIntegrity, err)
	}
	if err := lifetime.Validate(); err != nil || lifetime.RepoID != repo {
		return domain.BranchLifetime{}, fmt.Errorf("%w: invalid stored branch lifetime", domain.ErrIntegrity)
	}
	return lifetime, nil
}

func (s *PostgresStore) AliveBranchLifetime(ctx context.Context, repo domain.ContentHash, name string) (domain.BranchLifetime, error) {
	if err := validateHash(repo); err != nil {
		return domain.BranchLifetime{}, err
	}
	lifetime, err := scanBranchLifetime(s.db(ctx).QueryRow(ctx, `SELECT body FROM branch_lifetimes WHERE repo_id=$1 AND name=$2 AND closed_at IS NULL`, repo, name), repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BranchLifetime{}, domain.ErrNotFound
	}
	return lifetime, err
}

func (s *PostgresStore) BranchLifetimeHolding(ctx context.Context, repo domain.ContentHash, id string) (domain.BranchLifetime, error) {
	if err := validateHash(repo); err != nil {
		return domain.BranchLifetime{}, err
	}
	lifetime, err := scanBranchLifetime(s.db(ctx).QueryRow(ctx, `SELECT lifetime.body FROM branch_ledger_ids held
 JOIN branch_lifetimes lifetime ON lifetime.repo_id=held.repo_id AND lifetime.ledger_id=held.ledger_id
 WHERE held.repo_id=$1 AND held.id=$2`, repo, id), repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BranchLifetime{}, domain.ErrNotFound
	}
	return lifetime, err
}

func (s *PostgresStore) ListBranchLifetimes(ctx context.Context, repo domain.ContentHash, name string) ([]domain.BranchLifetime, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT body FROM branch_lifetimes WHERE repo_id=$1 AND ($2='' OR name=$2) ORDER BY opened_at, ledger_id`, repo, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lifetimes := []domain.BranchLifetime{}
	for rows.Next() {
		lifetime, err := scanBranchLifetime(rows, repo)
		if err != nil {
			return nil, err
		}
		lifetimes = append(lifetimes, lifetime)
	}
	return lifetimes, rows.Err()
}

// PutBranchLifetime stores the lifetime and the IDs it holds as one change of
// the caller's repository transaction. The table's own constraints refuse a
// second lifetime alive under the name and an ID another lifetime holds.
func (s *PostgresStore) PutBranchLifetime(ctx context.Context, lifetime domain.BranchLifetime) error {
	if err := lifetime.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(lifetime)
	if err != nil {
		return err
	}
	db := s.db(ctx)
	// Refuse before writing anything, so that a refused lifetime leaves no
	// row behind even for a caller without a transaction of its own.
	var taken bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_ledger_ids WHERE repo_id=$1 AND id=ANY($2) AND ledger_id<>$3)`,
		lifetime.RepoID, lifetime.IDs(), lifetime.LedgerID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("%w: a branch ID belongs to two lifetimes", domain.ErrConflict)
	}
	if lifetime.Alive() {
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_lifetimes WHERE repo_id=$1 AND name=$2 AND closed_at IS NULL AND ledger_id<>$3)`,
			lifetime.RepoID, lifetime.Name, lifetime.LedgerID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return fmt.Errorf("%w: two lifetimes are alive under one branch name", domain.ErrConflict)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO branch_lifetimes(repo_id,ledger_id,name,opened_at,closed_at,body) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT (repo_id,ledger_id) DO UPDATE SET name=EXCLUDED.name, opened_at=EXCLUDED.opened_at, closed_at=EXCLUDED.closed_at, body=EXCLUDED.body`,
		lifetime.RepoID, lifetime.LedgerID, lifetime.Name, lifetime.OpenedAt, lifetime.ClosedAt, body); err != nil {
		return branchLedgerConflict(err)
	}
	for _, id := range lifetime.IDs() {
		tag, err := db.Exec(ctx, `INSERT INTO branch_ledger_ids(repo_id,id,ledger_id) VALUES($1,$2,$3)
 ON CONFLICT (repo_id,id) DO UPDATE SET ledger_id=branch_ledger_ids.ledger_id WHERE branch_ledger_ids.ledger_id=EXCLUDED.ledger_id`, lifetime.RepoID, id, lifetime.LedgerID)
		if err != nil {
			return branchLedgerConflict(err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: a branch ID belongs to two lifetimes", domain.ErrConflict)
		}
	}
	return nil
}

// branchLedgerConflict names a refused write by what the ledger promises.
func branchLedgerConflict(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return fmt.Errorf("%w: the branch ledger already holds that name or ID", domain.ErrConflict)
	}
	return err
}
