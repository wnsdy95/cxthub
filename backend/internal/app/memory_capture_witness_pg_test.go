//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGCapturedMemoryWitnessCurrentSourceAndHistoricalBoundary(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx := systemTestContext()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	snapshot := collaborationSnapshot(test, storage, repo, "witness selected")
	hash, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "verified source"))
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: hash, CodeCommit: effectiveOID(3)}, Limit: 1}
	capture := func() *CapturedEffectiveMemory {
		test.Helper()
		captured, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
		if err != nil {
			test.Fatal(err)
		}
		if err := service.ValidateCapturedMemorySource(ctx, repo, captured); err != nil {
			test.Fatal("fresh capture failed current validation", err)
		}
		return captured
	}
	captured := capture()
	witnesses, err := captured.ObjectWitnesses(ctx)
	if err != nil || len(witnesses) != 1 || witnesses[0].Hash != hash {
		test.Fatal("selected dependency witnesses lost", err)
	}
	witnesses[0].Root.Version++
	if err := service.ValidateCapturedMemorySource(ctx, repo, captured); err != nil {
		test.Fatal("external mutation poisoned source witness", err)
	}
	page, err := captured.Project(ctx, request)
	if err != nil {
		test.Fatal(err)
	}
	restored := roundTripMemoryCapturePG(test, storage, collaborationDSN(test), captured)
	if err := service.ValidateCapturedMemorySource(ctx, repo, restored); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("historical bundle implied current proof", err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, hh("foreign"), captured); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("cross-repository witness accepted", err)
	}
	denied := inbound.WithDocumentReadAuthorizer(ctx, func(context.Context, domain.ContentHash) (context.Context, error) {
		return nil, domain.ErrForbidden
	})
	if err := service.ValidateCapturedMemorySource(denied, repo, captured); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("witness bypassed current authorization", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := storage.ReadRepositoryMemorySourceGeneration(bound, repo); err != nil {
			return err
		}
		if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
			return err
		}
		if err := service.ValidateCapturedMemorySource(bound, repo, captured); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("current validation borrowed a stale snapshot", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil || generation != captured.Generation() {
		test.Fatal("byte-only mutation unexpectedly changed repository generation", err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, captured); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("same-generation physical write reused prior verification", err)
	}
	if historical, err := captured.Project(ctx, request); err != nil || !reflect.DeepEqual(historical, page) {
		test.Fatal("current invalidation mutated historical replay", err)
	}
	fresh := capture()
	if _, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, repo, hash); err != nil {
		test.Fatal(err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, fresh); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("removed source ownership reused current capture", err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, repo, hash); err != nil {
		test.Fatal(err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, fresh); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("revoked and regranted source erased intervening changes", err)
	}
	fresh = capture()
	if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' changed' WHERE repo_id=$1 AND id=$2`, repo, snapshot); err != nil {
		test.Fatal(err)
	}
	if err := service.ValidateCapturedMemorySource(ctx, repo, fresh); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("graph mutation escaped source validation", err)
	}
}
