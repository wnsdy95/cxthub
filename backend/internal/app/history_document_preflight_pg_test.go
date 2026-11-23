//go:build postgres

package app

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyDocumentLockObservation struct {
	readOnly, graphAvailable bool
}

// The probe delegates every byte check and mutation to the real PG adapter.
// A separate connection tries the actual repository advisory lock without
// waiting, so these assertions do not depend on elapsed-time thresholds.
type historyDocumentLockProbe struct {
	*store.PostgresStore
	peer         *pgx.Conn
	observations []historyDocumentLockObservation
	applies      int
	afterVerify  func(context.Context) error
	afterApply   func(context.Context) error
	afterAbort   func(error) error
}

func (p *historyDocumentLockProbe) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	err := p.PostgresStore.WithinRepository(ctx, repo, fn)
	if err != nil && p.afterAbort != nil {
		if hookErr := p.afterAbort(err); hookErr != nil {
			return hookErr
		}
	}
	return err
}

func (p *historyDocumentLockProbe) graphAvailable(ctx context.Context, repo domain.ContentHash) (bool, error) {
	tx, err := p.peer.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var available bool
	err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1),1735289201)`, string(repo)).Scan(&available)
	return available, err
}

func (p *historyDocumentLockProbe) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	available, err := p.graphAvailable(ctx, repo)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	p.observations = append(p.observations, historyDocumentLockObservation{p.InReadOnlyTransaction(ctx), available})
	proof, err := p.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
	if err == nil && p.afterVerify != nil {
		err = p.afterVerify(ctx)
	}
	return proof, err
}
