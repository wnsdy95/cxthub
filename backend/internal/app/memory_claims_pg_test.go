//go:build postgres

package app

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGTypedMemoryReplicaRoundTripAndCAS(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := systemTestContext()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	first := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	second := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	repo := hh(t.Name() + time.Now().String())
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: string(repo)}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	id := doc.Hash
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	d := domain.MemoryDigest{SnapshotID: id, ClaimsVersion: 1, Summary: strings.Repeat("historical text ", 10000), Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "Keep completed history."}}}}}
	root, err := first.PutMemoryDigestCAS(ctx, repo, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.GetMemoryObject(ctx, repo, root)
	gotHash, _ := domain.MemoryDigestHash(got)
	if err != nil || gotHash != root || !got.HasMemoryClaims() || got.ClaimsVersion != 1 {
		t.Fatalf("replica roundtrip=%+v err=%v", got, err)
	}
	legacy := domain.MemoryDigest{SnapshotID: id, PreviousMemoryHash: root, Summary: "downgrade"}
	if _, err := second.PutMemoryDigestCAS(ctx, repo, legacy); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("downgrade=%v", err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for i, svc := range []*Service{first, second} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			next := d
			next.PreviousMemoryHash = root
			next.Summary += []string{"left", "right"}[i]
			_, e := svc.PutMemoryDigestCAS(ctx, repo, next)
			out <- e
		}(i, svc)
	}
	wg.Wait()
	close(out)
	accepted, conflicted := 0, 0
	for e := range out {
		if e == nil {
			accepted++
		} else if errors.Is(e, domain.ErrConflict) {
			conflicted++
		} else {
			t.Fatal(e)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("accepted=%d conflicts=%d", accepted, conflicted)
	}
	final, err := second.GetMemoryDigest(ctx, repo, id)
	if err != nil || !final.HasMemoryClaims() || final.ClaimsVersion != 1 || final.PreviousMemoryHash != root {
		t.Fatalf("final=%+v err=%v", final, err)
	}
}
