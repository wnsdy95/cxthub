package documentworker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ChunkReader interface {
	GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
}

type Finalizer struct {
	storage  outbound.DocJobStore
	chunks   ChunkReader
	verifier *domain.CanonicalDocVerifier
	scope    Scope
}

func NewFinalizer(storage outbound.DocJobStore, chunks ChunkReader, verifier *domain.CanonicalDocVerifier, scope Scope) (*Finalizer, error) {
	if verifier == nil {
		return nil, fmt.Errorf("%w: document finalization verifier is required", domain.ErrValidation)
	}
	if absent(scope) {
		return nil, fmt.Errorf("%w: document finalization scope is required", domain.ErrValidation)
	}
	return &Finalizer{storage: storage, chunks: chunks, verifier: verifier, scope: scope}, nil
}

func (finalizer *Finalizer) Verify(ctx context.Context, job domain.DocFinalizationJob) (domain.VerifiedSessionDoc, error) {
	if err := job.Validate(); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	representation, err := job.Representation()
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if representation.Identity == domain.DocumentIdentityRootV1 {
		manifest, err := representation.ConversationManifest()
		if err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		reader, ok := finalizer.chunks.(outbound.ConversationChunkReader)
		if !ok {
			return domain.VerifiedSessionDoc{}, domain.ErrUnsupportedDocumentIdentity
		}
		lengths := make(map[domain.ContentHash]int64, len(manifest.Chunks))
		for _, chunk := range manifest.Chunks {
			if old, seen := lengths[chunk.Hash]; seen && old != chunk.Bytes {
				return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
			}
			lengths[chunk.Hash] = chunk.Bytes
		}
		return finalizer.verifier.VerifyConversationManifestDoc(ctx, job.DocHash, manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
			return reader.ReadConversationChunk(ctx, job.RepoID, hash, lengths[hash])
		})
	}
	return finalizer.verifier.VerifyChunks(ctx, job.DocHash, job.Manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return finalizer.chunks.GetChunk(ctx, job.RepoID, hash)
	})
}

func (finalizer *Finalizer) RunJob(ctx context.Context, job domain.DocFinalizationJob) (runErr error) {
	started := time.Now()
	log.Printf("document finalization started: job=%s identity=%s format=%s attempt=%d", job.ID, job.DocIdentity, job.Manifest.Format, job.Attempts)
	defer func() {
		log.Printf("document finalization exited: job=%s elapsed=%s", job.ID, time.Since(started).Round(time.Millisecond))
	}()
	work, cancel := context.WithTimeout(ctx, WorkLimit)
	defer cancel()
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	defer stop()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-work.Done():
				return
			case <-ticker.C:
				renew, finish := context.WithTimeout(work, 5*time.Second)
				err := finalizer.storage.RenewDocJob(renew, job, time.Now().UTC(), Lease)
				finish()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var publication outbound.PreparedDocPublication
	var err error
	if streaming, ok := finalizer.storage.(outbound.StreamingRootJobStore); ok && job.DocIdentity == domain.DocumentIdentityRootV1 {
		var prepared outbound.PreparedRootDocPublication
		err = finalizer.scope.WithinRepositoryRead(work, job.RepoID, func(read context.Context) error {
			var prepareErr error
			prepared, prepareErr = streaming.PrepareRootDocJob(read, job)
			return prepareErr
		})
		if prepared != nil {
			defer func() { runErr = errors.Join(runErr, prepared.Close()) }()
		}
		publication = prepared
	} else if streaming, ok := finalizer.storage.(outbound.StreamingLegacyJobStore); ok && job.DocIdentity == domain.DocumentIdentityLegacy && job.Manifest.Format == domain.ChunkFormatV2 {
		var prepared outbound.PreparedRootDocPublication
		err = finalizer.scope.WithinRepositoryRead(work, job.RepoID, func(read context.Context) error {
			var prepareErr error
			prepared, prepareErr = streaming.PrepareLegacyDocJob(read, job)
			return prepareErr
		})
		if prepared != nil {
			defer func() { runErr = errors.Join(runErr, prepared.Close()) }()
		}
		publication = prepared
	} else {
		var verified domain.VerifiedSessionDoc
		err = finalizer.scope.WithinRepositoryRead(work, job.RepoID, func(read context.Context) error {
			var verifyErr error
			verified, verifyErr = finalizer.Verify(read, job)
			return verifyErr
		})
		if err == nil {
			publication, err = finalizer.storage.PrepareDocJob(work, verified)
		}
	}
	if err == nil && job.DocIdentity == domain.DocumentIdentityRootV1 {
		err = finalizer.YieldRootPreparation(work, publication)
	}
	stop()
	<-renewed
	if err == nil {
		err = work.Err()
	}
	if staged, ok := publication.(outbound.StagedDocPublication); err == nil && ok {
		err = staged.Stage(work, job, Lease)
	}
	if err == nil {
		err = publication.Complete(work, job, time.Now().UTC())
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	now := time.Now().UTC()
	job.State = "retrying"
	job.Reason = "temporary_failure"
	job.UpdatedAt = now
	job.LeaseUntil = time.Time{}
	job.NextAttempt = now.Add(time.Second << min(job.Attempts, 8))
	if errors.Is(err, domain.ErrDocumentResourceLimit) {
		job.Reason = "resource_limit"
	}
	if errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrUnsupportedCIRVersion) || errors.Is(err, domain.ErrConversationManifest) || errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		job.State = "rejected"
		job.Reason = "invalid_document"
	}
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelFinish()
	return errors.Join(err, finalizer.storage.FinishDocJob(finish, job, now))
}

func (finalizer *Finalizer) YieldRootPreparation(ctx context.Context, publication outbound.PreparedDocPublication) error {
	if !finalizer.scope.CanYield(ctx) {
		return nil
	}
	bounded, ok := publication.(outbound.BoundedRootDocPublication)
	if !ok {
		return nil
	}
	resources, err := bounded.RetainedResources()
	if err != nil {
		return err
	}
	return finalizer.scope.Yield(ctx, resources)
}

func (finalizer *Finalizer) Process(ctx context.Context, limit int) error {
	ctx = finalizer.scope.WorkerContext(ctx)
	if finalizer.storage == nil {
		return fmt.Errorf("durable document finalization unavailable")
	}
	for processed := 0; processed < limit; processed++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed := false
		err := finalizer.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := func() (domain.DocFinalizationJob, error) {
				claim, cancel := context.WithTimeout(bound, 5*time.Second)
				defer cancel()
				return finalizer.storage.ClaimDocJob(claim, "", time.Now().UTC(), Lease)
			}()
			if err != nil {
				return err
			}
			claimed = true
			return finalizer.RunJob(bound, job)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (finalizer *Finalizer) Run(ctx context.Context) {
	for {
		if err := finalizer.Process(ctx, 1); err != nil && ctx.Err() == nil {
			log.Printf("document finalization: %v", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
