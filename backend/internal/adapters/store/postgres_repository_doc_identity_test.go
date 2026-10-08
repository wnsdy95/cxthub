//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
	"time"
)

func TestP6PGRequirementPersistenceAndGuard(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityLegacy); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("downgrade", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE repos SET required_doc_identity='' WHERE id=$1`, string(repo)); err == nil {
		t.Fatal("SQL downgrade bypassed migration trigger")
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentity("future")); err == nil {
		t.Fatal("unknown accepted")
	}
	branch := "other"
	if err := s.UpdateRepoConfig(ctx, repo, &branch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepo(ctx, repo)
	if err != nil || r.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal("requirement lost", r, err)
	}
	if _, err := s.PutRepo(ctx, domain.Repo{ID: domain.HashContent([]byte(t.Name())), RequiredDocIdentity: domain.DocumentIdentityRootV1}); err == nil {
		t.Fatal("registration opted in")
	}
}
func TestP6PGMerkleStagedBeforeOptInCannotPublish(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	build := catalogMerklePreparedPG(t, ctx, s, repo)
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := s.publishCatalogMerkle(ctx, repo, build); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("staged cache bypass", err)
	}
	n, r := catalogMerkleCountsPG(t, ctx, s, repo)
	if n != 0 || r != 0 {
		t.Fatalf("cache effects=%d/%d", n, r)
	}
}
func TestP6PGMerkleWarmFrozenAndSourcePages(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	warm := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	n, r := catalogMerkleCountsPG(t, ctx, s, repo)
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	for _, req := range []domain.CatalogMerkleRequest{{Version: 1}, {Version: 1, RootHash: warm.RootHash, Checkpoint: &warm.Checkpoint}} {
		if _, err := s.CatalogMerkle(ctx, repo, req); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
			t.Fatal("old peer served", err)
		}
	}
	if _, err := s.CatalogChanges(ctx, repo, domain.CatalogRequest{Version: 1}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("source page bypass", err)
	}
	// Explicit synthetic capable peer+binary validates adapter semantics only.
	capable := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	got, err := s.CatalogMerkle(capable, repo, domain.CatalogMerkleRequest{Version: 1})
	if err != nil || got.RootHash != warm.RootHash {
		t.Fatal("capable frozen read", err)
	}
	n2, r2 := catalogMerkleCountsPG(t, ctx, s, repo)
	if n != n2 || r != r2 {
		t.Fatal("warm/denied reads changed cache")
	}
	peerOnly := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, nil)
	if _, err := s.CatalogMerkle(peerOnly, repo, domain.CatalogMerkleRequest{Version: 1}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("peer forged binary", err)
	}
}
func TestP6PGReadPinsRequirementAcrossConcurrentOptIn(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	err := s.WithinReadSnapshot(ctx, func(read context.Context) error {
		before, err := s.GetRepo(read, repo)
		if err != nil || before.RequiredDocIdentity != "" {
			t.Fatal(before, err)
		}
		done := make(chan error, 1)
		go func() { done <- s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1) }()
		if err := <-done; err != nil {
			return err
		}
		return s.checkRepositoryDocumentIdentity(read, s.db(read), repo, false)
	})
	if err != nil {
		t.Fatal("read did not retain its snapshot", err)
	}
	if err = s.WithinReadSnapshot(ctx, func(read context.Context) error {
		return s.checkRepositoryDocumentIdentity(read, s.db(read), repo, false)
	}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("next snapshot missed opt-in", err)
	}
}

func TestP6PGCacheRequirementPinBlocksConcurrentOptIn(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(tx)
	if err = s.checkRepositoryDocumentIdentity(ctx, tx, repo, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1) }()
	select {
	case err = <-done:
		t.Fatalf("opt-in crossed cache policy pin: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
