//go:build postgres

package store

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func putReadIndexPG(ctx context.Context, tx pgx.Tx, doc domain.VerifiedSessionDoc) error {
	plan, err := prepareReadIndexPG(ctx, doc)
	if err != nil {
		return err
	}
	return putPreparedReadIndexPG(ctx, tx, plan)
}

func putPreparedReadIndexPG(ctx context.Context, tx pgx.Tx, plan preparedReadIndexPG) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_read_indexes_v3 WHERE hash=$1)`, plan.hash).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	blocks := plan.blocks
	if err := putPreparedReadBlocksPG(ctx, tx, blocks); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO doc_read_indexes_v3(hash,version,envelope,event_count) VALUES($1,1,$2,$3)`, plan.hash, plan.envelope, plan.eventCount); err != nil {
		return err
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"doc_read_block_locations_v3"}, []string{"doc_hash", "first_event", "byte_offset", "block_hash"}, pgx.CopyFromSlice(len(blocks), func(i int) ([]any, error) {
		b := blocks[i]
		return []any{string(plan.hash), b.FirstEvent, int64(b.Offset), string(b.Hash)}, nil
	}))
	return err
}

// Shared derivatives can be prepared without granting document ownership. Both
// staging and final publication use the same ordered retention/insertion path.
func putPreparedReadBlocksPG(ctx context.Context, tx pgx.Tx, blocks []domain.DocReadBlock) error {
	// Retain existing shared blocks until this document's references commit.
	// Hash ordering also orders competing insertion of previously unseen blocks.
	hashes := make([]domain.ContentHash, len(blocks))
	byHash := make(map[domain.ContentHash]domain.DocReadBlock, len(blocks))
	for i, b := range blocks {
		hashes[i] = b.Hash
		byHash[b.Hash] = b
	}
	rows, err := tx.Query(ctx, `SELECT hash,event_count FROM doc_read_blocks_v3 WHERE hash=ANY($1::text[]) ORDER BY hash FOR KEY SHARE`, hashes)
	if err != nil {
		return err
	}
	known := map[domain.ContentHash]bool{}
	for rows.Next() {
		var h domain.ContentHash
		var count int
		if err = rows.Scan(&h, &count); err != nil {
			rows.Close()
			return err
		}
		if count != byHash[h].Count {
			rows.Close()
			return domain.ErrIntegrity
		}
		known[h] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var fresh []domain.DocReadBlock
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	for _, hash := range hashes {
		if known[hash] {
			continue
		}
		b := byHash[hash]
		ct, err := tx.Exec(ctx, `INSERT INTO doc_read_blocks_v3(hash,event_count) VALUES($1,$2) ON CONFLICT DO NOTHING`, hash, b.Count)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			// Another transaction published the same exact block. Its FK parent
			// stays pinned even if that writer immediately loses its last document.
			var count int
			if err = tx.QueryRow(ctx, `SELECT event_count FROM doc_read_blocks_v3 WHERE hash=$1 FOR KEY SHARE`, hash).Scan(&count); err != nil {
				return err
			}
			if count != b.Count {
				return domain.ErrIntegrity
			}
		} else {
			fresh = append(fresh, b)
		}
		known[hash] = true
	}
	if err = putReadBlockEventsPG(ctx, tx, fresh); err != nil {
		return err
	}
	return nil
}

// Acquire all missing blocks before search rows, then acquire/insert search
// identities in global hash order across the whole document. Per-block ordering
// alone can deadlock concurrent documents with differently grouped shared events.
func putReadBlockEventsPG(ctx context.Context, tx pgx.Tx, blocks []domain.DocReadBlock) error {
	if len(blocks) == 0 {
		return nil
	}
	var eventHashes []domain.ContentHash
	for _, b := range blocks {
		eventHashes = append(eventHashes, b.EventHashes()...)
	}
	known, err := retainSearchEventsPG(ctx, tx, eventHashes)
	if err != nil {
		return err
	}
	indexes := make([]domain.DocReadIndex, len(blocks))
	hashes, texts, lower := []string{}, []string{}, []string{}
	for i, b := range blocks {
		idx, err := b.Build(known)
		if err != nil {
			return err
		}
		indexes[i] = idx
		for _, e := range idx.Events {
			if known[e.Hash] {
				continue
			}
			known[e.Hash] = true
			hashes = append(hashes, string(e.Hash))
			texts = append(texts, e.Text)
			lower = append(lower, strings.ToLower(e.Text))
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO doc_search_events_v2(hash,search_text,search_lower)
 SELECT DISTINCT ON (hash) hash,body,lower FROM unnest($1::text[],$2::text[],$3::text[]) AS x(hash,body,lower)
 ORDER BY hash ON CONFLICT DO NOTHING`, hashes, texts, lower); err != nil {
		return err
	}
	block, event := 0, 0
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"doc_read_block_events_v3"}, []string{"block_hash", "ordinal", "byte_offset", "byte_length", "event_hash", "seq", "role"}, pgx.CopyFromFunc(func() ([]any, error) {
		for block < len(blocks) && event == len(indexes[block].Events) {
			block++
			event = 0
		}
		if block == len(blocks) {
			return nil, nil
		}
		e := indexes[block].Events[event]
		event++
		return []any{string(blocks[block].Hash), e.Index, int64(e.Offset), e.Length, string(e.Hash), e.Seq, e.Role}, nil
	}))
	return err
}
