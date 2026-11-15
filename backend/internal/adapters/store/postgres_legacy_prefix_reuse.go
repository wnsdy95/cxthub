//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Retained-history uploads finalize many captures of one long session in
// capture order. Each legacy v2 document repeats almost every chunk of the
// capture before it, but the legacy identity hashes the envelope before the
// events, so the full hash pass cannot resume. Finalization still reads and
// verifies every byte once; the repeated prefix is neither spooled to disk nor
// re-read under the repository lock when an earlier document of the same
// repository, published by this server, already covers it.

// legacyBaseHints remembers, per repository, the last legacy document this
// process published. A hint only nominates a candidate: the base must still be
// owned by the repository, and its stored manifest is re-read every time.
type legacyBaseHints struct {
	mu    sync.Mutex
	bases map[domain.ContentHash]domain.ContentHash
}

const maxLegacyBaseHints = 256

func (hints *legacyBaseHints) get(repo domain.ContentHash) (domain.ContentHash, bool) {
	hints.mu.Lock()
	defer hints.mu.Unlock()
	base, ok := hints.bases[repo]
	return base, ok
}

func (hints *legacyBaseHints) put(repo, doc domain.ContentHash) {
	hints.mu.Lock()
	defer hints.mu.Unlock()
	if hints.bases == nil || len(hints.bases) >= maxLegacyBaseHints {
		hints.bases = map[domain.ContentHash]domain.ContentHash{}
	}
	hints.bases[repo] = doc
}

// legacyPrefixReuse is a base document and how many leading chunks of the job's
// manifest it already lists.
type legacyPrefixReuse struct {
	base   domain.ContentHash
	chunks int
}

// legacyReusablePrefix reads the base's stored manifest inside the caller's
// snapshot. A base that is gone, not chunked, or shares no chunk gives no reuse.
func (storage *PostgresStore) legacyReusablePrefix(ctx context.Context, repo, base domain.ContentHash, chunks []domain.ContentHash) (legacyPrefixReuse, error) {
	var raw []byte
	err := storage.db(ctx).QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $3) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='doc' AND rb.hash=$2`, repo, base, 2*domain.MaxDocJobManifestBytes+1).Scan(&raw)
	if err != nil {
		if err = mapNoRows(err); errors.Is(err, domain.ErrNotFound) {
			return legacyPrefixReuse{}, nil
		}
		return legacyPrefixReuse{}, err
	}
	stored, err := boundedLegacyObject(ctx, raw, domain.MaxDocJobManifestBytes)
	if errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrDocumentResourceLimit) {
		return legacyPrefixReuse{}, nil
	}
	if err != nil {
		return legacyPrefixReuse{}, err
	}
	manifest, ok := domain.ParseDocChunkManifest(stored)
	if !ok || manifest.Format != domain.ChunkFormatV2 {
		return legacyPrefixReuse{}, nil
	}
	shared := 0
	for shared < len(manifest.Chunks) && shared < len(chunks) && manifest.Chunks[shared] == chunks[shared] {
		shared++
	}
	if shared == 0 {
		return legacyPrefixReuse{}, nil
	}
	return legacyPrefixReuse{base: base, chunks: shared}, nil
}

// legacyPrefixSource is the verified stream of one legacy document. Bytes of
// reused prefix chunks are not written to the spool; reads of that range load
// the stored chunk again and check its hash before returning any byte.
type legacyPrefixSource struct {
	spool   *legacyDocSpool
	load    func(context.Context, domain.ContentHash) ([]byte, error)
	extents []legacyPrefixExtent
	skip    int
	size    int
	last    legacyPrefixExtent
	body    []byte
}

type legacyPrefixExtent struct {
	offset, length int
	hash           domain.ContentHash
}

// reuse records that the next chunk of the stream is served from storage. The
// verifier loads a chunk before appending any of its bytes.
func (source *legacyPrefixSource) reuse(hash domain.ContentHash, length int) {
	source.extents = append(source.extents, legacyPrefixExtent{offset: source.skip, length: length, hash: hash})
	source.skip += length
}

func (source *legacyPrefixSource) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case source.size+len(body) <= source.skip:
		source.size += len(body)
		return nil
	case source.size < source.skip:
		// Parts never cross a chunk boundary, and reuse ends on one.
		return domain.ErrIntegrity
	}
	if err := source.spool.Append(ctx, body); err != nil {
		return err
	}
	source.size += len(body)
	return nil
}

func (source *legacyPrefixSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > source.size || length > source.size-offset {
		return nil, domain.ErrIntegrity
	}
	if length > maxLegacyReadBytes {
		return nil, domain.ErrDocumentResourceLimit
	}
	if offset >= source.skip {
		return source.spool.Read(ctx, offset-source.skip, length)
	}
	end := offset + length
	body := make([]byte, 0, length)
	first := sort.Search(len(source.extents), func(i int) bool {
		return source.extents[i].offset+source.extents[i].length > offset
	})
	for _, extent := range source.extents[first:] {
		if extent.offset >= min(end, source.skip) {
			break
		}
		chunk, err := source.chunk(ctx, extent)
		if err != nil {
			return nil, err
		}
		body = append(body, chunk[max(offset, extent.offset)-extent.offset:min(end, extent.offset+extent.length)-extent.offset]...)
	}
	if end > source.skip {
		start := max(offset, source.skip)
		rest, err := source.spool.Read(ctx, start-source.skip, end-start)
		if err != nil {
			return nil, err
		}
		body = append(body, rest...)
	}
	if len(body) != length {
		return nil, domain.ErrIntegrity
	}
	return body, ctx.Err()
}

// chunk keeps the last reused chunk, because read blocks visit the events of a
// chunk one after another.
func (source *legacyPrefixSource) chunk(ctx context.Context, extent legacyPrefixExtent) ([]byte, error) {
	if source.body != nil && source.last == extent {
		return source.body, nil
	}
	body, err := source.load(ctx, extent.hash)
	if err != nil {
		return nil, err
	}
	hash, err := domain.HashContentContext(ctx, body)
	if err != nil {
		return nil, err
	}
	if len(body) != extent.length || hash != extent.hash {
		return nil, domain.ErrIntegrity
	}
	source.last, source.body = extent, bytes.Clone(body)
	return source.body, nil
}

// lockReusedLegacyPrefix runs under the repository lock. The base must still
// be owned, and so must every reused chunk; their bytes are not read again.
// No set means the base is gone, and every chunk is checked as before.
func (publication *pgLegacyStreamingPublication) lockReusedLegacyPrefix(ctx context.Context, tx pgx.Tx) (map[domain.ContentHash]bool, error) {
	reuse := publication.reuse
	if reuse.chunks == 0 {
		return nil, nil
	}
	job := publication.job
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2 FOR SHARE)`, job.RepoID, reuse.base).Scan(&owned); err != nil {
		return nil, err
	}
	if !owned {
		return nil, nil
	}
	reused := make(map[domain.ContentHash]bool, reuse.chunks)
	hashes := make([]domain.ContentHash, 0, reuse.chunks)
	for _, hash := range job.Manifest.Chunks[:reuse.chunks] {
		if !reused[hash] {
			reused[hash] = true
			hashes = append(hashes, hash)
		}
	}
	sort.Slice(hashes, func(left, right int) bool { return hashes[left] < hashes[right] })
	rows, err := tx.Query(ctx, `SELECT rb.hash FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=ANY($2::text[]) ORDER BY rb.hash FOR SHARE OF rb,b`, job.RepoID, hashes)
	if err != nil {
		return nil, err
	}
	found := 0
	for rows.Next() {
		var hash domain.ContentHash
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return nil, err
		}
		if reused[hash] {
			found++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found != len(hashes) {
		return nil, domain.ErrNotFound
	}
	return reused, nil
}
