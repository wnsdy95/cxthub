//go:build postgres

package store

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const memoryReadRangeChunkLimit = 51
const memoryReadRangeByteLimit = 3 * domain.MaxMemoryReadChunkBytes

const memoryReadRangeChunkSQL = `SELECT CASE WHEN octet_length(chunk_hash)=71 THEN chunk_hash END,
	start_item,item_count,octet_length(body),CASE WHEN octet_length(body)<=$4 THEN body END
	FROM memory_read_chunks WHERE repo_id=$1 AND read_id=$2 AND ordinal=$3`

type memoryReadRangeKey struct{}

type memoryReadRange struct {
	mutex       sync.Mutex
	active      bool
	context     context.Context
	transaction pgx.Tx
	repo        domain.ContentHash
	identifier  domain.ContentHash
	references  []domain.MemoryReadChunkRef
	verifier    *domain.MemoryReadChunkVerifier
	reads       int
	bytes       int
	err         error
}

func (storage *PostgresStore) WithMemoryReadRange(ctx context.Context, repo, identifier domain.ContentHash, consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	if consume == nil {
		return domain.ErrValidation
	}
	transaction, owned, err := storage.memoryReadSnapshot(ctx, repo, identifier)
	if err != nil {
		return err
	}
	if owned {
		defer rollbackPG(transaction)
	}
	header, verifier, err := readMemoryReadDescriptor(ctx, transaction, repo, identifier)
	if err != nil {
		return err
	}
	if err := consumeMemoryReadRange(ctx, transaction, repo, identifier, header, verifier, consume); err != nil {
		return err
	}
	if owned {
		return transaction.Commit(ctx)
	}
	return nil
}

func consumeMemoryReadRange(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash, header domain.MemoryReadHeader, verifier *domain.MemoryReadChunkVerifier, consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	bound, cancel := context.WithCancel(ctx)
	scope := &memoryReadRange{active: true, context: bound, transaction: transaction, repo: repo, identifier: identifier,
		references: slices.Clone(header.Chunks), verifier: verifier}
	defer func() {
		cancel()
		scope.close()
	}()
	err := consume(context.WithValue(bound, memoryReadRangeKey{}, scope), header, scope)
	cancel()
	readErr := scope.close()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, header.CaptureID); err != nil {
		return err
	}
	return nil
}

func (scope *memoryReadRange) close() error {
	scope.mutex.Lock()
	defer scope.mutex.Unlock()
	scope.active = false
	return scope.err
}

func (scope *memoryReadRange) ReadChunk(ctx context.Context, ordinal int) (items []domain.EffectiveMemoryItem, err error) {
	scope.mutex.Lock()
	defer scope.mutex.Unlock()
	defer func() {
		if err != nil && scope.err == nil {
			scope.err = err
		}
	}()
	if !scope.active || ctx.Value(memoryReadRangeKey{}) != scope {
		return nil, domain.ErrConflict
	}
	if scope.err != nil {
		return nil, scope.err
	}
	if err := scope.context.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ordinal < 0 || ordinal >= len(scope.references) {
		return nil, domain.ErrValidation
	}
	reference := scope.references[ordinal]
	if scope.reads == memoryReadRangeChunkLimit || reference.Bytes > memoryReadRangeByteLimit-scope.bytes {
		return nil, domain.ErrMemoryProjectionLimit
	}
	scope.reads++
	readContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(scope.context, cancel)
	defer func() {
		stop()
		cancel()
	}()
	var digest *string
	var start, count, size int64
	var raw []byte
	if err := scope.transaction.QueryRow(readContext, memoryReadRangeChunkSQL, string(scope.repo), string(scope.identifier), ordinal,
		min(domain.MaxMemoryReadChunkBytes, memoryReadRangeByteLimit-scope.bytes)).Scan(&digest, &start, &count, &size, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIntegrity
		}
		return nil, err
	}
	if size > domain.MaxMemoryReadChunkBytes || size > int64(memoryReadRangeByteLimit-scope.bytes) {
		return nil, domain.ErrMemoryProjectionLimit
	}
	if digest == nil || domain.ContentHash(*digest) != reference.Hash || start != int64(reference.Start) || count != int64(reference.Count) ||
		size != int64(reference.Bytes) || size != int64(len(raw)) {
		return nil, domain.ErrIntegrity
	}
	scope.bytes += len(raw)
	return scope.verifier.Decode(readContext, ordinal, raw)
}
