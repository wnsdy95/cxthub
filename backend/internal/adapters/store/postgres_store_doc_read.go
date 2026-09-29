//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Reuse only rows from the canonical-event projection namespace. Key-share
// locks keep concurrent last-owner deletion from removing a reused search row
// before this transaction publishes its event locations. No text crosses the
// database connection for an unchanged inherited event.
func retainSearchEventsPG(ctx context.Context, tx pgx.Tx, hashes []domain.ContentHash) (map[domain.ContentHash]bool, error) {
	rows, err := tx.Query(ctx, `SELECT hash FROM doc_search_events_v2 WHERE hash=ANY($1::text[]) ORDER BY hash FOR KEY SHARE`, hashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := make(map[domain.ContentHash]bool)
	for rows.Next() {
		var hash domain.ContentHash
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		known[hash] = true
	}
	return known, rows.Err()
}

func (s *PostgresStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	var idx domain.DocReadIndex
	if err := validateHashes(repo, hash); err != nil {
		return idx, err
	}
	var owned, ready bool
	if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2),EXISTS(SELECT 1 FROM doc_read_index_current WHERE hash=$2)`, string(repo), string(hash)).Scan(&owned, &ready); err != nil {
		return idx, err
	}
	if !owned {
		return idx, domain.ErrNotFound
	}
	if !ready {
		if outbound.DocReadOnly(ctx) {
			return idx, domain.ErrAgentHistoryUnavailable
		}
		doc, err := s.GetDoc(ctx, repo, hash)
		if err != nil {
			return idx, err
		}
		if tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok && tx.readOnly {
			// A pinned reader must not publish a lazy index or repack the archive.
			// Rebuild the verified projection in memory; maintenance can persist it.
			return domain.BuildDocReadIndex(doc)
		}
		// PutDoc validates/repackages legacy manifests and publishes the index in
		// the same document transaction. Concurrent readers serialize on the blob.
		if _, err = s.PutDoc(ctx, repo, doc); err != nil {
			return idx, err
		}
	}
	var env []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT version,envelope FROM doc_read_index_current WHERE hash=$1`, string(hash)).Scan(&idx.Version, &env); err != nil {
		return idx, mapNoRows(err)
	}
	idx.Hash = hash
	if idx.Version != 1 {
		return idx, domain.ErrIntegrity
	}
	if err := json.Unmarshal(env, &idx.Envelope); err != nil {
		return idx, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT ordinal,byte_offset,byte_length,event_hash,seq,role FROM doc_read_event_locations_current WHERE doc_hash=$1 ORDER BY ordinal`, string(hash))
	if err != nil {
		return idx, err
	}
	defer rows.Close()
	idx.Events = []domain.DocEventIndex{}
	for rows.Next() {
		var e domain.DocEventIndex
		if err = rows.Scan(&e.Index, &e.Offset, &e.Length, &e.Hash, &e.Seq, &e.Role); err != nil {
			return idx, err
		}
		idx.Events = append(idx.Events, e)
	}
	return idx, rows.Err()
}

func (s *PostgresStore) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, q string, after, limit int) ([]domain.DocEventIndex, error) {
	if err := validateHashes(repo, hash); err != nil {
		return nil, err
	}
	var owned, ready bool
	if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2), EXISTS(SELECT 1 FROM doc_read_index_current WHERE hash=$2)`, string(repo), string(hash)).Scan(&owned, &ready); err != nil {
		return nil, err
	}
	if !owned {
		return nil, domain.ErrNotFound
	}
	if !ready {
		idx, err := s.DocReadIndex(ctx, repo, hash)
		if err != nil {
			return nil, err
		}
		if tx, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok && tx.readOnly {
			// DocReadIndex intentionally did not persist rows. Query its verified
			// in-memory text instead of incorrectly reporting an empty SQL result.
			out := []domain.DocEventIndex{}
			for i, e := range idx.Events {
				if i%1024 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if len(out) >= limit {
					break
				}
				if e.Index > after && e.Text != "" && strings.Contains(strings.ToLower(e.Text), q) {
					out = append(out, e)
				}
			}
			return out, nil
		}
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	rows, err := s.db(ctx).Query(ctx, `SELECT e.ordinal,e.event_hash,e.seq,e.role,t.search_text FROM doc_read_event_locations_current e
	JOIN doc_search_events_v2 t ON t.hash=e.event_hash
	JOIN repo_blobs rb ON rb.hash=e.doc_hash AND rb.kind='doc' AND rb.repo_id=$1
	WHERE e.doc_hash=$2 AND e.ordinal>$3 AND t.search_lower LIKE $4 ESCAPE '\' ORDER BY e.ordinal LIMIT $5`, string(repo), string(hash), after, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.DocEventIndex{}
	for rows.Next() {
		var e domain.DocEventIndex
		if err = rows.Scan(&e.Index, &e.Hash, &e.Seq, &e.Role, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) BackfillReadIndexes(ctx context.Context, progress func(int)) error {
	// Close the ownership query before writing; do not occupy a connection while
	// reconstructing a large legacy body. The hash order makes runs resumable.
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT ON (rb.hash) rb.repo_id,rb.hash FROM repo_blobs rb LEFT JOIN doc_read_index_current i ON i.hash=rb.hash WHERE rb.kind='doc' AND i.hash IS NULL ORDER BY rb.hash,rb.repo_id`)
	if err != nil {
		return err
	}
	type pair struct{ repo, hash domain.ContentHash }
	var missing []pair
	for rows.Next() {
		var p pair
		if err = rows.Scan(&p.repo, &p.hash); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i, p := range missing {
		if _, err := s.DocReadIndex(ctx, p.repo, p.hash); err != nil {
			return err
		}
		progress(i + 1)
	}
	return nil
}

// MatchingDocHashes evaluates the trigram query once for the whole repository.
// Unindexed legacy documents remain candidates until the backfill completes.
func (s *PostgresStore) MatchingDocHashes(ctx context.Context, repo domain.ContentHash, q string) (map[domain.ContentHash]bool, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	rows, err := s.db(ctx).Query(ctx, `WITH matching AS MATERIALIZED (SELECT hash FROM doc_search_events_v2 WHERE search_lower LIKE $2 ESCAPE '\')
	SELECT DISTINCT e.doc_hash FROM matching m JOIN doc_read_event_locations_current e ON e.event_hash=m.hash JOIN repo_blobs rb ON rb.hash=e.doc_hash AND rb.kind='doc' AND rb.repo_id=$1
	UNION SELECT rb.hash FROM repo_blobs rb LEFT JOIN doc_read_index_current i ON i.hash=rb.hash WHERE rb.repo_id=$1 AND rb.kind='doc' AND i.hash IS NULL`, string(repo), pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.ContentHash]bool{}
	for rows.Next() {
		var h domain.ContentHash
		if err = rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}
