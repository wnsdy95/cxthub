package store

import (
	"bytes"
	"context"
	"math"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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
	if prepared == nil || prepared.spool == nil || prepared.spool.file == nil || prepared.spool.writer == nil {
		return domain.ErrIntegrity
	}
	if _, err := prepared.spool.file.Stat(); err != nil {
		return err
	}
	if job.RepoID != prepared.job.RepoID || job.RepositoryEpoch != prepared.job.RepositoryEpoch || job.ID != prepared.job.ID || job.Version != prepared.job.Version || job.DocHash != prepared.job.DocHash {
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
	if prepared == nil || prepared.spool == nil || prepared.spool.file == nil {
		return domain.ErrIntegrity
	}
	err := prepared.spool.Close()
	prepared.spool.offsets = nil
	prepared.job = domain.DocFinalizationJob{}
	prepared.root = preparedRootDoc{}
	return err
}

func (prepared *preparedStreamingRootDoc) RetainedResources() (outbound.RootPublicationResources, error) {
	if prepared == nil || prepared.spool == nil || prepared.spool.file == nil || prepared.spool.writer == nil || prepared.job.DocIdentity != domain.DocumentIdentityRootV1 {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	spool := prepared.spool
	info, err := spool.file.Stat()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	events := prepared.root.manifest.EventCount
	if !info.Mode().IsRegular() || info.Size() < 0 || spool.writer.Buffered() != 0 || len(spool.offsets) == 0 || spool.offsets[0] != 0 ||
		events < 0 || events > domain.MaxConversationDocumentBytes || int64(len(spool.offsets)-1) != (events+domain.DocRangePageEvents-1)/domain.DocRangePageEvents {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	for position := 1; position < len(spool.offsets); position++ {
		previous, next := spool.offsets[position-1], spool.offsets[position]
		if next <= previous || next-previous > domain.MaxDocRangePageBytes {
			return outbound.RootPublicationResources{}, domain.ErrIntegrity
		}
	}
	if info.Size() != spool.offsets[len(spool.offsets)-1] {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	headerBytes, err := prepared.root.ranges.RetainedHeaderBytes()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	estimate := rootPublicationHeapEstimate{bytes: headerBytes}
	estimate.allocation(1, 4096+int64(unsafe.Sizeof(*prepared)))
	estimate.allocation(int64(cap(prepared.job.RootManifest)), 1)
	estimate.allocation(int64(cap(prepared.job.Manifest.Envelope)), 1)
	estimate.allocation(int64(cap(prepared.job.Manifest.Chunks)), int64(unsafe.Sizeof(domain.ContentHash(""))))
	estimate.allocation(int64(cap(prepared.root.canonical)), 1)
	estimate.allocation(int64(cap(prepared.root.manifest.Envelope)), 1)
	estimate.allocation(int64(cap(prepared.root.manifest.Chunks)), int64(unsafe.Sizeof(domain.ConversationManifestChunk{})))
	estimate.allocation(int64(cap(spool.offsets)), int64(unsafe.Sizeof(int64(0))))
	estimate.allocation(int64(spool.writer.Size()), 1)
	for _, value := range []string{
		string(prepared.job.DocIdentity), prepared.job.ID, string(prepared.job.RepoID), prepared.job.RepositoryEpoch, string(prepared.job.DocHash),
		prepared.job.Manifest.Format, prepared.job.State, prepared.job.Reason,
		prepared.root.manifest.Identity, prepared.root.manifest.ChunkFormat, spool.file.Name(),
	} {
		estimate.allocation(int64(len(value)), 1)
	}
	for _, hash := range prepared.job.Manifest.Chunks[:cap(prepared.job.Manifest.Chunks)] {
		estimate.allocation(int64(len(hash)), 1)
	}
	for _, chunk := range prepared.root.manifest.Chunks[:cap(prepared.root.manifest.Chunks)] {
		estimate.allocation(int64(len(chunk.Hash)), 1)
	}
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	return outbound.RootPublicationResources{HeapBytes: estimate.bytes, SpoolBytes: info.Size()}, nil
}

type rootPublicationHeapEstimate struct {
	bytes int64
	err   error
}

func (estimate *rootPublicationHeapEstimate) include(other rootPublicationHeapEstimate) {
	if estimate.err != nil {
		return
	}
	if other.err != nil {
		estimate.err = other.err
		return
	}
	if estimate.bytes < 0 || other.bytes < 0 || other.bytes > math.MaxInt64-estimate.bytes {
		estimate.err = domain.ErrIntegrity
		return
	}
	estimate.bytes += other.bytes
}

func (estimate *rootPublicationHeapEstimate) allocation(count, width int64) {
	if estimate.err != nil {
		return
	}
	const overhead = 64
	if count < 0 || width <= 0 || estimate.bytes < 0 || estimate.bytes > math.MaxInt64-overhead || count > (math.MaxInt64-overhead-estimate.bytes)/2/width {
		estimate.err = domain.ErrIntegrity
		return
	}
	if count != 0 {
		estimate.bytes += overhead + 2*count*width
	}
}
