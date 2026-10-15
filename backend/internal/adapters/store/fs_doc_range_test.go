package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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

func assertRangePages(t *testing.T, ctx context.Context, storage outbound.DocRangeStore, repo domain.ContentHash, fixture rootReaderFixture) domain.DocRangeIndex {
	t.Helper()
	header, err := storage.DocRangeIndex(ctx, repo, fixture.hash)
	if err != nil {
		t.Fatal(err)
	}
	index, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	want := index.Header()
	if !reflect.DeepEqual(header, want) {
		t.Fatal("streaming and owned-proof headers disagree")
	}
	for position := range header.Pages {
		entries, err := storage.DocRangePage(ctx, repo, header, position)
		if err != nil {
			t.Fatal(err)
		}
		page, err := index.Page(ctx, position)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := want.DecodePage(position, page)
		if err != nil || !reflect.DeepEqual(entries, expected) {
			t.Fatalf("page %d differs: %v", position, err)
		}
	}
	return header
}

func TestDocRangeFSColdRebuildAndWarmSelection(t *testing.T) {
	ctx := context.Background()
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	fixture := rangeFixture(t, 2*domain.DocRangePageEvents+1)
	seedRootFS(t, storage, repo, fixture)
	if err := writeAtomic(storage.readIndexPath(repo, fixture.hash), []byte("not root evidence")); err != nil {
		t.Fatal(err)
	}
	before := rootFSImage(t, storage.dataDir)
	if _, err := storage.DocRangeIndex(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatal("cold header", err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	after := rootFSImage(t, storage.dataDir)
	for path, contents := range before {
		if after[path] != contents {
			t.Fatalf("maintenance changed archive or legacy projection: %s", path)
		}
	}
	storage = NewFSStore(storage.dataDir)
	header := assertRangePages(t, ctx, storage, repo, fixture)
	if err := writeAtomic(storage.chunkPath(repo, fixture.manifest.Chunks[0].Hash), []byte("damaged source")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(storage.rangePagePath(repo, fixture.hash, header.Pages[1]), []byte("damaged unselected page")); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal("warm ensure touched source or pages", err)
	}
	if _, err := storage.DocRangePage(ctx, repo, header, 0); err != nil {
		t.Fatal("selected page touched unrelated data", err)
	}
	if _, err := storage.DocRangePage(ctx, repo, header, 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("selected corruption accepted", err)
	}
	if _, err := storage.ReadVerifiedDoc(ctx, repo, fixture.hash); err == nil {
		t.Fatal("strict reader stopped auditing current source")
	}
}

func TestDocRangeFSRejectsOwnershipAndCorruption(t *testing.T) {
	for _, damage := range []string{"foreign", "root-missing", "root-corrupt", "header-corrupt", "header-manifest", "page-missing", "page-corrupt", "page-oversized", "caller-header"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			storage := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			fixture := rangeFixture(t, 1)
			seedRootFS(t, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			header := assertRangePages(t, ctx, storage, repo, fixture)
			var err error
			want := domain.ErrIntegrity
			switch damage {
			case "foreign":
				repo = domain.HashContent([]byte("foreign"))
				want = domain.ErrNotFound
			case "root-missing":
				err = os.Remove(storage.docPath(repo, fixture.hash))
				want = domain.ErrNotFound
			case "root-corrupt":
				changed := rangeFixture(t, 2)
				err = writeAtomic(storage.docPath(repo, fixture.hash), docCompress(changed.manifestBytes))
			case "header-corrupt":
				err = writeAtomic(storage.rangeIndexPath(repo, fixture.hash), []byte("not JSON"))
			case "header-manifest":
				changed := header
				changed.Manifest.EventCount++
				raw, _ := json.Marshal(changed)
				err = writeAtomic(storage.rangeIndexPath(repo, fixture.hash), raw)
			case "page-missing":
				err = os.Remove(storage.rangePagePath(repo, fixture.hash, header.Pages[0]))
			case "page-corrupt":
				err = writeAtomic(storage.rangePagePath(repo, fixture.hash, header.Pages[0]), []byte("[]"))
			case "page-oversized":
				err = writeAtomic(storage.rangePagePath(repo, fixture.hash, header.Pages[0]), []byte(strings.Repeat(" ", domain.MaxDocRangePageBytes+1)))
			case "caller-header":
				header.Pages[0] = domain.HashContent([]byte("forged page"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.DocRangePage(ctx, repo, header, 0); !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
		})
	}
}

func TestDocRangeFSColdFailureDoesNotPublish(t *testing.T) {
	for _, damage := range []string{"source-corrupt", "source-missing", "canceled", "readonly", "page-write"} {
		t.Run(damage, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			ctx := context.Background()
			storage := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			fixture := rangeFixture(t, 1)
			seedRootFS(t, storage, repo, fixture)
			var err error
			switch damage {
			case "source-corrupt":
				err = writeAtomic(storage.chunkPath(repo, fixture.manifest.Chunks[0].Hash), []byte("corrupt"))
			case "source-missing":
				err = os.Remove(storage.chunkPath(repo, fixture.manifest.Chunks[0].Hash))
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "readonly":
				ctx = outbound.WithDocReadOnly(ctx)
			case "page-write":
				index, buildErr := domain.BuildDocRangeIndex(ctx, rootPublicationProof(t, fixture))
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				err = os.MkdirAll(storage.rangePagePath(repo, fixture.hash, index.Header().Pages[0]), 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err == nil {
				t.Fatal("invalid cold rebuild succeeded")
			}
			if exists(storage.rangeIndexPath(repo, fixture.hash)) {
				t.Fatal("failed rebuild published a header")
			}
			assertRangeSpoolsRemoved(t, spoolDirectory)
		})
	}
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
			assertRangePages(t, ctx, storage, job.RepoID, fixture)
			if exists(storage.readIndexPath(job.RepoID, fixture.hash)) {
				t.Fatal("root publication created legacy index")
			}
			if err := os.RemoveAll(filepath.Dir(storage.rangeIndexPath(job.RepoID, fixture.hash))); err != nil {
				t.Fatal(err)
			}
			if err := NewFSStore(storage.dataDir).EnsureDocRangeIndex(ctx, job.RepoID, fixture.hash); err != nil {
				t.Fatal("cold rebuild after publication", err)
			}
		})
	}
}

func TestDocRangeFSDeleteDocRemovesOnlyOwnedProjection(t *testing.T) {
	ctx := context.Background()
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	foreign := domain.HashContent([]byte(t.Name() + "/foreign"))
	fixture := rangeFixture(t, 2*domain.DocRangePageEvents+1)
	other := rangeFixture(t, 1)
	for _, owner := range []domain.ContentHash{repo, foreign} {
		seedRootFS(t, storage, owner, fixture)
		if err := storage.EnsureDocRangeIndex(ctx, owner, fixture.hash); err != nil {
			t.Fatal(err)
		}
	}
	seedRootFS(t, storage, repo, other)
	if err := storage.EnsureDocRangeIndex(ctx, repo, other.hash); err != nil {
		t.Fatal(err)
	}
	header := assertRangePages(t, ctx, storage, repo, fixture)
	directory := filepath.Dir(storage.rangeIndexPath(repo, fixture.hash))
	before := rootFSImage(t, storage.dataDir)
	for attempt := 0; attempt < 2; attempt++ {
		if err := storage.DeleteDoc(ctx, repo, fixture.hash); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Fatal("range projection survived deletion", err)
		}
		if _, err := os.Stat(storage.docPath(repo, fixture.hash)); !os.IsNotExist(err) {
			t.Fatal("document survived deletion", err)
		}
	}
	after := rootFSImage(t, storage.dataDir)
	for path, contents := range before {
		if path == storage.docPath(repo, fixture.hash) || strings.HasPrefix(path, directory+string(os.PathSeparator)) {
			continue
		}
		if after[path] != contents {
			t.Fatalf("deletion changed another document, repository or retained chunk: %s", path)
		}
	}
	assertRangePages(t, ctx, storage, foreign, fixture)
	assertRangePages(t, ctx, storage, repo, other)
	if err := writeAtomic(storage.rangePagePath(repo, fixture.hash, header.Pages[0]), []byte("orphaned partial projection")); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteDoc(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("orphaned pages survived idempotent deletion", err)
	}
}

func TestDocRangeFSManyShortEventsPublicationAndBackfill(t *testing.T) {
	spoolDirectory := rangeSpoolTempDir(t)
	ctx := rootPublicationContext(context.Background())
	storage := NewFSStore(t.TempDir())
	fixture := manyShortRangeFixture(t)
	job, proof := rootPublicationClaimFS(t, storage, fixture)
	publication, err := storage.PrepareDocJob(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal("many-short-event publication failed", err)
	}
	header, err := storage.DocRangeIndex(ctx, job.RepoID, fixture.hash)
	if err != nil {
		t.Fatal(err)
	}
	var pageBytes int64
	for _, hash := range header.Pages {
		info, err := os.Stat(storage.rangePagePath(job.RepoID, fixture.hash, hash))
		if err != nil {
			t.Fatal(err)
		}
		pageBytes += info.Size()
	}
	if pageBytes <= domain.MaxDocRangeIndexBytes {
		t.Fatal("fixture did not exceed old aggregate cap", pageBytes)
	}
	done, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil || done.State != "completed" {
		t.Fatal("publication lost completion", err)
	}
	if err := os.RemoveAll(filepath.Dir(storage.rangeIndexPath(job.RepoID, fixture.hash))); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, job.RepoID, fixture.hash); err != nil {
		t.Fatal("many-short-event backfill failed", err)
	}
	rebuilt, err := storage.DocRangeIndex(ctx, job.RepoID, fixture.hash)
	if err != nil || !reflect.DeepEqual(rebuilt, header) {
		t.Fatal("backfill changed verified header", err)
	}
	for _, position := range []int{0, len(header.Pages) / 2, len(header.Pages) - 1} {
		if _, err := storage.DocRangePage(ctx, job.RepoID, header, position); err != nil {
			t.Fatal("selected rebuilt page", position, err)
		}
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
}
