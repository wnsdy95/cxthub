package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docJobTestStore interface {
	outbound.DocJobStore
	outbound.BlobStore
	outbound.MetadataStore
}

func docJobFixture(t *testing.T, repo domain.ContentHash, body string) (domain.DocFinalizationJob, domain.VerifiedSessionDoc, domain.DocChunkPlan) {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: body}}}}}
	cb, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(cb)
	plan, ok := domain.PlanDocChunks(cb)
	if !ok {
		t.Fatal("no chunks")
	}
	doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: hash, CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	j, err := domain.NewDocFinalizationJob(repo, hash, plan.Manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return j, doc, plan
}
func checkDocJobs(t *testing.T, st docJobTestStore) {
	t.Helper()
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	j, doc, plan := docJobFixture(t, repo, "durable context")
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := st.EnqueueDocJob(ctx, j)
		if err != nil || got.ID != j.ID || got.Attempts != 0 {
			t.Fatalf("enqueue %+v %v", got, err)
		}
	}
	if have, _ := st.HasDocs(ctx, repo, []domain.ContentHash{doc.Hash()}); len(have) != 0 {
		t.Fatal("acceptance published body")
	}
	other, _, _ := docJobFixture(t, repo, "queued second")
	if _, err := st.EnqueueDocJob(ctx, other); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan domain.DocFinalizationJob, 8)
	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := st.ClaimDocJob(ctx, repo, now, time.Minute)
			if err == nil {
				claims <- got
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	close(claims)
	var first domain.DocFinalizationJob
	count := 0
	for c := range claims {
		first = c
		count++
	}
	if count != 1 || first.ID != j.ID {
		t.Fatalf("claims %d first %s", count, first.ID)
	}
	if err := st.RenewDocJob(ctx, first, now, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimDocJob(ctx, repo, now.Add(2*time.Minute), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("renewed lease was stolen: %v", err)
	}
	recovered, err := st.ClaimDocJob(ctx, repo, now.Add(4*time.Minute), time.Minute)
	if err != nil || recovered.Version != first.Version+1 {
		t.Fatalf("recover %+v %v", recovered, err)
	}
	if err := st.RenewDocJob(ctx, first, now.Add(2*time.Minute), time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale renewal %v", err)
	}
	if err := st.CompleteDocJob(ctx, first, doc, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale publication %v", err)
	}
	if have, _ := st.HasDocs(ctx, repo, []domain.ContentHash{doc.Hash()}); len(have) != 0 {
		t.Fatal("stale worker published")
	}
	if err := st.CompleteDocJob(ctx, recovered, doc, now); err != nil {
		t.Fatal(err)
	}
	replay, err := st.EnqueueDocJob(ctx, j)
	if err != nil || replay.State != "completed" || replay.Version != recovered.Version {
		t.Fatalf("completion replay %+v %v", replay, err)
	}
	if _, err := st.GetDocJob(ctx, domain.HashContent([]byte("foreign")), j.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("tenant leak %v", err)
	}
	if _, err := st.GetDoc(ctx, repo, doc.Hash()); err != nil {
		t.Fatal(err)
	}
	if snaps, err := st.ListSnapshots(ctx, repo, ""); err != nil || len(snaps) != 0 {
		t.Fatalf("document job created snapshots: %v %v", snaps, err)
	}
	if err := st.DeleteDoc(ctx, repo, doc.Hash()); err != nil {
		t.Fatal(err)
	}
	replay, err = st.EnqueueDocJob(ctx, j)
	if err != nil || replay.State != "waiting" || replay.Version <= recovered.Version {
		t.Fatalf("collected doc not requeued: %+v %v", replay, err)
	}
}
func TestFSDocJobLeasesAndPublication(t *testing.T) { checkDocJobs(t, NewFSStore(t.TempDir())) }
func TestFSDocJobRestartAndGC(t *testing.T) {
	dir := t.TempDir()
	s := NewFSStore(dir)
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	j, doc, plan := docJobFixture(t, repo, strings.Repeat("saved ", 200))
	if _, _, err := s.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	for _, h := range plan.Order {
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(s.chunkPath(repo, h), old, old); err != nil {
			t.Fatal(err)
		}
	}
	restarted := NewFSStore(dir)
	if _, _, err := restarted.repackRepo(repo); err != nil {
		t.Fatal(err)
	}
	for _, h := range plan.Order {
		if _, err := restarted.GetChunk(ctx, repo, h); err != nil {
			t.Fatalf("GC deleted queued chunk: %v", err)
		}
	}
	claim, err := restarted.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.CompleteDocJob(ctx, claim, doc, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
