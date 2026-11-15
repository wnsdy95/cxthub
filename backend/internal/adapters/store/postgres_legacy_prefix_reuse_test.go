//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// growingLegacyDocs returns two captures of one session: the second repeats
// every event of the first and adds more, under a different envelope, as
// retained-history captures do. Event text is unique per test so damaging a
// shared chunk row cannot affect any other test.
func growingLegacyDocs(t *testing.T, repo domain.ContentHash) (first, second legacyGrowthDoc) {
	t.Helper()
	unique := t.Name() + time.Now().String()
	var events []domain.CIREvent
	build := func(count, compactions int) legacyGrowthDoc {
		for len(events) < count {
			seq := len(events)
			role := domain.RoleUser
			if seq%2 == 1 {
				role = domain.RoleAssistant
			}
			events = append(events, domain.CIREvent{Kind: domain.EventMessage, Seq: seq, Role: role,
				Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s event %d ", unique, seq) + strings.Repeat("prefix reuse ", 600)}}})
		}
		cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", CompactionCount: compactions}, Events: append([]domain.CIREvent(nil), events...)}
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		hash := domain.HashContent(raw)
		plan, ok := domain.PlanDocChunks(raw)
		if !ok {
			t.Fatal("no chunk plan")
		}
		doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: hash, CIR: cir})
		if err != nil {
			t.Fatal(err)
		}
		job, err := domain.NewDocFinalizationJob(repo, hash, plan.Manifest, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return legacyGrowthDoc{job: job, doc: doc, plan: plan}
	}
	first, second = build(300, 1), build(360, 2)
	if len(first.plan.Manifest.Chunks) < 4 {
		t.Fatal("fixture too small", len(first.plan.Manifest.Chunks))
	}
	return first, second
}

type legacyGrowthDoc struct {
	job  domain.DocFinalizationJob
	doc  domain.VerifiedSessionDoc
	plan domain.DocChunkPlan
}

func claimLegacyGrowthPG(t *testing.T, storage *PostgresStore, ctx context.Context, doc legacyGrowthDoc) domain.DocFinalizationJob {
	t.Helper()
	if _, _, err := storage.PutChunks(ctx, doc.job.RepoID, doc.plan.Bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(ctx, doc.job); err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimDocJob(ctx, doc.job.RepoID, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ID != doc.job.ID {
		t.Fatal("claimed another job", claim.ID)
	}
	return claim
}

func finalizeLegacyGrowthPG(t *testing.T, storage *PostgresStore, ctx context.Context, doc legacyGrowthDoc) *pgLegacyStreamingPublication {
	t.Helper()
	job := claimLegacyGrowthPG(t, storage, ctx, doc)
	publication := prepareLegacyJobPG(t, storage, ctx, job)
	if err := publication.Stage(ctx, job, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal(err)
	}
	return publication
}

func assertLegacyGrowthPublishedPG(t *testing.T, storage *PostgresStore, ctx context.Context, doc legacyGrowthDoc) {
	t.Helper()
	actual, err := storage.GetDoc(ctx, doc.job.RepoID, doc.job.DocHash)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := domain.CanonicalBytes(actual.CIR)
	if err != nil || !bytes.Equal(canonical, doc.doc.Bytes()) {
		t.Fatal("published document changed", err)
	}
	index, err := storage.DocReadIndex(ctx, doc.job.RepoID, doc.job.DocHash)
	want, _ := doc.doc.ReadIndex()
	if err != nil || len(index.Events) != len(want.Events) {
		t.Fatal("index not published", err)
	}
	for ordinal, event := range index.Events {
		want.Events[ordinal].Text = ""
		if event != want.Events[ordinal] {
			t.Fatal("projection changed", ordinal, event, want.Events[ordinal])
		}
	}
}

func TestLegacyPrefixReusePGSkipsRepeatedPrefix(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	first, second := growingLegacyDocs(t, repo)
	if base := finalizeLegacyGrowthPG(t, storage, ctx, first); base.reuse.chunks != 0 {
		t.Fatal("first capture had no base", base.reuse)
	}
	assertLegacyGrowthPublishedPG(t, storage, ctx, first)

	job := claimLegacyGrowthPG(t, storage, ctx, second)
	publication := prepareLegacyJobPG(t, storage, ctx, job)
	shared := len(first.plan.Manifest.Chunks) - 1
	if publication.reuse.base != first.job.DocHash || publication.reuse.chunks != shared {
		t.Fatal("prefix not reused", publication.reuse, shared)
	}
	suffix := 0
	for _, hash := range second.job.Manifest.Chunks[shared:] {
		suffix += len(second.plan.Bodies[hash])
	}
	if publication.spool.size != suffix {
		t.Fatal("reused prefix was spooled", publication.spool.size, suffix)
	}
	// The read block that holds the reuse boundary is new, so staging reads
	// events on both sides of it.
	if publication.source.body != nil {
		t.Fatal("prefix read before staging")
	}
	if err := publication.Stage(ctx, job, time.Minute); err != nil {
		t.Fatal(err)
	}
	if publication.source.body == nil || publication.source.last.offset+publication.source.last.length != publication.source.skip {
		t.Fatal("boundary block did not read the reused chunk before the boundary", publication.source.last)
	}
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertLegacyGrowthPublishedPG(t, storage, ctx, second)
	if base, ok := storage.legacyBases.get(repo); !ok || base != second.job.DocHash {
		t.Fatal("completion did not become the next base", base, ok)
	}
}

func TestLegacyPrefixReusePGFallsBack(t *testing.T) {
	for _, change := range []string{"no-hint", "foreign-hint", "base-revoked-before-complete"} {
		t.Run(change, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			first, second := growingLegacyDocs(t, repo)
			finalizeLegacyGrowthPG(t, storage, ctx, first)
			switch change {
			case "no-hint":
				// A restarted process has no hint.
				storage.legacyBases = legacyBaseHints{}
			case "foreign-hint":
				storage.legacyBases.put(repo, domain.HashContent([]byte("not owned "+t.Name())))
			}
			job := claimLegacyGrowthPG(t, storage, ctx, second)
			publication := prepareLegacyJobPG(t, storage, ctx, job)
			wantReuse := change == "base-revoked-before-complete"
			if (publication.reuse.chunks > 0) != wantReuse {
				t.Fatal("unexpected reuse", publication.reuse)
			}
			if err := publication.Stage(ctx, job, time.Minute); err != nil {
				t.Fatal(err)
			}
			if change == "base-revoked-before-complete" {
				if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, first.job.DocHash); err != nil {
					t.Fatal(err)
				}
			}
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				t.Fatal(err)
			}
			assertLegacyGrowthPublishedPG(t, storage, ctx, second)
		})
	}
}

func TestLegacyPrefixReusePGStillRefuses(t *testing.T) {
	for _, change := range []string{"prefix-damaged-before-prepare", "prefix-revoked-before-complete", "suffix-damaged-before-complete"} {
		t.Run(change, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			first, second := growingLegacyDocs(t, repo)
			finalizeLegacyGrowthPG(t, storage, ctx, first)
			job := claimLegacyGrowthPG(t, storage, ctx, second)
			prefixChunk := second.job.Manifest.Chunks[1]
			suffixChunk := second.job.Manifest.Chunks[len(second.job.Manifest.Chunks)-1]
			if change == "prefix-damaged-before-prepare" {
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, prefixChunk, []byte("damaged")); err != nil {
					t.Fatal(err)
				}
				if _, err := storage.PrepareLegacyDocJob(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("damaged reused chunk was accepted", err)
				}
				assertDocNotPublishedPG(t, storage, ctx, job)
				return
			}
			publication := prepareLegacyJobPG(t, storage, ctx, job)
			if publication.reuse.chunks == 0 {
				t.Fatal("prefix not reused")
			}
			if err := publication.Stage(ctx, job, time.Minute); err != nil {
				t.Fatal(err)
			}
			want := domain.ErrIntegrity
			switch change {
			case "prefix-revoked-before-complete":
				if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, prefixChunk); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrNotFound
			case "suffix-damaged-before-complete":
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, suffixChunk, []byte("damaged")); err != nil {
					t.Fatal(err)
				}
			}
			if err := publication.Complete(ctx, job, time.Now()); !errors.Is(err, want) {
				t.Fatalf("completion error %v, want %v", err, want)
			}
			assertDocNotPublishedPG(t, storage, ctx, job)
		})
	}
}

func TestLegacyPrefixSourceReadsAcrossReuse(t *testing.T) {
	spool, err := newLegacyDocSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	chunks := [][]byte{[]byte("alpha-"), []byte("bravo-"), []byte("charlie-"), []byte("delta")}
	stored := map[domain.ContentHash][]byte{}
	loads := 0
	source := &legacyPrefixSource{spool: spool, load: func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		loads++
		return stored[hash], nil
	}}
	ctx := context.Background()
	var whole []byte
	for ordinal, body := range chunks {
		hash := domain.HashContent(body)
		stored[hash] = body
		if ordinal < 2 {
			source.reuse(hash, len(body))
		}
		for _, part := range [][]byte{body[:2], body[2:]} {
			if err := source.Append(ctx, part); err != nil {
				t.Fatal(err)
			}
		}
		whole = append(whole, body...)
	}
	if spool.size != len("charlie-delta") || source.size != len(whole) {
		t.Fatal("reused bytes were spooled", spool.size, source.size)
	}
	for offset := 0; offset <= len(whole); offset++ {
		for end := offset; end <= len(whole); end++ {
			got, err := source.Read(ctx, offset, end-offset)
			if err != nil || !bytes.Equal(got, whole[offset:end]) {
				t.Fatalf("read [%d,%d) = %q, %v", offset, end, got, err)
			}
		}
	}
	if _, err := source.Read(ctx, 1, len(whole)); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("read past the end", err)
	}
	stored[domain.HashContent(chunks[0])] = []byte("ALPHA-")
	source.body = nil
	if _, err := source.Read(ctx, 0, 3); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("changed stored chunk was served", err)
	}
	if loads == 0 {
		t.Fatal("reused range never loaded")
	}
}
