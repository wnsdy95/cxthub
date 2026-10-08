//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Ownership is checked through the caller's transaction for documents and
// chunks alike, even when a disposable physical proof is already cached.
func (s *PostgresStore) readOwnedDocObject(ctx context.Context, repo domain.ContentHash, kind string, hash domain.ContentHash) ([]byte, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var raw []byte
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
			doc, err := verifyStoredConversation(ctx, hash, manifest, s.ownedDocChunkReader(repo, conversationChunkOrder(manifest)))
			proof = doc.Reference()
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
	return proof, nil
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
			doc, err = verifyStoredConversation(ctx, hash, manifest, s.ownedDocChunkReader(repo, conversationChunkOrder(manifest)))
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
		if body, found := window[hash]; found {
			delete(window, hash)
			return body, nil
		}
		start, found := positions[hash]
		if !found {
			return one(ctx, hash)
		}
		const batchSize = 16
		wanted := order[start:min(start+batchSize, len(order))]
		rows, err := s.db(ctx).Query(ctx, `SELECT rb.hash,b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=ANY($2::text[])`, string(repo), wanted)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		window = make(map[domain.ContentHash][]byte, len(wanted))
		for rows.Next() {
			var id domain.ContentHash
			var body []byte
			if err := rows.Scan(&id, &body); err != nil {
				return nil, err
			}
			window[id] = body
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(window) != len(wanted) {
			return nil, domain.ErrNotFound
		}
		body, found := window[hash]
		if !found {
			return nil, domain.ErrNotFound
		}
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
