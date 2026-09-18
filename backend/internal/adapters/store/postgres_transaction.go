//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pgDatabase interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type repositoryTxKey struct{}
type repositoryTx struct {
	pgx.Tx
	owner     *PostgresStore
	repo      domain.ContentHash
	workspace string
	readOnly  bool
}

// Inner storage operations use savepoints; releasing one never commits the
// outer business operation. Isolation belongs to that outer transaction.
func (t *repositoryTx) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return t.Tx.Begin(ctx)
}

func (s *PostgresStore) db(ctx context.Context) pgDatabase {
	if tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if tx.owner == s {
			return tx
		}
		return foreignTransactionDB{}
	}
	return s.pool
}

// A miswired service must fail instead of silently writing a second store
// outside the transaction. Separate replicas use separate request contexts.
type foreignTransactionDB struct{}

func (foreignTransactionDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, domain.ErrConflict
}
func (foreignTransactionDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, domain.ErrConflict
}
func (foreignTransactionDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return foreignTransactionDB{}
}
func (foreignTransactionDB) Scan(...any) error                     { return domain.ErrConflict }
func (foreignTransactionDB) Begin(context.Context) (pgx.Tx, error) { return nil, domain.ErrConflict }
func (foreignTransactionDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, domain.ErrConflict
}

var _ outbound.RepositoryTransactions = (*PostgresStore)(nil)
var _ outbound.PRJobFence = (*PostgresStore)(nil)

func rollbackPG(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *PostgresStore) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != s || prior.readOnly || prior.repo != repo {
			return fmt.Errorf("%w: incompatible nested repository transaction", domain.ErrConflict)
		}
		return fn(ctx)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err = lockRepoGraph(ctx, tx, repo); err != nil {
		return err
	}
	// Pin repository identity and policy for the complete operation.
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM repos WHERE id=$1 FOR SHARE`, string(repo)).Scan(&id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return mapNoRows(err)
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s, repo: repo})
	if err = fn(bound); err != nil {
		return err
	}
	// Deferred quota triggers run at the outer commit, not at savepoint release.
	return storageWriteError(tx.Commit(ctx))
}

func (s *PostgresStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != s {
			return domain.ErrConflict
		}
		return fn(ctx)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err = fn(context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s, readOnly: true})); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) FencePRJob(ctx context.Context, j domain.PRPromotionJob) error {
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != s || tx.repo != j.RepoID || tx.readOnly {
		return domain.ErrConflict
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT state='running' AND version=$3 AND lease_until>clock_timestamp()
 FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID, j.Version).Scan(&valid)
	if err != nil {
		return mapNoRows(err)
	}
	if !valid {
		return domain.ErrRefConflict
	}
	return nil
}

func (s *PostgresStore) CompareAndSwapSecrets(ctx context.Context, repo domain.ContentHash, expected, next []byte) error {
	return s.WithinRepository(ctx, repo, func(ctx context.Context) error {
		old, err := s.GetSecretsEnvelope(ctx, repo)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if !bytes.Equal(old, expected) {
			return domain.ErrRefConflict
		}
		return s.PutSecretsEnvelope(ctx, repo, next)
	})
}
