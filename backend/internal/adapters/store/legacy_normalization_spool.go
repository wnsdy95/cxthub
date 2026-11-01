package store

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyNormalizationSpool struct {
	spools   [domain.LegacyNormalizationSlots]*legacyDocSpool
	readOnly bool
	closed   bool
	once     sync.Once
	err      error
}

func newLegacyNormalizationSpool(ctx context.Context) (_ *legacyNormalizationSpool, failure error) {
	storage := &legacyNormalizationSpool{}
	defer func() {
		if failure != nil {
			failure = errors.Join(failure, storage.Close())
			if err := ctx.Err(); err != nil {
				failure = err
			}
		}
	}()
	for slot := range storage.spools {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		spool, err := newLegacyDocSpool()
		if err != nil {
			if errors.Is(err, domain.ErrResourceOverloaded) {
				return nil, err
			}
			return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
		}
		storage.spools[slot] = spool
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := storage.RetainedResources(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return storage, nil
}

func (storage *legacyNormalizationSpool) slot(ctx context.Context, slot int) (*legacyDocSpool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if storage == nil || storage.closed {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, os.ErrClosed)
	}
	if slot < 0 || slot >= len(storage.spools) {
		return nil, domain.ErrIntegrity
	}
	spool := storage.spools[slot]
	if spool == nil || spool.file == nil {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, os.ErrClosed)
	}
	info, err := spool.file.Stat()
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil || spool.size < 0 || spool.size > domain.MaxFinalizedDocBytes || info.Size() != int64(spool.size) {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return spool, nil
}

func (storage *legacyNormalizationSpool) Reset(ctx context.Context, slot int) error {
	spool, err := storage.slot(ctx, slot)
	if err != nil {
		return err
	}
	if storage.readOnly {
		return domain.ErrIntegrity
	}
	if err := spool.Truncate(ctx); err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		return errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	position, err := spool.file.Seek(0, io.SeekStart)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil || position != 0 {
		return errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return nil
}

func (storage *legacyNormalizationSpool) Append(ctx context.Context, slot int, body []byte) error {
	spool, err := storage.slot(ctx, slot)
	if err != nil {
		return err
	}
	if storage.readOnly || len(body) > domain.MaxFinalizedDocBytes-spool.size {
		return domain.ErrIntegrity
	}
	err = spool.Append(ctx, body)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil {
		if errors.Is(err, domain.ErrResourceOverloaded) {
			return err
		}
		return errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return nil
}

func (storage *legacyNormalizationSpool) Read(ctx context.Context, slot, offset, length int) ([]byte, error) {
	spool, err := storage.slot(ctx, slot)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > spool.size || length > spool.size-offset {
		return nil, domain.ErrIntegrity
	}
	if length > maxLegacyReadBytes {
		return nil, domain.ErrDocumentResourceLimit
	}
	body, err := spool.Read(ctx, offset, length)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return body, nil
}

func (storage *legacyNormalizationSpool) RetainedResources() (outbound.RootPublicationResources, error) {
	if storage == nil || storage.closed {
		return outbound.RootPublicationResources{}, os.ErrClosed
	}
	var resources outbound.RootPublicationResources
	estimate := rootPublicationHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(*storage)))
	var files [domain.LegacyNormalizationSlots]os.FileInfo
	for slot, spool := range storage.spools {
		if spool == nil || spool.file == nil {
			return outbound.RootPublicationResources{}, domain.ErrPreparedDocRangeObject
		}
		info, err := spool.file.Stat()
		if err != nil || spool.size < 0 || spool.size > domain.MaxFinalizedDocBytes || info.Size() != int64(spool.size) ||
			resources.SpoolBytes > math.MaxInt64-info.Size() {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
		}
		for prior := 0; prior < slot; prior++ {
			if spool == storage.spools[prior] || spool.file == storage.spools[prior].file || os.SameFile(info, files[prior]) {
				return outbound.RootPublicationResources{}, domain.ErrPreparedDocRangeObject
			}
		}
		files[slot] = info
		resources.SpoolBytes += info.Size()
		estimate.allocation(1, int64(unsafe.Sizeof(*spool)))
		estimate.allocation(int64(len(spool.file.Name())), 1)
	}
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	resources.HeapBytes = estimate.bytes
	return resources, nil
}

func (storage *legacyNormalizationSpool) Close() error {
	if storage == nil {
		return nil
	}
	storage.once.Do(func() {
		storage.closed = true
		for slot, spool := range storage.spools {
			if spool != nil {
				storage.err = errors.Join(storage.err, spool.Close())
				storage.spools[slot] = nil
			}
		}
		if storage.err != nil {
			storage.err = errors.Join(domain.ErrPreparedDocRangeObject, storage.err)
		}
	})
	return storage.err
}

var _ domain.LegacyNormalizationStorage = (*legacyNormalizationSpool)(nil)
