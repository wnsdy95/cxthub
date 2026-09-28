//go:build postgres

package app

import (
	"errors"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGMemoryReuse(t *testing.T) {
	svc, _, repo := collaborationPG(t)
	testMemoryReuse(t, svc, repo)
}

func TestPGMemoryReuseConcurrentCASAndRollback(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	source := reuseSnapshot(t, svc, repo, "source")
	target := reuseSnapshot(t, svc, repo, "target")
	d := domain.MemoryDigest{SnapshotID: source, Summary: "selected memory"}
	base, err := svc.PutMemoryDigestCAS(ctx, repo, d)
	if err != nil {
		t.Fatal(err)
	}
	d.SnapshotID = target
	providers := []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex}
	var inputs []inbound.MemoryReuse
	for _, provider := range providers {
		d.Provider = provider
		hash, _ := domain.MemoryDigestHash(d)
		inputs = append(inputs, inbound.MemoryReuse{Version: 1, BaseHash: base, MemoryHash: hash, Provider: provider})
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, in := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.ReuseMemoryDigest(ctx, repo, target, in)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, conflicts := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, domain.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("accepted=%d conflicts=%d", accepted, conflicts)
	}
	rollbackTarget := reuseSnapshot(t, svc, repo, "rollback")
	d.SnapshotID = rollbackTarget
	hash, _ := domain.MemoryDigestHash(d)
	reject := &rejectPublicationMemoryPG{PostgresStore: st}
	failing := NewService(reject, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(reject), st)
	before, err := st.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	_, err = failing.ReuseMemoryDigest(ctx, repo, rollbackTarget, inbound.MemoryReuse{Version: 1, BaseHash: base, MemoryHash: hash, Provider: d.Provider})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("injected failure: %v", err)
	}
	if _, err := st.GetMemory(ctx, repo, hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("blob escaped rollback: %v", err)
	}
	after, err := st.RepositoryRevision(ctx, repo)
	if err != nil || before != after {
		t.Fatalf("revision escaped rollback: %v", err)
	}
}
