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
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2"}}
	for ordinal := 0; ordinal < 4097; ordinal++ {
		document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleUser,
			Seq: ordinal, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("metadata cleanup event %d", ordinal)}}})
	}
	raw, err := domain.CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	hash := domain.HashContent(raw)
	proof, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: hash, CIR: document})
	if err != nil {
		test.Fatal(err)
	}
	plan, err := domain.NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	key, err := plan.Key()
	if err != nil {
		test.Fatal(err)
	}
	spec, err := plan.Spec()
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := spec.Encode()
	if err != nil {
		test.Fatal(err)
	}
	created := time.Now().UTC()
	job := domain.DocRangeIndexJob{RepoID: domain.HashContent([]byte(test.Name())), DocHash: hash, State: "running",
		Version: 1, CreatedAt: created, NextAttempt: created, LeaseUntil: created.Add(time.Minute)}
	if err := job.Validate(); err != nil {
		test.Fatal(err)
	}
	session := legacyByteBackfillSession{repo: job.RepoID, hash: hash, proof: proof, plan: plan, encoded: encoded,
		stage: legacyByteStage{plan: key, blocks: spec.Blocks}}
	directory := rangeSpoolTempDir(test)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &metadataSpoolCancellation{Context: base, directory: directory, cancel: cancel}
	prepared, err := (&PostgresStore{}).prepareLegacyByteWork(observed, job, session)
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
}
