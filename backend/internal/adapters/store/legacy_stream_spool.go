package store

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyDocSpool struct {
	file          *os.File
	quota         *transientSpoolAllocation
	reservedBytes int64
	size          int
	once          sync.Once
	err           error
}

func newLegacyDocSpool() (*legacyDocSpool, error) {
	quota, err := newTransientSpoolAllocation()
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "cxthub-legacy-doc-*")
	if err != nil {
		quota.Close()
		return nil, err
	}
	return &legacyDocSpool{file: file, quota: quota}, nil
}

func (spool *legacyDocSpool) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(body) > domain.MaxFinalizedDocBytes-spool.size {
		return domain.ErrIntegrity
	}
	if err := spool.quota.Reserve(int64(len(body))); err != nil {
		return err
	}
	written, err := spool.file.WriteAt(body, int64(spool.size))
	spool.reservedBytes += int64(written)
	if written < len(body) {
		spool.quota.Release(int64(len(body) - written))
	}
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	spool.size += written
	return ctx.Err()
}

func (spool *legacyDocSpool) Truncate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := spool.file.Truncate(0); err != nil {
		return err
	}
	spool.quota.Release(spool.reservedBytes)
	spool.reservedBytes = 0
	spool.size = 0
	return ctx.Err()
}

func (spool *legacyDocSpool) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > spool.size || length > spool.size-offset {
		return nil, domain.ErrIntegrity
	}
	if length > maxLegacyReadBytes {
		return nil, domain.ErrDocumentResourceLimit
	}
	body := make([]byte, length)
	if _, err := spool.file.ReadAt(body, int64(offset)); err != nil {
		return nil, err
	}
	return body, ctx.Err()
}

func (spool *legacyDocSpool) Close() error {
	spool.once.Do(func() {
		closeErr := spool.file.Close()
		removeErr := os.Remove(spool.file.Name())
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		if removeErr == nil {
			spool.quota.Close()
		}
		spool.err = errors.Join(closeErr, removeErr)
	})
	return spool.err
}

const maxLegacyReadBytes = 32 << 20

func boundedLegacyObject(ctx context.Context, raw []byte, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > limit*2 {
		return nil, domain.ErrDocumentResourceLimit
	}
	reader, closeReader, err := docObjectStream(raw)
	if err != nil {
		return nil, domain.ErrIntegrity
	}
	defer closeReader()
	body, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, domain.ErrIntegrity
	}
	if len(body) > limit {
		return nil, domain.ErrDocumentResourceLimit
	}
	return body, ctx.Err()
}
