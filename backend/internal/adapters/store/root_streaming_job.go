package store

import (
	"bytes"
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedStreamingRootDoc struct {
	job   domain.DocFinalizationJob
	root  preparedRootDoc
	spool *docRangeSpool
}

func streamingJobRoot(job domain.DocFinalizationJob) (preparedRootDoc, error) {
	if err := job.Validate(); err != nil {
		return preparedRootDoc{}, err
	}
	if job.DocIdentity != domain.DocumentIdentityRootV1 {
		return preparedRootDoc{}, domain.ErrUnsupportedDocumentIdentity
	}
	representation, err := job.Representation()
	if err != nil {
		return preparedRootDoc{}, err
	}
	manifest, err := representation.ConversationManifest()
	if err != nil {
		return preparedRootDoc{}, err
	}
	canonical, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return preparedRootDoc{}, err
	}
	return preparedRootDoc{manifest: manifest, canonical: canonical}, nil
}

func prepareStreamingRootJob(ctx context.Context, job domain.DocFinalizationJob, verifier *domain.CanonicalDocVerifier, read storedChunkReader) (*preparedStreamingRootDoc, error) {
	root, err := streamingJobRoot(job)
	if err != nil {
		return nil, err
	}
	spool, err := newDocRangeSpool()
	if err != nil {
		return nil, err
	}
	root.ranges, err = verifyStoredRangeIndex(ctx, verifier, job.DocHash, root.manifest, read, spool)
	if err != nil {
		_ = spool.Close()
		return nil, err
	}
	return &preparedStreamingRootDoc{job: job, root: root, spool: spool}, nil
}

func (prepared *preparedStreamingRootDoc) matches(job domain.DocFinalizationJob) error {
	if prepared == nil || prepared.spool == nil {
		return domain.ErrIntegrity
	}
	if _, err := prepared.spool.file.Stat(); err != nil {
		return err
	}
	if job.RepoID != prepared.job.RepoID || job.ID != prepared.job.ID || job.Version != prepared.job.Version || job.DocHash != prepared.job.DocHash {
		return domain.ErrConflict
	}
	root, err := streamingJobRoot(job)
	if err != nil {
		return err
	}
	if !bytes.Equal(root.canonical, prepared.root.canonical) {
		return domain.ErrIntegrity
	}
	return nil
}

func (prepared *preparedStreamingRootDoc) compareCurrent(ctx context.Context, read storedChunkReader) error {
	for _, chunk := range prepared.root.manifest.Chunks {
		raw, err := read(ctx, chunk.Hash)
		if err != nil {
			return err
		}
		body, err := rootChunkBytes(ctx, raw, chunk.Bytes)
		if err != nil {
			return err
		}
		if domain.HashContent(body) != chunk.Hash {
			return domain.ErrIntegrity
		}
	}
	return ctx.Err()
}

func (prepared *preparedStreamingRootDoc) Close() error {
	return prepared.spool.Close()
}
