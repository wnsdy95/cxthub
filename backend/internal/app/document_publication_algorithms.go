package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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
	// The staged discriminator is not an advertised publication capability.
	// Reject a mixed batch before reassembly, body reads or any object writes,
	// including snapshots whose legacy body is supplied in this same request.
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
	for _, snap := range in.Snapshots {
		if err := snap.DocIdentity.Validate(); err != nil {
			return inbound.CommitOutput{}, err
		}
		if snap.DocIdentity == domain.DocumentIdentityRootV1 {
			if !hasDocumentIdentity(commands.supported, snap.DocIdentity) {
				return inbound.CommitOutput{}, domain.ErrUnsupportedDocumentIdentity
			}
			// This route attaches metadata to an already-owned, verified root.
			// New root admission belongs to the durable finalization endpoint;
			// disabling it must not freeze existing snapshots or memory archives.
			bound := outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), commands.supported)
			if err := requireRootDocumentRepository(bound, commands.store, in.RepoID); err != nil {
				return inbound.CommitOutput{}, err
			}
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
	docByHash := make(map[domain.ContentHash]domain.VerifiedSessionDoc, len(docs))
	verifiedDocs := make([]domain.VerifiedSessionDoc, 0, len(docs))
	for _, document := range docs {
		if err := ctx.Err(); err != nil {
			return inbound.CommitOutput{}, err
		}
		verified, err := domain.VerifySessionDoc(document)
		if err != nil {
			return inbound.CommitOutput{}, err
		}
		// Validate every submitted body, even a duplicate claimed hash.
		docByHash[document.Hash] = verified
		verifiedDocs = append(verifiedDocs, verified)
	}
	verifiedSettings := make(map[domain.ContentHash]bool)
	normalizedSnaps := make([]domain.Snapshot, 0, len(in.Snapshots))
	for _, snap := range in.Snapshots {
		if err := ctx.Err(); err != nil {
			return inbound.CommitOutput{}, err
		}
		snap.RepoID = in.RepoID
		// Branches is a response-specific projection calculated from the branch reflog. If the client stores the sent value, it can introduce fake membership in the next response through the FS adapter.
		snap.Branches = nil
		if err := domain.ValidateSnapshotHashes(snap); err != nil {
			return inbound.CommitOutput{}, err
		}
		if document, ok := docByHash[snap.DocHash]; ok {
			if !document.Reference().Matches(snap) {
				return inbound.CommitOutput{}, domain.ErrIntegrity
			}
		} else {
			if commands.sources == nil {
				return inbound.CommitOutput{}, domain.ErrCatalogUnsupported
			}
			err := commands.sources.VerifySnapshotDocument(ctx, in.RepoID, snap)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					return inbound.CommitOutput{}, fmt.Errorf("%w: snapshot %s references an absent doc %s (send doc first via push/objects)", domain.ErrIntegrity, snap.ID, snap.DocHash)
				}
				return inbound.CommitOutput{}, err
			}
		}
		for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
			if hash == "" || verifiedSettings[hash] {
				continue
			}
			if _, err := commands.store.GetSettingsObject(ctx, in.RepoID, hash); err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					return inbound.CommitOutput{}, fmt.Errorf("%w: snapshot %s references missing settings object %s", domain.ErrIntegrity, snap.ID, hash)
				}
				return inbound.CommitOutput{}, err
			}
			verifiedSettings[hash] = true
		}
		// MemoryHash is derived metadata. Only PutMemoryDigest may attach it after
		// validating the digest hash, snapshot ownership, and repo-scoped blob grant.
		snap.MemoryHash = ""
		normalizedSnaps = append(normalizedSnaps, snap)
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
	existingSnaps, err := commands.store.ListSnapshots(ctx, in.RepoID, "")
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	if err := domain.ValidateSnapshotGraphUpdate(existingSnaps, normalizedSnaps); err != nil {
		return inbound.CommitOutput{}, err
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
	for _, snap := range normalizedSnaps {
		if err := ctx.Err(); err != nil {
			return inbound.CommitOutput{}, err
		}
		before, priorErr := commands.store.GetSnapshot(ctx, in.RepoID, snap.ID)
		if err := priorErr; err == nil {
			out.DedupedSnapshots++
		} else if errors.Is(err, domain.ErrNotFound) {
			out.StoredSnapshots++
		} else {
			return inbound.CommitOutput{}, err
		}
		if err := commands.store.PutSnapshot(ctx, snap); err != nil {
			return inbound.CommitOutput{}, err
		}
		if priorErr == nil {
			after, err := commands.store.GetSnapshot(ctx, in.RepoID, snap.ID)
			if err != nil {
				return inbound.CommitOutput{}, err
			}
			if !reflect.DeepEqual(before, after) {
				if err := commands.scope.GraphChanged(ctx, in.RepoID); err != nil {
					return inbound.CommitOutput{}, err
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return inbound.CommitOutput{}, err
	}
	return out, nil
}
