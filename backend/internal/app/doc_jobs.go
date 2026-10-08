package app

import (
	"context"
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
	return inbound.DocFinalizationStatus{ID: j.ID, DocHash: j.DocHash, DocIdentity: j.DocIdentity, State: j.State, Reason: j.Reason, UpdatedAt: j.UpdatedAt}
}
func (s *Service) submitDocFinalizationCommand(ctx context.Context, repo domain.ContentHash, doc inbound.ChunkedDoc) (inbound.DocFinalizationStatus, error) {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
	return s.submitDocFinalizationWithPolicy(ctx, repo, doc, s.RootPublicationEnabled())
}

// enabled comes only from startup policy at the public command boundary. Tests
// can exercise ready admission without changing the binary's release fuse.
func (s *Service) submitDocFinalizationWithPolicy(ctx context.Context, repo domain.ContentHash, doc domain.DocumentRepresentation, enabled bool) (inbound.DocFinalizationStatus, error) {
	if err := doc.Identity.Validate(); err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	if doc.Identity == domain.DocumentIdentityRootV1 && !enabled {
		return inbound.DocFinalizationStatus{}, domain.ErrRootPublicationDisabled
	}
	j, err := domain.NewDocFinalizationJobForRepresentation(repo, doc, time.Now().UTC())
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	if metadata.RepositoryID == "" {
		return inbound.DocFinalizationStatus{}, domain.ErrForbidden
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, metadata.RequiredDocIdentity); err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	if doc.Identity == domain.DocumentIdentityRootV1 && metadata.RequiredDocIdentity != doc.Identity {
		return inbound.DocFinalizationStatus{}, domain.ErrRootPublicationDisabled
	}
	st, err := s.docJobs()
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	chunks := j.Manifest.Chunks
	if doc.Identity == domain.DocumentIdentityRootV1 {
		manifest, err := doc.ConversationManifest()
		if err != nil {
			return inbound.DocFinalizationStatus{}, err
		}
		chunks = make([]domain.ContentHash, len(manifest.Chunks))
		for i, chunk := range manifest.Chunks {
			chunks[i] = chunk.Hash
		}
	}
	have, err := s.blobs.HasChunks(ctx, repo, chunks)
	if err != nil {
		return inbound.DocFinalizationStatus{}, err
	}
	owned := map[domain.ContentHash]bool{}
	for _, h := range have {
		owned[h] = true
	}
	for _, h := range chunks {
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
	return repositoryReadForRepo(ctx, s, repo, func(read context.Context) (inbound.DocFinalizationStatus, error) {
		j, err := st.GetDocJob(read, repo, id)
		return docJobStatus(j), err
	})
}
func (s *Service) verifyDocJob(ctx context.Context, j domain.DocFinalizationJob) (domain.VerifiedSessionDoc, error) {
	if err := j.Validate(); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	representation, err := j.Representation()
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if representation.Identity == domain.DocumentIdentityRootV1 {
		manifest, err := representation.ConversationManifest()
		if err != nil {
			return domain.VerifiedSessionDoc{}, err
		}
		reader, ok := s.blobs.(outbound.ConversationChunkReader)
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
		return s.docVerifier.VerifyConversationManifestDoc(ctx, j.DocHash, manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
			return reader.ReadConversationChunk(ctx, j.RepoID, hash, lengths[hash])
		})
	}
	return s.docVerifier.VerifyChunks(ctx, j.DocHash, j.Manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		// A semantic proof never authorizes another repository's chunk. Read
		// the current owned bytes even when the verifier has seen their events.
		return s.blobs.GetChunk(ctx, j.RepoID, hash)
	})
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
	verified, err := repositoryReadForRepo(work, s, j.RepoID, func(read context.Context) (domain.VerifiedSessionDoc, error) { return s.verifyDocJob(read, j) })
	var publication outbound.PreparedDocPublication
	if err == nil {
		publication, err = st.PrepareDocJob(work, verified)
	}
	stop()
	<-renewed
	// Preparation may succeed before an in-flight renewal cancels the work.
	if err == nil {
		err = work.Err()
	}
	if err == nil {
		err = publication.Complete(work, j, time.Now().UTC())
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
	if errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrUnsupportedCIRVersion) || errors.Is(err, domain.ErrConversationManifest) || errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		j.State = "rejected"
		j.Reason = "invalid_document"
	}
	finish, c := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer c()
	return errors.Join(err, st.FinishDocJob(finish, j, now))
}
func (s *Service) ProcessDocFinalizations(ctx context.Context, limit int) error {
	ctx = s.DocumentIdentityWorkerContext(inbound.WithSystemActor(ctx))
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
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

func (s *Service) SubmitDocFinalization(ctx context.Context, repo domain.ContentHash, doc inbound.ChunkedDoc) (inbound.DocFinalizationStatus, error) {
	return repositoryWrite(context.WithValue(ctx, revisionScopeKey{}, "none"), s, repo, func(ctx context.Context) (inbound.DocFinalizationStatus, error) {
		return s.submitDocFinalizationCommand(ctx, repo, doc)
	})
}
