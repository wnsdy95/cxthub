package store

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docRangeSpool struct {
	file    *os.File
	writer  *bufio.Writer
	quota   *transientSpoolAllocation
	offsets []int64
	once    sync.Once
	err     error
}

func newDocRangeSpool() (*docRangeSpool, error) {
	quota, err := newTransientSpoolAllocation()
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "cxthub-doc-range-*")
	if err != nil {
		quota.Close()
		return nil, err
	}
	return &docRangeSpool{file: file, writer: bufio.NewWriterSize(file, domain.MaxDocRangePageBytes), quota: quota, offsets: []int64{0}}, nil
}

func (spool *docRangeSpool) Append(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if spool.writer == nil {
		return os.ErrClosed
	}
	if len(raw) == 0 || len(raw) > domain.MaxDocRangePageBytes || len(spool.offsets)-1 >= domain.MaxDocRangePages {
		return domain.ErrIntegrity
	}
	offset := spool.offsets[len(spool.offsets)-1]
	if err := spool.quota.Reserve(int64(len(raw))); err != nil {
		return err
	}
	written, err := spool.writer.Write(raw)
	if written < len(raw) {
		spool.quota.Release(int64(len(raw) - written))
	}
	if err != nil {
		return err
	}
	if written != len(raw) {
		return io.ErrShortWrite
	}
	spool.offsets = append(spool.offsets, offset+int64(written))
	return ctx.Err()
}

func (spool *docRangeSpool) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if spool.writer == nil {
		return os.ErrClosed
	}
	if err := spool.writer.Flush(); err != nil {
		return err
	}
	return ctx.Err()
}

func (spool *docRangeSpool) Read(ctx context.Context, position int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spool.writer == nil {
		return nil, os.ErrClosed
	}
	if position < 0 || position >= len(spool.offsets)-1 {
		return nil, domain.ErrIntegrity
	}
	offset, end := spool.offsets[position], spool.offsets[position+1]
	if end <= offset || end-offset > domain.MaxDocRangePageBytes {
		return nil, domain.ErrIntegrity
	}
	if err := spool.Flush(ctx); err != nil {
		return nil, err
	}
	raw := make([]byte, int(end-offset))
	if _, err := spool.file.ReadAt(raw, offset); err != nil {
		return nil, err
	}
	return raw, ctx.Err()
}

func (spool *docRangeSpool) Close() error {
	spool.once.Do(func() {
		spool.writer = nil
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

var _ domain.DocRangePageBuffer = (*docRangeSpool)(nil)
