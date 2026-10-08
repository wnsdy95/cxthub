package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type rootChunkSink interface {
	PutChunk(context.Context, domain.ContentHash, []byte) error
}

func (c *BackendClient) pullRootDocument(ctx context.Context, repo string, ref domain.DocumentRef, receiver outbound.PullDocumentReceiver) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.Identity != domain.DocumentIdentityRootV1 || receiver == nil {
		return domain.ErrUnsupportedDocumentIdentity
	}
	sink, ok := c.chunks.(rootChunkSink)
	if !ok {
		return domain.ErrUnsupportedDocumentIdentity
	}
	retention, ok := c.chunks.(outbound.ObjectRetention)
	if !ok {
		return domain.ErrUnsupportedDocumentIdentity
	}
	return retention.WithObjectsRetained(ctx, func() error {
		remote := c.SyncRemoteIdentity()
		var response pullResp
		if err := c.doLimited(ctx, http.MethodPost, c.reposPath(repo)+"/pull/objects", pullReq{DocManifestWants: []domain.ContentHash{ref.Hash}, ChunkFormatsSupported: []string{chunkcas.FormatV2}, CIRVersionsSupported: domain.SupportedCIRVersions()}, &response, maxChunkWireJSONBody); err != nil {
			return err
		}
		if !response.BoundedChunksSupported || len(response.DocManifests) != 1 || len(response.Docs) != 0 || len(response.ChunkObjects) != 0 || len(response.Snapshots) != 0 {
			return domain.ErrHashMismatch
		}
		rep := response.DocManifests[0]
		if rep.DocumentRef() != ref {
			return domain.ErrHashMismatch
		}
		manifest, err := rep.ConversationManifest()
		if err != nil {
			return err
		}
		sizes := map[domain.ContentHash]int{}
		var pending []domain.ContentHash
		for _, chunk := range manifest.Chunks {
			if old, exists := sizes[chunk.Hash]; exists {
				if old != int(chunk.Bytes) {
					return domain.ErrHashMismatch
				}
				continue
			}
			sizes[chunk.Hash] = int(chunk.Bytes)
			// Existing files remain untrusted until the installer's bounded full read.
			// Do not turn a corrupt local object into an implicit download repair.
			if !c.chunks.HasChunk(chunk.Hash) {
				pending = append(pending, chunk.Hash)
			}
		}
		for len(pending) > 0 {
			wants := pending[:min(len(pending), maxChunkWireObjects)]
			var batch pullResp
			if err := c.doLimited(ctx, http.MethodPost, c.reposPath(repo)+"/pull/chunks", pullReq{ChunkWants: wants}, &batch, maxChunkWireJSONBody); err != nil {
				return err
			}
			if len(batch.ChunkObjects) == 0 || len(batch.ChunkObjects) > len(wants) || len(batch.Docs) != 0 || len(batch.DocManifests) != 0 || len(batch.Snapshots) != 0 {
				return domain.ErrHashMismatch
			}
			total := 0
			for i, obj := range batch.ChunkObjects {
				total += len(obj.Data)
				if obj.Hash != wants[i] || len(obj.Data) != sizes[obj.Hash] || len(obj.Data) == 0 || total > maxChunkWireRawBytes || domain.HashContent(obj.Data) != obj.Hash {
					return domain.ErrHashMismatch
				}
			}
			for _, obj := range batch.ChunkObjects {
				if err := sink.PutChunk(ctx, obj.Hash, obj.Data); err != nil {
					return err
				}
			}
			pending = pending[len(batch.ChunkObjects):]
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.SyncRemoteIdentity() != remote {
			return domain.ErrSyncConflict
		}
		return receiver.ReceiveRoot(ctx, rep)
	})
}
