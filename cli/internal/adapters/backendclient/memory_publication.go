package backendclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
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
	if snap.RepoID != repoID || snap.ID != root.SnapshotID || snap.DocHash != doc.Hash || root.PreviousMemoryHash != "" {
		return domain.ErrHashMismatch
	}
	if err := root.ValidateMemoryClaims(); err != nil {
		return err
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		return err
	}
	if domain.HashContent(raw) != doc.Hash {
		return domain.ErrHashMismatch
	}
	want, err := domain.MemoryDigestHash(root)
	if err != nil {
		return err
	}
	snap = snapshotForCreate(snap)
	snap.MemoryHash = ""
	objects := objectsReq{Snapshots: []domain.Snapshot{snap}}
	plan, chunked := chunkcas.PlanDoc(raw)
	if chunked {
		var neg negotiateResp
		if err := c.do(ctx, http.MethodPost, c.reposPath(repoID)+"/push/negotiate", negotiateReq{ChunkHaves: plan.Order}, &neg); err != nil {
			return err
		}
		if !neg.BoundedChunksSupported || !containsString(neg.ChunkFormatsSupported, plan.Manifest.Format) {
			return fmt.Errorf("remote does not support bounded memory archive publication")
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
		objects.ChunkedDocs = []chunkedDocWire{{Hash: doc.Hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}}
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
