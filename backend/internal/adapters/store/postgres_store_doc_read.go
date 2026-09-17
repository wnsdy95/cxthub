//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func putReadIndexPG(ctx context.Context, tx pgx.Tx, doc domain.SessionDoc) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_read_indexes WHERE hash=$1)`, string(doc.Hash)).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	idx, err := domain.BuildDocReadIndex(doc)
	if err != nil {
		return err
	}
	env, err := json.Marshal(idx.Envelope)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO doc_read_indexes(hash,version,envelope,event_count) VALUES($1,1,$2,$3)`, string(doc.Hash), env, len(idx.Events)); err != nil {
		return err
	}
	hashes, texts, lower := []string{}, []string{}, []string{}
	for _, e := range idx.Events {
		hashes = append(hashes, string(e.Hash))
		texts = append(texts, e.Text)
		lower = append(lower, strings.ToLower(e.Text))
	}
	// Consistent hash order prevents deadlocks between documents sharing events.
	if _, err = tx.Exec(ctx, `INSERT INTO doc_search_events(hash,search_text,search_lower)
		SELECT DISTINCT ON (hash) hash,body,lower FROM unnest($1::text[],$2::text[],$3::text[]) AS x(hash,body,lower)
		ORDER BY hash ON CONFLICT DO NOTHING`, hashes, texts, lower); err != nil {
		return err
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"doc_read_events"}, []string{"doc_hash", "ordinal", "byte_offset", "byte_length", "event_hash", "seq", "role"}, pgx.CopyFromSlice(len(idx.Events), func(i int) ([]any, error) {
		e := idx.Events[i]
		return []any{string(doc.Hash), i, int64(e.Offset), e.Length, string(e.Hash), e.Seq, e.Role}, nil
	}))
	return err
}

func (s *PostgresStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	var idx domain.DocReadIndex
	if err := validateHashes(repo, hash); err != nil {
		return idx, err
	}
	var owned, ready bool
	if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2),EXISTS(SELECT 1 FROM doc_read_indexes WHERE hash=$2)`, string(repo), string(hash)).Scan(&owned, &ready); err != nil {
		return idx, err
	}
	if !owned {
		return idx, domain.ErrNotFound
	}
	if !ready {
		doc, err := s.GetDoc(ctx, repo, hash)
		if err != nil {
			return idx, err
		}
		// PutDoc validates/repackages legacy manifests and publishes the index in
		// the same document transaction. Concurrent readers serialize on the blob.
		if _, err = s.PutDoc(ctx, repo, doc); err != nil {
			return idx, err
		}
	}
	var env []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT version,envelope FROM doc_read_indexes WHERE hash=$1`, string(hash)).Scan(&idx.Version, &env); err != nil {
		return idx, mapNoRows(err)
	}
	idx.Hash = hash
	if idx.Version != 1 {
		return idx, domain.ErrIntegrity
	}
	if err := json.Unmarshal(env, &idx.Envelope); err != nil {
		return idx, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT ordinal,byte_offset,byte_length,event_hash,seq,role FROM doc_read_events WHERE doc_hash=$1 ORDER BY ordinal`, string(hash))
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
	if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2), EXISTS(SELECT 1 FROM doc_read_indexes WHERE hash=$2)`, string(repo), string(hash)).Scan(&owned, &ready); err != nil {
		return nil, err
	}
	if !owned {
		return nil, domain.ErrNotFound
	}
	if !ready {
		if _, err := s.DocReadIndex(ctx, repo, hash); err != nil {
			return nil, err
		}
	}
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	rows, err := s.db(ctx).Query(ctx, `SELECT e.ordinal,e.event_hash,e.seq,e.role,t.search_text FROM doc_read_events e
	JOIN doc_search_events t ON t.hash=e.event_hash
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
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT ON (rb.hash) rb.repo_id,rb.hash FROM repo_blobs rb LEFT JOIN doc_read_indexes i ON i.hash=rb.hash WHERE rb.kind='doc' AND i.hash IS NULL ORDER BY rb.hash,rb.repo_id`)
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
	rows, err := s.db(ctx).Query(ctx, `WITH matching AS MATERIALIZED (SELECT hash FROM doc_search_events WHERE search_lower LIKE $2 ESCAPE '\')
	SELECT DISTINCT e.doc_hash FROM matching m JOIN doc_read_events e ON e.event_hash=m.hash JOIN repo_blobs rb ON rb.hash=e.doc_hash AND rb.kind='doc' AND rb.repo_id=$1
	UNION SELECT rb.hash FROM repo_blobs rb LEFT JOIN doc_read_indexes i ON i.hash=rb.hash WHERE rb.repo_id=$1 AND rb.kind='doc' AND i.hash IS NULL`, string(repo), pattern)
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
