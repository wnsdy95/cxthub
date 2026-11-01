//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var errRawBlobNeedsExclusive = errors.New("raw blob representation needs exclusive retention")

type rawRepresentationAttempt struct {
	hashes    []domain.ContentHash
	exclusive map[domain.ContentHash]bool
}

func (attempt *rawRepresentationAttempt) order(hashes []domain.ContentHash) []domain.ContentHash {
	ordered := append([]domain.ContentHash(nil), hashes...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	attempt.hashes = ordered
	return ordered
}

func (attempt *rawRepresentationAttempt) mapOrder(bodies map[domain.ContentHash][]byte) []domain.ContentHash {
	hashes := make([]domain.ContentHash, 0, len(bodies))
	for hash := range bodies {
		hashes = append(hashes, hash)
	}
	return attempt.order(hashes)
}

func (attempt *rawRepresentationAttempt) lock(ctx context.Context, transaction pgx.Tx, parent domain.ContentHash, hashes []domain.ContentHash) error {
	attempt.hashes = hashes
	modes := make(map[int32]bool, len(hashes)+1)
	include := func(hash domain.ContentHash, exclusive bool) error {
		if err := validateHash(hash); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(hash))
		key := int32(binary.BigEndian.Uint32(digest[:4]))
		modes[key] = modes[key] || exclusive
		return nil
	}
	for _, hash := range hashes {
		if err := include(hash, attempt.exclusive[hash]); err != nil {
			return err
		}
	}
	if parent != "" {
		if err := include(parent, true); err != nil {
			return err
		}
	}
	keys := make([]int32, 0, len(modes))
	for key := range modes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
	for offset := 0; offset < len(keys); offset += 64 {
		var batch pgx.Batch
		for _, key := range keys[offset:min(offset+64, len(keys))] {
			query := `SELECT pg_advisory_xact_lock_shared($1,$2)`
			if modes[key] {
				query = `SELECT pg_advisory_xact_lock($1,$2)`
			}
			batch.Queue(query, int32(0x43585250), key)
		}
		if err := transaction.SendBatch(ctx, &batch).Close(); err != nil {
			return err
		}
	}
	return nil
}

func canonicalRawDocumentPG(data []byte, hash domain.ContentHash) ([]byte, error) {
	if domain.HashContent(data) == hash {
		return data, nil
	}
	var document domain.CIRDocument
	if json.Unmarshal(data, &document) != nil {
		return nil, domain.ErrIntegrity
	}
	canonical, err := domain.CanonicalBytes(document)
	if err != nil || domain.HashContent(canonical) != hash {
		return nil, domain.ErrIntegrity
	}
	return canonical, nil
}

func (attempt *rawRepresentationAttempt) preflight(ctx context.Context, transaction pgx.Tx) error {
	attempt.exclusive = make(map[domain.ContentHash]bool)
	for _, hash := range attempt.hashes {
		var stored []byte
		if err := transaction.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&stored); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return err
		}
		canonical, err := docDecompress(stored)
		if err != nil {
			return domain.ErrIntegrity
		}
		_, document := domain.ParseDocChunkManifest(canonical)
		_, memory, parseErr := domain.ParseMemoryChunkManifest(canonical)
		if document || memory && parseErr == nil {
			attempt.exclusive[hash] = true
		}
	}
	return nil
}

func rawRepresentationTransaction[Value any](ctx context.Context, storage *PostgresStore, operation func(pgx.Tx, *rawRepresentationAttempt) (Value, error)) (Value, error) {
	var empty Value
	attempt := &rawRepresentationAttempt{}
	for round := 0; round < 2; round++ {
		transaction, err := storage.db(ctx).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return empty, err
		}
		value, err := func() (Value, error) {
			defer rollbackPG(transaction)
			if round > 0 {
				if err := attempt.preflight(ctx, transaction); err != nil {
					return empty, err
				}
			}
			value, err := operation(transaction, attempt)
			if err != nil {
				return empty, err
			}
			if err := storageWriteError(transaction.Commit(ctx)); err != nil {
				return empty, err
			}
			return value, nil
		}()
		if err != nil {
			if errors.Is(err, errRawBlobNeedsExclusive) {
				if round == 0 {
					continue
				}
				return empty, domain.ErrConflict
			}
			return empty, err
		}
		return value, nil
	}
	return empty, domain.ErrConflict
}

func preserveRawBlobPG(ctx context.Context, transaction pgx.Tx, hash domain.ContentHash, replacement []byte) error {
	var pinned bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs
 WHERE hash=$1 AND kind IN ('chunk','memory_chunk'))`, hash).Scan(&pinned); err != nil {
		return err
	}
	if pinned {
		return nil
	}
	_, err := transaction.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, replacement, hash)
	return err
}

func (storage *PostgresStore) retainRawBlobPG(ctx context.Context, transaction pgx.Tx, hash domain.ContentHash, body []byte, allowInsert, exclusive bool) error {
	if domain.HashContent(body) != hash {
		return domain.ErrIntegrity
	}
	query := `SELECT bytes FROM blobs WHERE hash=$1 FOR SHARE`
	if exclusive {
		query = `SELECT bytes FROM blobs WHERE hash=$1 FOR UPDATE`
	}
	var stored []byte
	read := func() error { return transaction.QueryRow(ctx, query, hash).Scan(&stored) }
	err := read()
	if errors.Is(err, pgx.ErrNoRows) {
		if !allowInsert {
			return domain.ErrIntegrity
		}
		inserted, insertErr := transaction.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT(hash) DO NOTHING`, hash, docCompress(body))
		if insertErr != nil {
			return insertErr
		}
		if inserted.RowsAffected() == 1 {
			return nil
		}
		err = read()
	}
	if err != nil {
		return err
	}
	stored, err = docDecompress(stored)
	if err != nil {
		return domain.ErrIntegrity
	}
	if bytes.Equal(stored, body) {
		return nil
	}
	_, document := domain.ParseDocChunkManifest(stored)
	_, memory, parseErr := domain.ParseMemoryChunkManifest(stored)
	if !document && (!memory || parseErr != nil) {
		return domain.ErrIntegrity
	}
	if !exclusive {
		return errRawBlobNeedsExclusive
	}
	var canonical []byte
	if document {
		canonical, _, err = storage.reassembleManifestTx(ctx, transaction, stored, hash)
	} else {
		var digest domain.MemoryDigest
		digest, _, err = storage.reassembleMemoryManifestTx(ctx, transaction, stored)
		if err == nil {
			canonical, err = json.Marshal(digest)
		}
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrNotFound) {
			return domain.ErrIntegrity
		}
		return err
	}
	if domain.HashContent(canonical) != hash || !bytes.Equal(canonical, body) {
		return domain.ErrIntegrity
	}
	_, err = transaction.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, docCompress(body), hash)
	return err
}
