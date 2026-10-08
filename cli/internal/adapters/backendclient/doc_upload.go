package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.ChunkedDocumentPusher = (*BackendClient)(nil)

// PushDocChunks uses the store's verified descriptor without assembling the
// document. Only a capability miss can return false, nil, before any writes.
// The store proves the complete canonical identity; requested bodies are also
// hash-checked here, and the server still validates the complete document.
func (c *BackendClient) PushDocChunks(ctx context.Context, repoID string, doc outbound.DocumentChunks) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return true, err
	}
	rep := doc.Representation
	if err := rep.DocumentRef().Validate(); err != nil {
		return true, err
	}
	root := rep.Identity == domain.DocumentIdentityRootV1
	format, envelopeBytes, hashes := rep.Format, rep.Envelope, rep.Chunks
	if root {
		manifest, err := rep.ConversationManifest()
		if err != nil {
			return true, err
		}
		format, envelopeBytes = manifest.ChunkFormat, manifest.Envelope
		hashes = make([]domain.ContentHash, 0, len(manifest.Chunks))
		for _, chunk := range manifest.Chunks {
			hashes = append(hashes, chunk.Hash)
		}
	} else if rep.RootManifest != nil {
		return true, domain.ErrHashMismatch
	}
	if !chunkcas.SupportedFormat(format) || (!root && len(hashes) == 0) || doc.ReadChunk == nil {
		return true, fmt.Errorf("%w: invalid document chunk descriptor", domain.ErrHashMismatch)
	}

	var envelope *domain.Envelope
	if err := json.Unmarshal(envelopeBytes, &envelope); err != nil || envelope == nil {
		return true, fmt.Errorf("%w: invalid document chunk envelope", domain.ErrInvalidCIR)
	}
	// Normalize only the envelope through the existing canonical rules. This
	// rejects empty/noncanonical metadata without decoding any event bodies.
	canonical, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: *envelope})
	if err != nil {
		return true, err
	}
	if !bytes.Equal(canonical, chunkcas.Assemble(envelopeBytes, nil)) {
		return true, fmt.Errorf("%w: noncanonical document chunk envelope", domain.ErrInvalidCIR)
	}
	// Repeated IDs are legitimate in a byte-stream manifest. Advertise and read
	// each body once, while preserving every occurrence in the final manifest.
	chunkHaves := make([]domain.ContentHash, 0, len(hashes))
	seen := make(map[domain.ContentHash]bool, len(hashes))
	for _, hash := range hashes {
		if err := domain.ValidateContentHash(hash); err != nil {
			return true, err
		}
		if !seen[hash] {
			seen[hash] = true
			chunkHaves = append(chunkHaves, hash)
		}
	}
	if !root && !chunkcas.PortableManifest(chunkcas.Manifest{Format: format, Envelope: envelopeBytes, Chunks: hashes}) {
		return false, nil
	}
	if root {
		if err := c.PreflightDocumentReferences(ctx, repoID, []domain.DocumentRef{rep.DocumentRef()}); err != nil {
			return true, err
		}
	}
	docHaves := []domain.ContentHash{rep.Hash}
	var neg negotiateResp
	negotiationCtx := outbound.WithSyncDiagnosticRole(ctx, outbound.SyncRoleDocumentChunks)
	outbound.RecordSyncDiagnostic(negotiationCtx, outbound.SyncStagePreparation, outbound.SyncDiagnosticCounts{Documents: 1, Chunks: len(chunkHaves)}, nil)
	if err := c.do(negotiationCtx, http.MethodPost, c.reposPath(repoID)+"/push/negotiate", negotiateReq{DocHaves: docHaves, ChunkHaves: chunkHaves}, &neg); err != nil {
		return true, err
	}
	// Validate every response field even for an unsupported or already-current
	// peer. Unoffered/duplicate wants must never drive reads or legacy retries.
	if err := validateNegotiatedSubset(nil, neg.SnapshotWants); err != nil {
		return true, err
	}
	if err := validateNegotiatedSubset(docHaves, neg.DocWants); err != nil {
		return true, err
	}
	if err := validateNegotiatedSubset(chunkHaves, neg.ChunkWants); err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if root && (!supportsRootIdentity(neg.DocIdentitiesSupported) || !neg.AsyncDocsSupported || !neg.ChunksSupported || !neg.BoundedChunksSupported || !containsString(neg.ChunkFormatsSupported, chunkcas.FormatV2)) {
		return true, domain.ErrUnsupportedDocumentIdentity
	}
	if len(neg.DocWants) == 0 {
		return true, nil
	}
	// Existing roots remain usable when admission of new roots is disabled.
	if root && !neg.RootPublicationEnabled {
		return true, domain.ErrUnsupportedDocumentIdentity
	}
	version := envelope.CIRVersion
	if version == "" {
		version = domain.CIRVersionV1
	}
	if !domain.SupportsCIRVersion(neg.CIRVersionsSupported, version) && (version != domain.CIRVersionV1 || len(neg.CIRVersionsSupported) > 0) {
		return true, fmt.Errorf("%w: server does not advertise CIR %s support", domain.ErrUnsupportedCIRVersion, version)
	}
	if format == "" {
		format = chunkcas.FormatV1
	}
	if !neg.ChunksSupported || !neg.BoundedChunksSupported || !containsString(neg.ChunkFormatsSupported, format) {
		return false, nil
	}
	if err := c.pushRequestedDocChunks(ctx, repoID, doc, chunkHaves, setOf(neg.ChunkWants)); err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if format == chunkcas.FormatV1 {
		format = "" // Match the existing legacy manifest wire encoding.
	}
	manifest := rep
	if !root {
		manifest = chunkedDocWire{Hash: rep.Hash, Format: format, Envelope: envelopeBytes, Chunks: hashes}
	}
	if neg.AsyncDocsSupported {
		return true, c.finalizeDocument(ctx, repoID, manifest)
	}
	return true, c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/objects", objectsReq{ChunkedDocs: []chunkedDocWire{manifest}}, nil)
}

func (c *BackendClient) pushRequestedDocChunks(ctx context.Context, repoID string, doc outbound.DocumentChunks, order []domain.ContentHash, wants map[domain.ContentHash]bool) error {
	var rootSizes map[domain.ContentHash]int
	if doc.Representation.Identity == domain.DocumentIdentityRootV1 {
		manifest, err := doc.Representation.ConversationManifest()
		if err != nil {
			return err
		}
		rootSizes = map[domain.ContentHash]int{}
		for _, chunk := range manifest.Chunks {
			rootSizes[chunk.Hash] = int(chunk.Bytes)
		}
	}
	var pending []chunkObjWire
	for _, hash := range order {
		if !wants[hash] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := doc.ReadChunk(ctx, hash)
		if err != nil {
			return fmt.Errorf("read document chunk %s: %w", hash, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if domain.HashContent(body) != hash || (rootSizes != nil && len(body) != rootSizes[hash]) {
			return fmt.Errorf("%w: document chunk %s", domain.ErrHashMismatch, hash)
		}
		// Keep at most a bounded batch plus one lookahead body. Reuse the same
		// size/count policy as ordinary Push instead of collecting all bodies.
		batches, err := chunkUploadBatches(append(pending, chunkObjWire{Hash: hash, Data: body}))
		if err != nil {
			return err
		}
		for _, batch := range batches[:len(batches)-1] {
			if err := c.pushChunkBatches(ctx, repoID, batch); err != nil {
				return err
			}
		}
		pending = batches[len(batches)-1]
	}
	return c.pushChunkBatches(ctx, repoID, pending)
}
