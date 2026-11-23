//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyDocumentProofKeyPG struct{}

type historyRootReferencePG struct {
}

// Only version metadata survives preparation. This evidence is not a durable
// certificate. A root reference it reuses was established in the same read
// transaction, by full byte reads or by the completed finalization record.
type historyDocumentProofPG struct {
	owner                       *PostgresStore
	repo                        domain.ContentHash
	active, attempted, captured bool
	snapshots, blobs            map[string]string
	rootReferences              map[domain.DocumentRef]historyRootReferencePG
}

func (store *PostgresStore) rememberHistoryRootReference(ctx context.Context, repo domain.ContentHash, proof domain.VerifiedDocReference) {
	scope, ok := ctx.Value(historyDocumentProofKeyPG{}).(*historyDocumentProofPG)
	if !ok || scope.owner != store || scope.repo != repo || scope.attempted || !proof.Valid() || proof.DocumentRef().Identity != domain.DocumentIdentityRootV1 {
		return
	}
	if scope.transaction(ctx, true) != nil {
		return
	}
	if scope.rootReferences == nil {
		scope.rootReferences = make(map[domain.DocumentRef]historyRootReferencePG)
	}
	scope.rootReferences[proof.DocumentRef()] = historyRootReferencePG{}
}

func (p *historyDocumentProofPG) transaction(ctx context.Context, readOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(historyDocumentProofKeyPG{}) != p || !p.active {
		return domain.ErrConflict
	}
	tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || tx.owner != p.owner || tx.readOnly != readOnly || (!readOnly && tx.repo != p.repo) {
		return domain.ErrConflict
	}
	return nil
}
