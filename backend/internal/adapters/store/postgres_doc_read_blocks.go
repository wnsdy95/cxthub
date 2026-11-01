//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
	quota, err := newTransientSpoolAllocation()
	if err != nil {
		return err
	}
	spool, err := os.CreateTemp("", "cxthub-search-events-*")
	if err != nil {
		quota.Close()
		return err
	}
	defer func() {
		if err := os.Remove(spool.Name()); err == nil || os.IsNotExist(err) {
			quota.Close()
		}
	}()
	defer spool.Close()
	buffer := make([]byte, 32<<10)
	writer := readBlockSearchWriterPG{writer: spool, quota: quota}
	textHash := sha256.New()
	indexes := make([]domain.DocReadIndex, len(blocks))
	var entries []readBlockSearchEntryPG
	var offset int64
	for i, b := range blocks {
		idx := domain.DocReadIndex{}
		err := b.VisitContext(ctx, known, func(event domain.DocEventIndex) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !known[event.Hash] {
				textHash.Reset()
				written, err := io.CopyBuffer(io.MultiWriter(writer, textHash), readBlockSearchReaderPG{ctx, strings.NewReader(event.Text)}, buffer)
				if err != nil {
					return err
				}
				var checksum [sha256.Size]byte
				textHash.Sum(checksum[:0])
				entries = append(entries, readBlockSearchEntryPG{hash: string(event.Hash), offset: offset, length: written, checksum: checksum})
				offset += written
				known[event.Hash] = true
			}
			event.Text = ""
			idx.Events = append(idx.Events, event)
			return nil
		})
		if err != nil {
			return err
		}
		indexes[i] = idx
	}
	if err = putReadBlockSearchPG(ctx, tx, spool, entries); err != nil {
		return err
	}
	block, event := 0, 0
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"doc_read_block_events_v3"}, []string{"block_hash", "ordinal", "byte_offset", "byte_length", "event_hash", "seq", "role"}, pgx.CopyFromFunc(func() ([]any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

const readBlockSearchBatchBytesPG = 4 << 20
const readBlockSearchBatchRowsPG = 1024

type readBlockSearchEntryPG struct {
	hash     string
	offset   int64
	length   int64
	checksum [sha256.Size]byte
}

type readBlockSearchWriterPG struct {
	writer io.Writer
	quota  *transientSpoolAllocation
}

func (writer readBlockSearchWriterPG) Write(body []byte) (int, error) {
	if err := writer.quota.Reserve(int64(len(body))); err != nil {
		return 0, err
	}
	written, err := writer.writer.Write(body)
	if written < len(body) {
		writer.quota.Release(int64(len(body) - written))
		if err == nil {
			err = io.ErrShortWrite
		}
	}
	return written, err
}

type readBlockSearchReaderPG struct {
	ctx    context.Context
	reader io.Reader
}

func (reader readBlockSearchReaderPG) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(body)
}

func putReadBlockSearchPG(ctx context.Context, tx pgx.Tx, spool io.ReaderAt, entries []readBlockSearchEntryPG) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].hash < entries[right].hash })
	buffer := make([]byte, 32<<10)
	textHash := sha256.New()
	var hashes, texts, lower []string
	batchBytes := 0
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(hashes) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO doc_search_events_v2(hash,search_text,search_lower)
 SELECT DISTINCT ON (hash) hash,body,lower FROM unnest($1::text[],$2::text[],$3::text[]) AS x(hash,body,lower)
 ORDER BY hash ON CONFLICT DO NOTHING`, hashes, texts, lower); err != nil {
			return err
		}
		clear(hashes)
		clear(texts)
		clear(lower)
		hashes, texts, lower = hashes[:0], texts[:0], lower[:0]
		batchBytes = 0
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		var text strings.Builder
		text.Grow(int(entry.length))
		textHash.Reset()
		written, err := io.CopyBuffer(io.MultiWriter(&text, textHash), readBlockSearchReaderPG{ctx, io.NewSectionReader(spool, entry.offset, entry.length)}, buffer)
		if err != nil {
			return err
		}
		if written != entry.length {
			return io.ErrUnexpectedEOF
		}
		var checksum [sha256.Size]byte
		textHash.Sum(checksum[:0])
		if checksum != entry.checksum {
			return domain.ErrIntegrity
		}
		body := text.String()
		folded := strings.ToLower(body)
		entryBytes := len(entry.hash) + len(body) + len(folded)
		if batchBytes+entryBytes > readBlockSearchBatchBytesPG || len(hashes) == readBlockSearchBatchRowsPG {
			if err := flush(); err != nil {
				return err
			}
		}
		hashes = append(hashes, entry.hash)
		texts = append(texts, body)
		lower = append(lower, folded)
		batchBytes += entryBytes
		if batchBytes >= readBlockSearchBatchBytesPG {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
