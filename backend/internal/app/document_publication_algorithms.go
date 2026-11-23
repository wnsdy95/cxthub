package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (commands *DocumentPublicationCommands) storeChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	if len(in.Chunks) == 0 || len(in.Chunks) > inbound.MaxChunkWireObjects {
		return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk batch count must be 1..%d", domain.ErrValidation, inbound.MaxChunkWireObjects)
	}
	repo, err := commands.store.GetRepo(ctx, in.RepoID)
	if err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	if repo.RepositoryID == "" {
		return inbound.StoreChunksOutput{}, fmt.Errorf("%w: repo is not bound to a repository", domain.ErrForbidden)
	}
	bodies := make(map[domain.ContentHash][]byte, len(in.Chunks))
	total := 0
	for _, chunk := range in.Chunks {
		if err := domain.ValidateContentHash(chunk.Hash); err != nil {
			return inbound.StoreChunksOutput{}, err
		}
		if len(chunk.Data) == 0 || len(chunk.Data) > inbound.MaxChunkWireRawBytes {
			return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk %s exceeds bounded transport size", domain.ErrValidation, chunk.Hash)
		}
		if domain.HashContent(chunk.Data) != chunk.Hash {
			return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk %s body hash mismatch", domain.ErrIntegrity, chunk.Hash)
		}
		if previous, exists := bodies[chunk.Hash]; exists {
			if !bytes.Equal(previous, chunk.Data) {
				return inbound.StoreChunksOutput{}, fmt.Errorf("%w: duplicate chunk %s disagrees", domain.ErrIntegrity, chunk.Hash)
			}
			continue
		}
		total += len(chunk.Data)
		if total > inbound.MaxChunkWireRawBytes {
			return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk batch exceeds bounded transport size", domain.ErrValidation)
		}
		bodies[chunk.Hash] = chunk.Data
	}
	stored, deduped, err := commands.chunks.PutChunks(ctx, in.RepoID, bodies)
	return inbound.StoreChunksOutput{Stored: stored, Deduped: deduped}, err
}

func (commands *DocumentPublicationCommands) commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error) {
	if err := ctx.Err(); err != nil {
		return inbound.CommitOutput{}, err
	}
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.CommitOutput{}, err
	}
	// A capture is no longer stored: a client that still sends one is told so
	// before anything is written.
	if len(in.Snapshots) > 0 {
		return inbound.CommitOutput{}, fmt.Errorf("%w: this server stores sessions, not captures; update cxt", domain.ErrValidation)
	}
	// The staged discriminator is not an advertised publication capability.
	// Reject a mixed batch before reassembly, body reads or any object writes.
	for _, doc := range in.Docs {
		if doc.Identity != domain.DocumentIdentityLegacy {
			return inbound.CommitOutput{}, domain.ErrUnsupportedDocumentIdentity
		}
	}
	for _, doc := range in.ChunkedDocs {
		if doc.Identity != domain.DocumentIdentityLegacy {
			return inbound.CommitOutput{}, domain.ErrUnsupportedDocumentIdentity
		}
	}
	// Reassemble a chunked wire document from chunks received in this request plus repository-owned stored chunks. Verify the canonical document hash, then pass the complete document through the ordinary validation and storage path.
	docs := in.Docs
	if len(in.ChunkedDocs) > 0 {
		bodies := make(map[domain.ContentHash][]byte, len(in.ChunkObjects))
		for _, chunkObject := range in.ChunkObjects {
			if err := ctx.Err(); err != nil {
				return inbound.CommitOutput{}, err
			}
			if err := domain.ValidateContentHash(chunkObject.Hash); err != nil {
				return inbound.CommitOutput{}, err
			}
			if domain.HashContent(chunkObject.Data) != chunkObject.Hash {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunk %s body hash mismatch", domain.ErrIntegrity, chunkObject.Hash)
			}
			bodies[chunkObject.Hash] = chunkObject.Data
		}
		for _, chunkedDoc := range in.ChunkedDocs {
			if err := ctx.Err(); err != nil {
				return inbound.CommitOutput{}, err
			}
			if err := domain.ValidateContentHash(chunkedDoc.Hash); err != nil {
				return inbound.CommitOutput{}, err
			}
			format := chunkedDoc.Format
			if format == "" {
				format = domain.ChunkFormatV1
			}
			if !domain.SupportedChunkFormat(format) {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s uses unsupported format %q", domain.ErrValidation, chunkedDoc.Hash, chunkedDoc.Format)
			}
			chunks := make([][]byte, 0, len(chunkedDoc.Chunks))
			for _, chunkHash := range chunkedDoc.Chunks {
				if err := ctx.Err(); err != nil {
					return inbound.CommitOutput{}, err
				}
				if err := domain.ValidateContentHash(chunkHash); err != nil {
					return inbound.CommitOutput{}, err
				}
				body, ok := bodies[chunkHash]
				if !ok {
					var loadErr error
					if body, loadErr = commands.chunks.GetChunk(ctx, in.RepoID, chunkHash); loadErr != nil {
						if err := ctx.Err(); err != nil {
							return inbound.CommitOutput{}, err
						}
						return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s references unknown chunk %s", domain.ErrIntegrity, chunkedDoc.Hash, chunkHash)
					}
				}
				chunks = append(chunks, body)
			}
			if err := ctx.Err(); err != nil {
				return inbound.CommitOutput{}, err
			}
			canonicalBody, assemblyErr := domain.AssembleDocChunks(domain.DocChunkManifest{Format: format, Envelope: chunkedDoc.Envelope, Chunks: chunkedDoc.Chunks}, chunks, chunkedDoc.Hash)
			if assemblyErr != nil {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s reassembly mismatch", domain.ErrIntegrity, chunkedDoc.Hash)
			}
			if err := ctx.Err(); err != nil {
				return inbound.CommitOutput{}, err
			}
			var cir domain.CIRDocument
			if err := json.Unmarshal(canonicalBody, &cir); err != nil {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s undecodable", domain.ErrIntegrity, chunkedDoc.Hash)
			}
			docs = append(docs, domain.SessionDoc{Hash: chunkedDoc.Hash, CIR: cir})
		}
	}
	verifiedDocs := make([]domain.VerifiedSessionDoc, 0, len(docs))
	for _, document := range docs {
		if err := ctx.Err(); err != nil {
			return inbound.CommitOutput{}, err
		}
		// Validate every submitted body, even a duplicate claimed hash.
		verified, err := domain.VerifySessionDoc(document)
		if err != nil {
			return inbound.CommitOutput{}, err
		}
		verifiedDocs = append(verifiedDocs, verified)
	}
	if err := ctx.Err(); err != nil {
		return inbound.CommitOutput{}, err
	}
	repo, err := commands.store.GetRepo(ctx, in.RepoID)
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	if repo.RepositoryID == "" {
		return inbound.CommitOutput{}, fmt.Errorf("%w: repo is not bound to a repository", domain.ErrForbidden)
	}
	var out inbound.CommitOutput
	for position, document := range docs {
		if err := ctx.Err(); err != nil {
			return inbound.CommitOutput{}, err
		}
		var stored bool
		var err error
		if commands.verified != nil {
			stored, err = commands.verified.PutVerifiedDoc(ctx, in.RepoID, verifiedDocs[position])
		} else if commands.legacy != nil {
			stored, err = commands.legacy.PutDoc(ctx, in.RepoID, document)
		} else {
			return inbound.CommitOutput{}, domain.ErrCatalogUnsupported
		}
		if err != nil {
			return inbound.CommitOutput{}, err
		}
		if stored {
			out.StoredDocs++
		} else {
			out.DedupedDocs++
		}
	}
	if err := ctx.Err(); err != nil {
		return inbound.CommitOutput{}, err
	}
	return out, nil
}
