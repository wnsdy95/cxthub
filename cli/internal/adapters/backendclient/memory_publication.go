package backendclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// PublishMemoryArchive stages bounded immutable chunks, then creates archive
// ownership and the first memory attachment in one server transaction. The new
// endpoint is a protocol gate: never downgrade to a non-atomic old-server path.
func (c *BackendClient) PublishMemoryArchive(ctx context.Context, repoID string, snap domain.Snapshot, doc domain.SessionDoc, root domain.MemoryDigest) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if err := validateSnapshotObject(snap); err != nil {
		return err
	}
	if snap.RepoID != repoID || snap.ID != root.SnapshotID || snap.DocumentRef() != doc.DocumentRef() || root.PreviousMemoryHash != "" {
		return domain.ErrHashMismatch
	}
	if err := root.ValidateMemoryClaims(); err != nil {
		return err
	}
	if err := doc.DocumentRef().Validate(); err != nil {
		return err
	}
	isRoot := doc.Identity == domain.DocumentIdentityRootV1
	var raw []byte
	if isRoot {
		if err := c.PreflightDocumentReferences(ctx, repoID, []domain.DocumentRef{doc.DocumentRef()}); err != nil {
			return err
		}
	} else {
		var err error
		raw, err = domain.CanonicalBytes(doc.CIR)
		if err != nil {
			return err
		}
		if domain.HashContent(raw) != doc.Hash {
			return domain.ErrHashMismatch
		}
	}
	want, err := domain.MemoryDigestHash(root)
	if err != nil {
		return err
	}
	snap = snapshotForCreate(snap)
	snap.MemoryHash = ""
	objects := objectsReq{Snapshots: []domain.Snapshot{snap}}
	plan, chunked := chunkcas.PlanDoc(raw)
	if isRoot {
		source, ok := c.chunks.(outbound.ChunkedDocumentStore)
		if !ok {
			return domain.ErrUnsupportedDocumentIdentity
		}
		var neg negotiateResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/negotiate", negotiateReq{DocHaves: []domain.ContentHash{doc.Hash}}, &neg); err != nil {
			return err
		}
		if !neg.PreparedMemoryArchivesSupported || !neg.AsyncDocsSupported || !supportsRootIdentity(neg.DocIdentitiesSupported) {
			return domain.ErrUnsupportedDocumentIdentity
		}
		if err := validateNegotiatedSubset([]domain.ContentHash{doc.Hash}, neg.DocWants); err != nil {
			return err
		}
		if len(neg.SnapshotWants) != 0 || len(neg.ChunkWants) != 0 {
			return domain.ErrHashMismatch
		}
		if len(neg.DocWants) != 0 && !neg.RootPublicationEnabled {
			return domain.ErrUnsupportedDocumentIdentity
		}
		supported, err := source.WithVerifiedDocChunks(ctx, doc.DocumentRef(), func(chunks outbound.DocumentChunks) error {
			if chunks.Representation.DocumentRef() != doc.DocumentRef() {
				return domain.ErrHashMismatch
			}
			accepted, err := c.PushDocChunks(ctx, repoID, chunks)
			if err != nil {
				return err
			}
			if !accepted {
				return domain.ErrUnsupportedDocumentIdentity
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !supported {
			return domain.ErrUnsupportedDocumentIdentity
		}
	} else if chunked {
		var neg negotiateResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/negotiate", negotiateReq{ChunkHaves: plan.Order}, &neg); err != nil {
			return err
		}
		if !neg.BoundedChunksSupported || !containsString(neg.ChunkFormatsSupported, plan.Manifest.Format) {
			return fmt.Errorf("remote does not support bounded memory archive publication")
		}
		if !neg.AsyncDocsSupported || !neg.PreparedMemoryArchivesSupported {
			return fmt.Errorf("remote does not support prepared memory archives; upgrade the server before retrying")
		}
		seen := map[domain.ContentHash]bool{}
		var chunks []chunkObjWire
		for _, hash := range neg.ChunkWants {
			body, ok := plan.Bodies[hash]
			if !ok || seen[hash] {
				return domain.ErrHashMismatch
			}
			seen[hash] = true
			chunks = append(chunks, chunkObjWire{Hash: hash, Data: body})
		}
		if err := c.pushChunkBatches(ctx, repoID, chunks); err != nil {
			return err
		}
		if err := c.finalizeDocument(ctx, repoID, chunkedDocWire{Hash: doc.Hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}); err != nil {
			return err
		}
		// Only metadata and memory enter the final transaction. Replaying a full
		// manifest here would redo the expensive validation inside the HTTP limit.
	} else {
		objects.Docs = []domain.SessionDoc{doc}
	}
	body := struct {
		Objects objectsReq          `json:"objects"`
		Memory  domain.MemoryDigest `json:"memory"`
	}{objects, root}
	// Keep the final transaction within the dedicated 32 MiB ingress limit.
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(encoded) > 32<<20 {
		return fmt.Errorf("memory archive publication exceeds 32 MiB")
	}
	var out struct {
		MemoryHash domain.ContentHash `json:"memory_hash"`
	}
	if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/memory-publications", body, &out); err != nil {
		return err
	}
	if out.MemoryHash != want {
		return domain.ErrHashMismatch
	}
	return nil
}
