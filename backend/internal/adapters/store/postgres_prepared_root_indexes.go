//go:build postgres

package store

import (
	"context"
	"errors"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pgRootIndexItem struct {
	hash      domain.ContentHash
	position  int
	rangePage bool
}

type pgPreparedRootIndexes struct {
	document          domain.ContentHash
	headerHash        domain.ContentHash
	rangeHash         domain.ContentHash
	visibleHash       domain.ContentHash
	header            []byte
	rangeDescriptor   []byte
	visibleDescriptor []byte
	spool             *docRangeSpool
	items             []pgRootIndexItem
}

func preparePGRootIndexes(ctx context.Context, prepared *preparedStreamingRootDoc) (result *pgPreparedRootIndexes, failure error) {
	if prepared == nil {
		return nil, domain.ErrIntegrity
	}
	if err := prepared.matches(prepared.job); err != nil {
		return nil, err
	}
	header, raw, err := verifiedRangeHeader(prepared.root.ranges)
	if err != nil {
		return nil, err
	}
	proof, objects, err := domain.BuildVerifiedDocRangeGeneration(ctx, prepared.root.ranges)
	if err != nil {
		return nil, err
	}
	ranges, err := proof.Descriptor().Encode()
	if err != nil {
		return nil, err
	}
	spool, err := newDocRangeSpool()
	if err != nil {
		return nil, err
	}
	indexes := &pgPreparedRootIndexes{document: header.Hash, header: raw, headerHash: domain.HashContent(raw),
		rangeDescriptor: ranges, rangeHash: domain.HashContent(ranges), spool: spool}
	complete := false
	defer func() {
		if !complete {
			failure = errors.Join(failure, spool.Close())
		}
	}()
	emit := func(ctx context.Context, object domain.DocRangeObject) error {
		if len(object.Bytes) == 0 || len(object.Bytes) > domain.MaxDocVisibleObjectBytes || domain.HashContent(object.Bytes) != object.Hash {
			return domain.ErrIntegrity
		}
		position := len(spool.offsets) - 1
		if err := spool.Append(ctx, object.Bytes); err != nil {
			return err
		}
		indexes.items = append(indexes.items, pgRootIndexItem{hash: object.Hash, position: position})
		return nil
	}
	for _, object := range objects {
		if err := emit(ctx, object); err != nil {
			return nil, err
		}
	}
	generation, err := domain.BuildDocVisibleGenerationWithRangePages(ctx, proof, emit, func(ctx context.Context, position int, object domain.DocRangeObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if position < 0 || position >= len(header.Pages) || object.Hash != header.Pages[position] {
			return domain.ErrIntegrity
		}
		indexes.items = append(indexes.items, pgRootIndexItem{hash: object.Hash, position: position, rangePage: true})
		return nil
	})
	if err != nil {
		return nil, err
	}
	indexes.visibleDescriptor, err = generation.Encode()
	if err != nil {
		return nil, err
	}
	indexes.visibleHash = domain.HashContent(indexes.visibleDescriptor)
	if err := spool.Flush(ctx); err != nil {
		return nil, err
	}
	if err := indexes.matches(prepared.job.DocHash, len(header.Pages)); err != nil {
		return nil, err
	}
	complete = true
	return indexes, nil
}

func (indexes *pgPreparedRootIndexes) matches(hash domain.ContentHash, pages int) error {
	if indexes == nil || indexes.spool == nil || indexes.spool.file == nil || indexes.spool.writer == nil ||
		len(indexes.header) == 0 || len(indexes.header) > domain.MaxDocRangeIndexBytes ||
		len(indexes.rangeDescriptor) == 0 || len(indexes.rangeDescriptor) > domain.MaxDocRangeGenerationBytes ||
		len(indexes.visibleDescriptor) == 0 || len(indexes.visibleDescriptor) > domain.MaxDocVisibleGenerationBytes ||
		domain.HashContent(indexes.header) != indexes.headerHash || domain.HashContent(indexes.rangeDescriptor) != indexes.rangeHash ||
		domain.HashContent(indexes.visibleDescriptor) != indexes.visibleHash {
		return domain.ErrIntegrity
	}
	if indexes.document != hash {
		return domain.ErrConflict
	}
	info, err := indexes.spool.file.Stat()
	if err != nil {
		return err
	}
	spool := indexes.spool
	if !info.Mode().IsRegular() || spool.writer.Buffered() != 0 || len(spool.offsets) == 0 || spool.offsets[0] != 0 ||
		info.Size() != spool.offsets[len(spool.offsets)-1] {
		return domain.ErrIntegrity
	}
	for position := 1; position < len(spool.offsets); position++ {
		previous, next := spool.offsets[position-1], spool.offsets[position]
		if next <= previous || next-previous > domain.MaxDocVisibleObjectBytes {
			return domain.ErrIntegrity
		}
	}
	derived, ranges := 0, 0
	for _, item := range indexes.items {
		if domain.ValidateContentHash(item.hash) != nil {
			return domain.ErrIntegrity
		}
		if item.rangePage {
			if item.position != ranges {
				return domain.ErrIntegrity
			}
			ranges++
		} else {
			if item.position != derived {
				return domain.ErrIntegrity
			}
			derived++
		}
	}
	if derived != len(spool.offsets)-1 || ranges != pages {
		return domain.ErrIntegrity
	}
	return nil
}

func (indexes *pgPreparedRootIndexes) chargeRetained(estimate *rootPublicationHeapEstimate) (int64, error) {
	spool := indexes.spool
	info, err := spool.file.Stat()
	if err != nil {
		return 0, err
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*indexes)))
	estimate.allocation(1, 4096+int64(unsafe.Sizeof(*spool)))
	estimate.allocation(int64(cap(indexes.header)), 1)
	estimate.allocation(int64(cap(indexes.rangeDescriptor)), 1)
	estimate.allocation(int64(cap(indexes.visibleDescriptor)), 1)
	estimate.allocation(int64(cap(indexes.items)), int64(unsafe.Sizeof(pgRootIndexItem{})))
	estimate.allocation(int64(cap(spool.offsets)), int64(unsafe.Sizeof(int64(0))))
	estimate.allocation(int64(spool.writer.Size()), 1)
	for _, value := range []string{string(indexes.document), string(indexes.headerHash), string(indexes.rangeHash), string(indexes.visibleHash), spool.file.Name()} {
		estimate.allocation(int64(len(value)), 1)
	}
	for _, item := range indexes.items[:cap(indexes.items)] {
		estimate.allocation(int64(len(item.hash)), 1)
	}
	return info.Size(), estimate.err
}

func (indexes *pgPreparedRootIndexes) Close() error {
	if indexes == nil || indexes.spool == nil {
		return domain.ErrIntegrity
	}
	err := indexes.spool.Close()
	indexes.spool.offsets = nil
	indexes.document, indexes.headerHash, indexes.rangeHash, indexes.visibleHash = "", "", "", ""
	indexes.header, indexes.rangeDescriptor, indexes.visibleDescriptor = nil, nil, nil
	indexes.items = nil
	return err
}

func (storage *PostgresStore) putPreparedRootIndexes(ctx context.Context, repo domain.ContentHash, prepared *preparedStreamingRootDoc, indexes *pgPreparedRootIndexes) error {
	transaction, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || transaction.owner != storage || transaction.readOnly || transaction.repo != repo {
		return domain.ErrConflict
	}
	if err := indexes.matches(prepared.job.DocHash, len(prepared.spool.offsets)-1); err != nil {
		return err
	}
	if err := writeRangeGenerationPG(ctx, transaction, indexes.document, indexes.header, indexes.rangeHash, indexes.rangeDescriptor); err != nil {
		return err
	}
	batch := postgresDocVisibleBatch{transaction: transaction, hash: indexes.document}
	for _, item := range indexes.items {
		spool := indexes.spool
		if item.rangePage {
			spool = prepared.spool
		}
		raw, err := spool.Read(ctx, item.position)
		if err != nil {
			return err
		}
		object := domain.DocRangeObject{Hash: item.hash, Bytes: raw}
		if item.rangePage {
			err = batch.emitRangePage(ctx, item.position, object)
		} else {
			err = batch.emit(ctx, object)
		}
		if err != nil {
			return err
		}
	}
	if err := batch.flush(ctx); err != nil {
		return err
	}
	return writeVisibleGenerationPG(ctx, transaction, indexes.document, indexes.rangeHash, indexes.visibleHash, indexes.visibleDescriptor)
}
