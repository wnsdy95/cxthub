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
	if err := domain.ValidateContentHash(doc.Hash); err != nil {
		return true, err
	}
	if !chunkcas.SupportedFormat(doc.Format) || len(doc.Chunks) == 0 || doc.ReadChunk == nil {
		return true, fmt.Errorf("%w: invalid document chunk descriptor", domain.ErrHashMismatch)
	}
	var envelope *domain.Envelope
	if err := json.Unmarshal(doc.Envelope, &envelope); err != nil || envelope == nil {
		return true, fmt.Errorf("%w: invalid document chunk envelope", domain.ErrInvalidCIR)
	}
	// Normalize only the envelope through the existing canonical rules. This
	// rejects empty/noncanonical metadata without decoding any event bodies.
	canonical, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: *envelope})
	if err != nil {
		return true, err
	}
	if !bytes.Equal(canonical, chunkcas.Assemble(doc.Envelope, nil)) {
		return true, fmt.Errorf("%w: noncanonical document chunk envelope", domain.ErrInvalidCIR)
	}
	// Repeated IDs are legitimate in a byte-stream manifest. Advertise and read
	// each body once, while preserving every occurrence in the final manifest.
	chunkHaves := make([]domain.ContentHash, 0, len(doc.Chunks))
	seen := make(map[domain.ContentHash]bool, len(doc.Chunks))
	for _, hash := range doc.Chunks {
		if err := domain.ValidateContentHash(hash); err != nil {
			return true, err
		}
		if !seen[hash] {
			seen[hash] = true
			chunkHaves = append(chunkHaves, hash)
		}
	}
	docHaves := []domain.ContentHash{doc.Hash}
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
	if len(neg.DocWants) == 0 {
		return true, nil
	}
	version := envelope.CIRVersion
	if version == "" {
		version = domain.CIRVersionV1
	}
	if !domain.SupportsCIRVersion(neg.CIRVersionsSupported, version) && (version != domain.CIRVersionV1 || len(neg.CIRVersionsSupported) > 0) {
		return true, fmt.Errorf("%w: server does not advertise CIR %s support", domain.ErrUnsupportedCIRVersion, version)
	}
	format := doc.Format
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
	manifest := chunkedDocWire{Hash: doc.Hash, Format: format, Envelope: doc.Envelope, Chunks: doc.Chunks}
	if neg.AsyncDocsSupported {
		return true, c.finalizeDocument(ctx, repoID, manifest)
	}
	return true, c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/objects", objectsReq{ChunkedDocs: []chunkedDocWire{manifest}}, nil)
}

func (c *BackendClient) pushRequestedDocChunks(ctx context.Context, repoID string, doc outbound.DocumentChunks, order []domain.ContentHash, wants map[domain.ContentHash]bool) error {
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
		if domain.HashContent(body) != hash {
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
