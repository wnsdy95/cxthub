package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rangeFixture(t *testing.T, count int) rootReaderFixture {
	t.Helper()
	fixture := rootFixture(t, "")
	fixture.cir.Envelope.SessionOriginID += time.Now().UTC().Format(time.RFC3339Nano)
	for position := 0; position < count; position++ {
		fixture.cir.Events = append(fixture.cir.Events, domain.CIREvent{
			Kind: domain.EventMessage, Seq: position + 1, Role: "user",
			Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s range event %d \uD55C", fixture.cir.Envelope.SessionOriginID, position)}},
		})
	}
	var err error
	fixture.manifest, fixture.bodies, err = domain.ConversationManifestForCIR(fixture.cir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.canonical, err = domain.CanonicalBytes(fixture.cir)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestDocRangeFSPublication(t *testing.T) {
	for _, mode := range []string{"empty", "pages", "corrupt", "expired", "page-write"} {
		t.Run(mode, func(t *testing.T) {
			ctx := rootPublicationContext(context.Background())
			storage := NewFSStore(t.TempDir())
			count := 2*domain.DocRangePageEvents + 1
			if mode == "empty" {
				count = 0
			}
			fixture := rangeFixture(t, count)
			job, doc := rootPublicationClaimFS(t, storage, fixture)
			publication, err := storage.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			if exists(storage.rangeIndexPath(job.RepoID, fixture.hash)) {
				t.Fatal("preparation published index")
			}
			switch mode {
			case "corrupt":
				err = writeAtomic(storage.chunkPath(job.RepoID, fixture.manifest.Chunks[0].Hash), []byte("corrupt"))
			case "expired":
				old := job
				old.LeaseUntil = time.Now().Add(-time.Second)
				err = storage.writeDocJob(old)
			case "page-write":
				index, buildErr := domain.BuildDocRangeIndex(ctx, doc)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				err = os.MkdirAll(storage.rangePagePath(job.RepoID, fixture.hash, index.Header().Pages[0]), 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = publication.Complete(ctx, job, time.Now())
			if mode != "empty" && mode != "pages" {
				if err == nil || exists(storage.rangeIndexPath(job.RepoID, fixture.hash)) || exists(storage.docPath(job.RepoID, fixture.hash)) {
					t.Fatal("failed publication exposed index or root", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if exists(storage.readIndexPath(job.RepoID, fixture.hash)) {
				t.Fatal("root publication created legacy index")
			}
			if err := os.RemoveAll(filepath.Dir(storage.rangeIndexPath(job.RepoID, fixture.hash))); err != nil {
				t.Fatal(err)
			}
		})
	}
}
