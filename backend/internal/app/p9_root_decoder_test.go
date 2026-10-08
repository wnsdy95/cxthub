package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Regression from the independently reproduced P3 decoder finding.
func TestP9RootWorkerStoredDecoderBoundary(t *testing.T) {
	for _, kind := range []string{"malformed-zstd", "oversized-zstd"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			st := store.NewFSStore(dir)
			svc := NewService(st, st, nil, nil, st)
			ctx := systemTestContext()
			repo := hh(t.Name())
			bindCommitTestRepo(t, st, repo)
			rep, bodies := rootWorkerFixture(t)
			if _, _, err := st.PutChunks(ctx, repo, bodies); err != nil {
				t.Fatal(err)
			}
			job, err := domain.NewDocFinalizationJobForRepresentation(repo, rep, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.EnqueueDocJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			// Warm semantic proof cannot authorize these later current-byte mutations.
			if _, err := svc.verifyDocJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			manifest, err := rep.ConversationManifest()
			if err != nil {
				t.Fatal(err)
			}
			chunk := manifest.Chunks[0]
			path := filepath.Join(dir, "repos", strings.TrimPrefix(string(repo), "sha256:"), "objects", "chunks", strings.TrimPrefix(string(chunk.Hash), "sha256:"))
			raw := []byte{0x28, 0xb5, 0x2f, 0xfd}
			if kind == "oversized-zstd" {
				enc, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatal(err)
				}
				raw = enc.EncodeAll(bytes.Repeat([]byte("x"), 4<<20), nil)
				enc.Close()
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			loaded, loadErr := st.ReadConversationChunk(ctx, repo, chunk.Hash, chunk.Bytes)
			t.Logf("declared=%d stored=%d decoded=%d load_error=%v integrity=%v", chunk.Bytes, len(raw), len(loaded), loadErr, errors.Is(loadErr, domain.ErrIntegrity))
			if !errors.Is(loadErr, domain.ErrIntegrity) || len(loaded) != 0 {
				t.Fatal("unbounded root load", len(loaded), loadErr)
			}
			err = svc.ProcessDocFinalizations(ctx, 1)
			if err == nil {
				t.Fatal("corruption accepted")
			}
			status, statusErr := svc.GetDocFinalization(ctx, repo, job.ID)
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			t.Logf("worker_error=%v state=%s reason=%s", err, status.State, status.Reason)
			if have, err := st.HasDocs(ctx, repo, []domain.ContentHash{rep.Hash}); err != nil || len(have) != 0 {
				t.Fatal("corrupt root published", err)
			}
			if status.State != "rejected" || status.Reason != "invalid_document" {
				t.Fatalf("permanent corrupt at-rest root chunk should be terminal: state=%s reason=%s", status.State, status.Reason)
			}
		})
	}
}

type p9FailingRootReader struct {
	*store.FSStore
	failure error
}

func (s p9FailingRootReader) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	return nil, s.failure
}

func TestP9RootWorkerLoaderErrorControls(t *testing.T) {
	for _, mode := range []string{"transient", "missing", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			st := store.NewFSStore(t.TempDir())
			svc := NewService(st, st, nil, nil, st)
			ctx := systemTestContext()
			repo := hh(t.Name())
			bindCommitTestRepo(t, st, repo)
			rep, bodies := rootWorkerFixture(t)
			if _, _, err := st.PutChunks(ctx, repo, bodies); err != nil {
				t.Fatal(err)
			}
			job, err := domain.NewDocFinalizationJobForRepresentation(repo, rep, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.EnqueueDocJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			claim, err := st.ClaimDocJob(ctx, repo, time.Now(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("synthetic I/O unavailable")
			if mode == "missing" {
				failure = domain.ErrNotFound
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				failure = context.Canceled
			}
			svc.blobs = p9FailingRootReader{st, failure}
			if err = svc.runDocJob(ctx, st, claim); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			got, err := st.GetDocJob(context.Background(), repo, claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancelled" {
				if got.Version != claim.Version || got.LeaseUntil.IsZero() {
					t.Fatal("cancel consumed claim", got)
				}
			} else if got.State != "retrying" || got.Reason != "temporary_failure" {
				t.Fatal(got)
			}
		})
	}
}
