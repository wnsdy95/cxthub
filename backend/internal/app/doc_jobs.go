package app

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docJobLease = documentworker.Lease

// This bound is for background work, independent of the 30s HTTP deadline. It
// prevents a pathological body from holding a worker indefinitely.
const docJobWorkLimit = documentworker.WorkLimit

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
	finalizer, err := s.documentFinalizer(nil)
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	return finalizer.Verify(ctx, j)
}
func (s *Service) runDocJob(ctx context.Context, st outbound.DocJobStore, j domain.DocFinalizationJob) error {
	finalizer, err := s.documentFinalizer(st)
	if err != nil {
		return err
	}
	return finalizer.RunJob(ctx, j)
}
func (s *Service) ProcessDocFinalizations(ctx context.Context, limit int) error {
	storage, _ := s.blobs.(outbound.DocJobStore)
	finalizer, err := s.documentFinalizer(storage)
	if err != nil {
		return err
	}
	return finalizer.Process(ctx, limit)
}

// One worker per process bounds resident large bodies. Database claims also
// serialize a repository across instances; no request spawns its own worker.
func (s *Service) RunDocFinalizationWorker(ctx context.Context) {
	storage, _ := s.blobs.(outbound.DocJobStore)
	finalizer, err := s.documentFinalizer(storage)
	if err != nil {
		log.Printf("document finalization: %v", err)
		return
	}
	finalizer.Run(ctx)
}

func (s *Service) SubmitDocFinalization(ctx context.Context, repo domain.ContentHash, doc inbound.ChunkedDoc) (inbound.DocFinalizationStatus, error) {
	return repositoryWrite(context.WithValue(ctx, revisionScopeKey{}, "none"), s, repo, func(ctx context.Context) (inbound.DocFinalizationStatus, error) {
		return s.submitDocFinalizationCommand(ctx, repo, doc)
	})
}
