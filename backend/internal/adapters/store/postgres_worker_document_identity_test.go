//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
	"time"
)

func TestWorkerPGQueueIdentity(t *testing.T) {
	st, _ := chunkReusePG(t)
	checkWorkerQueueIdentity(t, st)
}
func TestWorkerPGMetadataPinAndRollback(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, e := st.PutRepo(ctx, domain.Repo{ID: repo}); e != nil {
		t.Fatal(e)
	}
	tx, e := st.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer rollbackPG(tx)
	if e = st.checkRepositoryDocumentIdentity(ctx, tx, repo, true); e != nil {
		t.Fatal(e)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- st.RequireDocumentIdentity(bounded, repo, domain.DocumentIdentityRootV1) }()
	select {
	case e := <-done:
		t.Fatal("opt-in crossed retained policy", e)
	case <-time.After(30 * time.Millisecond):
	}
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if e = st.checkRepositoryDocumentIdentity(ctx, st.db(ctx), repo, false); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal(e)
	}
	// The existing savepoint and outer transaction still roll back all receipts.
	t.Run(time.Now().Format("150405.000000000"), func(t *testing.T) { checkPGPRJobDiagnosticRollback(t, st) })
}
func TestWorkerPGPRWakePolicy(t *testing.T) {
	st, _ := chunkReusePG(t)
	t.Run(time.Now().Format("150405.000000000"), func(t *testing.T) { checkWorkerPRWakePolicy(t, st) })
}

func TestWorkerPGPRLegacyRegressions(t *testing.T) {
	st, _ := chunkReusePG(t)
	t.Run(time.Now().Format("150405.000000000"), func(t *testing.T) {
		t.Run("claims", func(t *testing.T) { checkPRJobs(t, st) })
		t.Run("wake", func(t *testing.T) { checkPRSourceWake(t, st) })
		t.Run("reawakened", func(t *testing.T) { checkPRReawakenedSerialization(t, st) })
		t.Run("wake-claim-race", func(t *testing.T) { checkPGPRClaimWakeRace(t, st) })
	})
}
