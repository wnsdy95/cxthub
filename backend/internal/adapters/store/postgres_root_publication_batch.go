//go:build postgres

package store

import (
	"context"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	rootPublicationBatchRowsPG  = 64
	rootPublicationBatchBytesPG = 8 << 20
)

func (storage *PostgresStore) retainedRootChunkReader(ctx context.Context, repo domain.ContentHash, chunks []domain.ContentHash) (storedChunkReader, error) {
	reader, _, err := storage.retainedRootChunkReaderWithSource(ctx, repo, chunks, nil)
	return reader, err
}

func (storage *PostgresStore) retainedRootChunkReaderWithSource(ctx context.Context, repo domain.ContentHash, chunks []domain.ContentHash, source *pgRootSourceWitness) (storedChunkReader, bool, error) {
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != storage || bound.readOnly || bound.repo != repo || outbound.DocReadOnly(ctx) {
		return nil, false, domain.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validateHash(repo); err != nil {
		return nil, false, err
	}
	if len(chunks) > domain.MaxConversationManifestChunks {
		return nil, false, domain.ErrDocumentResourceLimit
	}
	order := make([]string, 0, len(chunks))
	positions := make(map[domain.ContentHash]int, len(chunks))
	for _, hash := range chunks {
		if err := validateHash(hash); err != nil {
			return nil, false, err
		}
		if _, exists := positions[hash]; !exists {
			positions[hash] = len(order)
			order = append(order, string(hash))
		}
	}
	lockedOrder := append([]string(nil), order...)
	sort.Strings(lockedOrder)
	if source != nil && len(source.chunks) != len(order) {
		return nil, false, domain.ErrIntegrity
	}
	selection, joined, retained := `SELECT b.hash,octet_length(b.bytes)`, "", "rb,b"
	if source != nil {
		selection += `,b.doc_range_source_version,incarnation.incarnation::text`
		joined = ` JOIN blob_source_incarnations incarnation ON incarnation.hash=b.hash`
		retained += ",incarnation"
	}
	metadata := selection + ` FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash` + joined + `
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=ANY($2::text[])
 ORDER BY b.hash FOR SHARE OF ` + retained
	current := true
	sizes := make(map[domain.ContentHash]int, len(order))
	for start := 0; start < len(lockedOrder); start += rootPublicationBatchRowsPG {
		wanted := lockedOrder[start:min(start+rootPublicationBatchRowsPG, len(lockedOrder))]
		rows, err := bound.Query(ctx, metadata, repo, wanted)
		if err != nil {
			return nil, false, err
		}
		count := 0
		for rows.Next() {
			var hash domain.ContentHash
			var size int
			var version int64
			var incarnation string
			values := []any{&hash, &size}
			if source != nil {
				values = append(values, &version, &incarnation)
			}
			if err := rows.Scan(values...); err != nil {
				rows.Close()
				return nil, false, err
			}
			if count >= len(wanted) || string(hash) != wanted[count] {
				rows.Close()
				return nil, false, domain.ErrNotFound
			}
			if size <= 0 || size > maxStoredRangeChunkBytes {
				rows.Close()
				return nil, false, domain.ErrIntegrity
			}
			if source != nil {
				expected, exists := source.dependency(hash)
				if !exists {
					rows.Close()
					return nil, false, domain.ErrIntegrity
				}
				current = current && expected.version == version && expected.incarnation == incarnation
			}
			sizes[hash] = size
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
		if count != len(wanted) {
			return nil, false, domain.ErrNotFound
		}
	}
	var window map[domain.ContentHash][]byte
	fetched := make(map[domain.ContentHash]bool, len(order))
	return func(read context.Context, hash domain.ContentHash) ([]byte, error) {
		if err := read.Err(); err != nil {
			return nil, err
		}
		if read.Value(repositoryTxKey{}) != bound || outbound.DocReadOnly(read) {
			return nil, domain.ErrConflict
		}
		start, found := positions[hash]
		if !found {
			return nil, domain.ErrIntegrity
		}
		if body, exists := window[hash]; exists {
			return body, nil
		}
		repeated := fetched[hash]
		if !repeated {
			window = nil
		}
		wanted := make([]string, 0, rootPublicationBatchRowsPG)
		budget := 0
		for _, candidate := range order[start:] {
			size := sizes[domain.ContentHash(candidate)]
			if len(wanted) == rootPublicationBatchRowsPG || budget+size > rootPublicationBatchBytesPG {
				break
			}
			if fetched[domain.ContentHash(candidate)] {
				if len(wanted) == 0 {
					wanted = append(wanted, candidate)
				}
				break
			}
			wanted = append(wanted, candidate)
			budget += size
		}
		rows, err := bound.Query(read, `SELECT b.hash,octet_length(b.bytes),
 CASE WHEN octet_length(b.bytes)<=$3 THEN substring(b.bytes FROM 1 FOR $3) END
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=ANY($2::text[])
 ORDER BY b.hash`, repo, wanted, maxStoredRangeChunkBytes)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		next := make(map[domain.ContentHash][]byte, len(wanted))
		total := 0
		for rows.Next() {
			var identity domain.ContentHash
			var size int
			var raw []byte
			if err := rows.Scan(&identity, &size, &raw); err != nil {
				return nil, err
			}
			position, known := positions[identity]
			if !known || position < start || position >= start+len(wanted) || size != sizes[identity] || len(raw) != size {
				return nil, domain.ErrIntegrity
			}
			if _, duplicate := next[identity]; duplicate {
				return nil, domain.ErrIntegrity
			}
			total += size
			if total > rootPublicationBatchBytesPG || len(next) == rootPublicationBatchRowsPG {
				return nil, domain.ErrDocumentResourceLimit
			}
			next[identity] = raw
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if err := read.Err(); err != nil {
			return nil, err
		}
		if len(next) != len(wanted) {
			return nil, domain.ErrNotFound
		}
		for identity := range next {
			fetched[identity] = true
		}
		if !repeated {
			window = next
		}
		return next[hash], nil
	}, current, ctx.Err()
}
