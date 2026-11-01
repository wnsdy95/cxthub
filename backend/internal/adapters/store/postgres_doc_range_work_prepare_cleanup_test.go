//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type metadataSpoolCancellation struct {
	context.Context
	directory string
	prefix    string
	cancel    context.CancelFunc
	flushed   atomic.Int64
}

func (observed *metadataSpoolCancellation) Err() error {
	if err := observed.Context.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(observed.directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		prefix := observed.prefix
		if prefix == "" {
			prefix = "cxthub-doc-range-"
		}
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 0 && observed.flushed.CompareAndSwap(0, info.Size()) {
			observed.cancel()
		}
	}
	return observed.Context.Err()
}

func TestDocSpoolWorkPGDirectCompatibilityCancellationAfterPartialWriteClosesBothSpools(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docSpoolWorkPGStore(test, storage, ctx)
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	observed := &metadataSpoolCancellation{Context: base, directory: directory, prefix: "cxthub-legacy-doc-", cancel: cancel}
	work := docRangeDirectSpoolWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, work)
	if observed.flushed.Load() != 0 {
		test.Fatal("local-spool setup reached the write cancellation injection before its first local step")
	}
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	if complete, err := work.Step(observed, job); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, context.Canceled) ||
		observed.flushed.Load() != domain.ConversationManifestChunkBytes {
		test.Fatal("first local spool quantum did not cancel after exactly one partial write", complete, err, observed.flushed.Load())
	}
	if trace.privateWrites.Load() != 0 || trace.sourceReads.Load() != 1 ||
		legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("canceled local write attempted private copying or changed persisted state")
	}
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	if remaining, err := os.ReadDir(directory); err != nil || len(remaining) != 0 {
		test.Fatal("closing canceled local work leaked a data or metadata spool", remaining, err)
	}
}

func TestLegacyMetadataPreparationCancellationRemovesPartialSpools(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2"}}
	for ordinal := 0; ordinal < 4097; ordinal++ {
		document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleUser,
			Seq: ordinal, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("metadata cleanup event %d", ordinal)}}})
	}
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	storage.ConfigureLegacyByteRangePublication(true)
	created := time.Now().UTC()
	if _, err := storage.EnqueueDocByteRangeJob(ctx, repo, fixture.hash, created); err != nil {
		test.Fatal(err)
	}
	job, err := storage.ClaimDocRangeJob(ctx, created, time.Minute)
	if err != nil || job.RepoID != repo || job.DocHash != fixture.hash || job.State != "running" {
		test.Fatal("claim metadata cleanup job", job, err)
	}
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	directory := rangeSpoolTempDir(test)
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	observed := &metadataSpoolCancellation{Context: base, directory: directory, cancel: cancel}
	prepared, err := storage.prepareLegacyByteWork(observed, job, session)
	if prepared != nil {
		defer prepared.Close()
	}
	if prepared != nil || !errors.Is(err, context.Canceled) || observed.flushed.Load() == 0 {
		test.Fatalf("partial metadata preparation did not cancel privately: returned=%t flushed=%d error=%v", prepared != nil, observed.flushed.Load(), err)
	}
	remaining, err := os.ReadDir(directory)
	if err != nil || len(remaining) != 0 {
		test.Fatalf("partial preparation leaked data or metadata spools: files=%v error=%v", remaining, err)
	}
	if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("canceled metadata preparation changed public state")
	}
}
