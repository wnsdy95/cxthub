//go:build postgres

package store

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGChunkBackedPublicationAndDuplicateIntegrity(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	doc, legacy := verifiedChunkFixture(t)
	// PutVerifiedDoc supports both already uploaded and newly supplied chunks.
	if _, err := st.PutVerifiedDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if created, err := st.PutVerifiedDoc(ctx, repo, doc); err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	got, err := st.GetDoc(ctx, repo, doc.Hash())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.ValidatedSessionDocBytes(got)
	if err != nil || !bytes.Equal(raw, legacy.Bytes()) {
		t.Fatal("archived bytes changed", err)
	}
	wantIndex, err := legacy.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	gotIndex, err := st.DocReadIndex(ctx, repo, doc.Hash())
	if err != nil {
		t.Fatal(err)
	}
	needle := strings.ToLower(wantIndex.Events[0].Text[:32])
	for i := range wantIndex.Events {
		wantIndex.Events[i].Text = "" // Search text has a separate query path.
	}
	if !reflect.DeepEqual(gotIndex, wantIndex) {
		t.Fatal("chunk-backed publication changed read metadata")
	}
	hits, err := st.SearchDocEvents(ctx, repo, doc.Hash(), needle, -1, 10)
	if err != nil || len(hits) != 1 || hits[0].Index != 0 {
		t.Fatal("chunk-backed publication changed search results", err)
	}
	// Matching the manifest is not enough: corruption after preparation must
	// still fail on a duplicate document before granting a second owner access.
	foreign := domain.HashContent([]byte(string(repo) + " another owner"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
		t.Fatal(err)
	}
	plan, _ := doc.ChunkPlan()
	if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, plan.Order[0], docCompress([]byte("corrupt"))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT(hash) DO UPDATE SET bytes=EXCLUDED.bytes`, plan.Order[0], docCompress(plan.Bodies[plan.Order[0]]))
		_, _ = st.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, repo, plan.Order[0])
	})
	if _, err := st.PutVerifiedDoc(ctx, foreign, doc); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("matching manifest hid corrupt chunk", err)
	}
	if have, err := st.HasDocs(ctx, foreign, []domain.ContentHash{doc.Hash()}); err != nil || len(have) != 0 {
		t.Fatal("failed publication granted ownership", err)
	}
	// Absence in an already published manifest also indicates a broken archive.
	// It must not silently become a repair simply because this caller supplies
	// good bytes. New-document post-verification collection remains reconstructible
	// and is separately covered by TestPGPreparedDocRechecksLeaseAndCurrentChunkBytes.
	if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, plan.Order[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, plan.Order[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutVerifiedDoc(ctx, foreign, doc); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("matching manifest repaired a missing chunk", err)
	}
	var repaired bool
	if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=$1)`, plan.Order[0]).Scan(&repaired); err != nil || repaired {
		t.Fatal("duplicate publication wrote missing archive bytes", err)
	}
	if have, err := st.HasDocs(ctx, foreign, []domain.ContentHash{doc.Hash()}); err != nil || len(have) != 0 {
		t.Fatal("missing archive granted ownership", err)
	}
}
