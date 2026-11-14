//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	ownedDocChunkBytesPG    = 2 * domain.MaxLegacyStreamEventBytes
	ownedDocPrefetchBytesPG = 8 << 20
)

// Ownership is checked through the caller's transaction for documents and
// chunks alike, even when a disposable physical proof is already cached.
func (s *PostgresStore) readOwnedDocObject(ctx context.Context, repo domain.ContentHash, kind string, hash domain.ContentHash) ([]byte, error) {
	return s.readOwnedDocObjectBounded(ctx, repo, kind, hash, ownedDocChunkBytesPG)
}

func (s *PostgresStore) readOwnedDocObjectBounded(ctx context.Context, repo domain.ContentHash, kind string, hash domain.ContentHash, limit int) ([]byte, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var raw []byte
	if kind == "chunk" {
		if limit <= 0 || limit > ownedDocChunkBytesPG {
			return nil, domain.ErrIntegrity
		}
		var size int
		if err := s.db(ctx).QueryRow(ctx, `SELECT octet_length(b.bytes),
 CASE WHEN octet_length(b.bytes)<=$4 THEN substring(b.bytes FROM 1 FOR $4) END
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3`, string(repo), kind, string(hash), limit).Scan(&size, &raw); err != nil {
			return nil, mapNoRows(err)
		}
		if size > limit {
			return nil, domain.ErrDocumentResourceLimit
		}
		if len(raw) != size {
			return nil, domain.ErrIntegrity
		}
		return raw, ctx.Err()
	}
	if err := s.db(ctx).QueryRow(ctx, `SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind=$2 AND rb.hash=$3`, string(repo), kind, string(hash)).Scan(&raw); err != nil {
		return nil, mapNoRows(err)
	}
	return raw, nil
}

func (s *PostgresStore) readDocBytes(ctx context.Context, repo, hash domain.ContentHash) ([]byte, bool, error) {
	raw, err := s.readOwnedDocObject(ctx, repo, "doc", hash)
	if err != nil {
		return nil, false, err
	}
	if err := rejectStoredRoot(ctx, raw); err != nil {
		return nil, false, err
	}
	data, err := docDecompress(raw)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if body, chunked, err := s.getDocChunkedPG(ctx, repo, hash, data); chunked {
		return body, true, err
	}
	return data, false, nil
}
func (s *PostgresStore) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (proof domain.VerifiedDocReference, err error) {
	err = s.WithinReadSnapshot(ctx, func(ctx context.Context) error {
		raw, err := s.readOwnedDocObject(ctx, repo, "doc", hash)
		if err != nil {
			return err
		}
		manifest, root, err := storedConversationManifest(ctx, raw)
		if err != nil {
			return err
		}
		if root {
			proof, err = verifyStoredConversationReference(ctx, &s.docProofs.rootVerifier, hash, manifest, s.ownedDocChunkReader(repo, conversationChunkOrder(manifest)))
			return err
		}
		proof, err = s.docProofs.verifyStoredWithLoader(ctx, repo, hash, raw, func(man domain.DocChunkManifest) storedChunkReader {
			return s.ownedDocChunkReader(repo, man.Chunks)
		})
		return err
	})
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	s.rememberHistoryRootReference(ctx, repo, proof)
	return proof, nil
}

// FinalizedDocReference implements outbound.FinalizedDocAttestor for legacy
// v2 documents and conversation roots. It recomputes the finalization job
// identity from the manifest that is stored now, so a completed job for another
// representation of the same hash does not match, and it requires the
// repository to still own the document object and every chunk the manifest
// names. No chunk body is read. Anything that does not match, including a
// document the repository does not own, is reported as not found, so the byte
// verifier remains the only place that refuses a document. Inside history
// preparation an attested root is remembered like a byte-verified one.
func (s *PostgresStore) FinalizedDocReference(ctx context.Context, repo, hash domain.ContentHash) (proof domain.VerifiedDocReference, found bool, err error) {
	err = s.WithinReadSnapshot(ctx, func(ctx context.Context) error {
		raw, err := s.readOwnedDocObject(ctx, repo, "doc", hash)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		id, matches, chunkList, ok := finalizedDocJobKey(ctx, repo, hash, raw)
		if !ok {
			return nil
		}
		job, err := s.GetDocJob(ctx, repo, id)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if job.State != "completed" || job.DocHash != hash || !matches(job) {
			return nil
		}
		seen := make(map[domain.ContentHash]bool, len(chunkList))
		chunks := make([]string, 0, len(chunkList))
		for _, chunk := range chunkList {
			if !seen[chunk] {
				seen[chunk] = true
				chunks = append(chunks, string(chunk))
			}
		}
		var unowned bool
		if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM unnest($2::text[]) AS wanted(hash)
 WHERE NOT EXISTS (SELECT 1 FROM repo_blobs owned JOIN blobs body ON body.hash=owned.hash
  WHERE owned.repo_id=$1 AND owned.kind='chunk' AND owned.hash=wanted.hash))`, string(repo), chunks).Scan(&unowned); err != nil {
			return err
		}
		if unowned {
			return nil
		}
		proof, err = domain.FinalizedDocReference(job)
		found = err == nil
		return err
	})
	if err != nil {
		return domain.VerifiedDocReference{}, false, err
	}
	if found {
		// History preparation reuses a root reference established in its own
		// read transaction instead of reading the root again, whether bytes or
		// this record established it.
		s.rememberHistoryRootReference(ctx, repo, proof)
	}
	return proof, found, nil
}

// finalizedDocJobKey derives, from the stored document object alone, the
// finalization job identity that would have published exactly that object, a
// check that the job record carries the same manifest, and the chunks the
// manifest names. ok is false for anything that is neither a legacy v2 chunk
// manifest nor a canonical conversation root.
func finalizedDocJobKey(ctx context.Context, repo, hash domain.ContentHash, raw []byte) (id string, matches func(domain.DocFinalizationJob) bool, chunks []domain.ContentHash, ok bool) {
	root, isRoot, err := storedConversationManifest(ctx, raw)
	if err != nil {
		return "", nil, nil, false
	}
	if isRoot {
		canonical, err := domain.CanonicalConversationManifest(root)
		if err != nil {
			return "", nil, nil, false
		}
		id, err := domain.RootDocFinalizationID(repo, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: canonical})
		if err != nil {
			return "", nil, nil, false
		}
		return id, func(job domain.DocFinalizationJob) bool {
			return job.DocIdentity == domain.DocumentIdentityRootV1 && bytes.Equal(job.RootManifest, canonical)
		}, conversationChunkOrder(root), true
	}
	data, err := docDecompress(raw)
	if err != nil {
		return "", nil, nil, false
	}
	manifest, chunked := domain.ParseDocChunkManifest(data)
	if !chunked || manifest.Format != domain.ChunkFormatV2 {
		return "", nil, nil, false
	}
	wanted, err := domain.NewDocFinalizationJob(repo, hash, manifest, time.Unix(0, 0).UTC())
	if err != nil {
		return "", nil, nil, false
	}
	return wanted.ID, func(job domain.DocFinalizationJob) bool {
		return job.DocIdentity == domain.DocumentIdentityLegacy && sameDocManifest(job.Manifest, manifest)
	}, manifest.Chunks, true
}

func (s *PostgresStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (doc domain.VerifiedSessionDoc, err error) {
	err = s.WithinReadSnapshot(ctx, func(ctx context.Context) error {
		raw, err := s.readOwnedDocObject(ctx, repo, "doc", hash)
		if err != nil {
			return err
		}
		manifest, root, err := storedConversationManifest(ctx, raw)
		if err != nil {
			return err
		}
		if root {
			doc, err = s.docProofs.verifyConversation(ctx, hash, manifest, s.ownedDocChunkReader(repo, conversationChunkOrder(manifest)))
		} else {
			// Legacy representations retain their existing byte/semantic rules.
			doc, err = verifyLegacyStoredDoc(ctx, hash, raw, func(ctx context.Context, h domain.ContentHash) ([]byte, error) {
				return s.readOwnedDocObject(ctx, repo, "chunk", h)
			})
		}
		return err
	})
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	return doc, nil
}

func conversationChunkOrder(manifest domain.ConversationManifest) []domain.ContentHash {
	order := make([]domain.ContentHash, len(manifest.Chunks))
	for i, chunk := range manifest.Chunks {
		order[i] = chunk.Hash
	}
	return order
}

// Prefetch a bounded window through the caller's transaction. These are current
// owned bytes, not existence receipts or a cross-request body cache. The proof
// verifier still hashes every distinct body and validates exactly those bytes.
func (s *PostgresStore) ownedDocChunkReader(repo domain.ContentHash, chunks []domain.ContentHash) storedChunkReader {
	one := func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return s.readOwnedDocObject(ctx, repo, "chunk", hash)
	}
	if len(chunks) > domain.MaxDocJobChunks {
		return one // Preserve the unbounded legacy reader's accepted inputs.
	}
	order := make([]string, 0, len(chunks))
	positions := make(map[domain.ContentHash]int, len(chunks))
	for _, hash := range chunks {
		if domain.ValidateContentHash(hash) != nil {
			return one
		}
		if _, seen := positions[hash]; !seen {
			positions[hash] = len(order)
			order = append(order, string(hash))
		}
	}
	var window map[domain.ContentHash][]byte
	return func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateHashes(repo, hash); err != nil {
			return nil, err
		}
		if body, found := window[hash]; found {
			delete(window, hash)
			return body, nil
		}
		window = nil
		start, found := positions[hash]
		if !found {
			return one(ctx, hash)
		}
		wanted := order[start:]
		rows, err := s.db(ctx).Query(ctx, `WITH sizes AS MATERIALIZED (
 SELECT wanted.hash,wanted.ordinal,octet_length(b.bytes) AS size
 FROM unnest($2::text[]) WITH ORDINALITY AS wanted(hash,ordinal)
 LEFT JOIN repo_blobs rb ON rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=wanted.hash
 LEFT JOIN blobs b ON b.hash=rb.hash
), selected AS MATERIALIZED (
 SELECT hash,ordinal,size FROM (
  SELECT sizes.*,sum(COALESCE(size,0)) OVER (ORDER BY ordinal) AS total_bytes FROM sizes
 ) budgeted WHERE ordinal=1 OR total_bytes<=$4
)
SELECT selected.hash,selected.size,
 CASE WHEN selected.size<=$3 THEN substring(b.bytes FROM 1 FOR $3) END
 FROM selected LEFT JOIN blobs b ON b.hash=selected.hash AND selected.size IS NOT NULL
 ORDER BY selected.ordinal`, string(repo), wanted, ownedDocChunkBytesPG, ownedDocPrefetchBytesPG)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		nextWindow := make(map[domain.ContentHash][]byte)
		windowBytes := 0
		for rows.Next() {
			var id domain.ContentHash
			var size *int
			var body []byte
			if err := rows.Scan(&id, &size, &body); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if size == nil {
				return nil, domain.ErrNotFound
			}
			if *size > ownedDocChunkBytesPG {
				return nil, domain.ErrDocumentResourceLimit
			}
			if len(nextWindow) >= len(wanted) || string(id) != wanted[len(nextWindow)] || len(body) != *size {
				return nil, domain.ErrIntegrity
			}
			windowBytes += len(body)
			if len(nextWindow) > 0 && windowBytes > ownedDocPrefetchBytesPG {
				return nil, domain.ErrDocumentResourceLimit
			}
			nextWindow[id] = body
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, found := nextWindow[hash]
		if !found {
			return nil, domain.ErrNotFound
		}
		window = nextWindow
		delete(window, hash)
		return body, ctx.Err()
	}
}

var _ outbound.StoredDocVerifier = (*PostgresStore)(nil)
var _ outbound.VerifiedDocReader = (*PostgresStore)(nil)

func (s *PostgresStore) CaptureSupersedes(ctx context.Context, repo, old, next domain.ContentHash, provider domain.ProviderKind, session string) (bool, error) {
	return compareStoredCaptures(ctx, s, &s.docProofs, repo, old, next, provider, session)
}

var _ outbound.StoredCaptureComparator = (*PostgresStore)(nil)
