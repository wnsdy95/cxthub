package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docJobLease = 2 * time.Minute

// This bound is for background work, independent of the 30s HTTP deadline. It
// prevents a pathological body from holding a worker indefinitely.
const docJobWorkLimit = 10 * time.Minute

func (s *Service) docJobs() (outbound.DocJobStore, error) {
	st, ok := s.blobs.(outbound.DocJobStore)
	if !ok {
		return nil, fmt.Errorf("durable document finalization unavailable")
	}
	return st, nil
}
func docJobStatus(j domain.DocFinalizationJob) inbound.DocFinalizationStatus {
	return inbound.DocFinalizationStatus{ID: j.ID, DocHash: j.DocHash, State: j.State, Reason: j.Reason, UpdatedAt: j.UpdatedAt}
}
func (s *Service) SubmitDocFinalization(ctx context.Context, repo domain.ContentHash, doc inbound.ChunkedDoc) (inbound.DocFinalizationStatus, error) {
	j, err := domain.NewDocFinalizationJob(repo, doc.Hash, domain.DocChunkManifest{Format: doc.Format, Envelope: doc.Envelope, Chunks: doc.Chunks}, time.Now().UTC())
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	if metadata.WorkspaceID == "" {
		return inbound.DocFinalizationStatus{}, domain.ErrForbidden
	}
	st, err := s.docJobs()
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	have, err := s.blobs.HasChunks(ctx, repo, j.Manifest.Chunks)
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	owned := map[domain.ContentHash]bool{}
	for _, h := range have {
		owned[h] = true
	}
	for _, h := range j.Manifest.Chunks {
		if !owned[h] {
			return inbound.DocFinalizationStatus{}, fmt.Errorf("%w: upload repository-owned chunks before finalization", domain.ErrValidation)
		}
	}
	j, err = st.EnqueueDocJob(ctx, j)
	return docJobStatus(j), err
}
func (s *Service) GetDocFinalization(ctx context.Context, repo domain.ContentHash, id string) (inbound.DocFinalizationStatus, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(id)); err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	st, err := s.docJobs()
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	j, err := st.GetDocJob(ctx, repo, id)
	return docJobStatus(j), err
}
func (s *Service) verifyDocJob(ctx context.Context, j domain.DocFinalizationJob) (domain.VerifiedSessionDoc, error) {
	var zero domain.VerifiedSessionDoc
	chunks := make([][]byte, 0, len(j.Manifest.Chunks))
	total := len(j.Manifest.Envelope)
	for _, h := range j.Manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		body, err := s.blobs.GetChunk(ctx, j.RepoID, h)
		if err != nil {
			return zero, err
		}
		total += len(body)
		if total > domain.MaxFinalizedDocBytes {
			return zero, fmt.Errorf("%w: document exceeds finalization limit", domain.ErrValidation)
		}
		if domain.HashContent(body) != h {
			return zero, domain.ErrIntegrity
		}
		chunks = append(chunks, body)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	cb, err := domain.AssembleDocChunks(j.Manifest, chunks, j.DocHash)
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if len(cb) > domain.MaxFinalizedDocBytes {
		return zero, fmt.Errorf("%w: assembled document exceeds finalization limit", domain.ErrValidation)
	}
	var cir domain.CIRDocument
	if err = json.Unmarshal(cb, &cir); err != nil {
		return zero, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	verified, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: j.DocHash, CIR: cir})
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return verified, nil
}
func (s *Service) runDocJob(ctx context.Context, st outbound.DocJobStore, j domain.DocFinalizationJob) error {
	work, cancel := context.WithTimeout(ctx, docJobWorkLimit)
	defer cancel()
	// Renew using an independent short operation. A lost lease cancels validation;
	// Complete also fences it transactionally, including after a process pause.
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	defer stop()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(docJobLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-work.Done():
				return
			case <-ticker.C:
				renew, finish := context.WithTimeout(work, 5*time.Second)
				err := st.RenewDocJob(renew, j, time.Now().UTC(), docJobLease)
				finish()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	verified, err := s.verifyDocJob(work, j)
	stop()
	<-renewed
	if err == nil {
		err = st.CompleteDocJob(work, j, verified, time.Now().UTC())
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	} // shutdown leaves the durable lease for recovery
	now := time.Now().UTC()
	j.State = "retrying"
	j.Reason = "temporary_failure"
	j.UpdatedAt = now
	j.LeaseUntil = time.Time{}
	j.NextAttempt = now.Add(time.Second << min(j.Attempts, 8))
	if errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrUnsupportedCIRVersion) {
		j.State = "rejected"
		j.Reason = "invalid_document"
	}
	finish, c := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer c()
	return errors.Join(err, st.FinishDocJob(finish, j, now))
}
func (s *Service) ProcessDocFinalizations(ctx context.Context, limit int) error {
	st, err := s.docJobs()
	if err != nil {
		return err
	}
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := st.ClaimDocJob(ctx, "", time.Now().UTC(), docJobLease)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = s.runDocJob(ctx, st, j); err != nil {
			return err
		}
	}
	return nil
}

// One worker per process bounds resident large bodies. Database claims also
// serialize a repository across instances; no request spawns its own worker.
func (s *Service) RunDocFinalizationWorker(ctx context.Context) {
	for {
		if err := s.ProcessDocFinalizations(ctx, 1); err != nil && ctx.Err() == nil {
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
