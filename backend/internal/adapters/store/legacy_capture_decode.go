package store

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyCapturePart struct {
	hash  domain.ContentHash
	bytes int
}

type legacyProofSpool struct {
	*legacyDocSpool
}

func checkedLegacySpoolRead(ctx context.Context, spool *legacyDocSpool, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spool == nil || spool.file == nil {
		return nil, os.ErrClosed
	}
	info, err := spool.file.Stat()
	if err != nil || info.Size() != int64(spool.size) {
		return nil, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	return spool.Read(ctx, offset, length)
}

func (spool *legacyProofSpool) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if spool == nil {
		return nil, os.ErrClosed
	}
	return checkedLegacySpoolRead(ctx, spool.legacyDocSpool, offset, length)
}

type legacyCaptureReader struct {
	ctx      context.Context
	source   *legacyDocSpool
	parts    []legacyCapturePart
	extent   int
	offset   int
	buffer   []byte
	position int
	failure  error
}

func (reader *legacyCaptureReader) check() error {
	if err := reader.ctx.Err(); err != nil {
		return err
	}
	if reader.source == nil || reader.source.file == nil || reader.extent < 0 || reader.offset < 0 || reader.offset > reader.extent ||
		reader.source.size != reader.extent || len(reader.parts) != (reader.extent+domain.ConversationManifestChunkBytes-1)/domain.ConversationManifestChunkBytes {
		return domain.ErrPreparedDocRangeObject
	}
	info, err := reader.source.file.Stat()
	if err != nil {
		return errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	if info.Size() != int64(reader.extent) {
		return domain.ErrPreparedDocRangeObject
	}
	return nil
}

func (reader *legacyCaptureReader) fill() error {
	if err := reader.check(); err != nil {
		return err
	}
	if reader.offset == reader.extent {
		return io.EOF
	}
	if reader.buffer != nil {
		return nil
	}
	ordinal := reader.offset / domain.ConversationManifestChunkBytes
	length := min(domain.ConversationManifestChunkBytes, reader.extent-reader.offset)
	if reader.offset%domain.ConversationManifestChunkBytes != 0 || reader.parts[ordinal].bytes != length {
		return domain.ErrPreparedDocRangeObject
	}
	body, err := reader.source.Read(reader.ctx, reader.offset, length)
	if err != nil {
		if canceled := reader.ctx.Err(); canceled != nil {
			return canceled
		}
		return errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	hash, err := domain.HashContentContext(reader.ctx, body)
	if err != nil {
		return err
	}
	if hash != reader.parts[ordinal].hash {
		return domain.ErrPreparedDocRangeObject
	}
	reader.buffer, reader.position = body, 0
	return nil
}

func (reader *legacyCaptureReader) Read(body []byte) (int, error) {
	if reader.failure != nil {
		return 0, reader.failure
	}
	if err := reader.fill(); err != nil {
		if !errors.Is(err, io.EOF) {
			reader.failure = err
		}
		return 0, err
	}
	length := copy(body, reader.buffer[reader.position:])
	reader.position += length
	reader.offset += length
	if reader.position == len(reader.buffer) {
		reader.buffer, reader.position = nil, 0
	}
	return length, nil
}

type legacyCaptureDecodeWork struct {
	reader             *legacyCaptureReader
	stream             io.Reader
	closeStream        func()
	compressed         bool
	decoded            *legacyDocSpool
	parts              []legacyCapturePart
	normalizationParts []legacyCapturePart
	partHeap           rootPublicationHeapEstimate
	draining           bool
	streamError        error
	terminal           error
	complete           bool
	closed             bool
	once               sync.Once
	err                error
}

func newLegacyCaptureDecodeWork(ctx context.Context, source *legacyDocSpool, parts []legacyCapturePart, extent int) (_ *legacyCaptureDecodeWork, result error) {
	reader := &legacyCaptureReader{ctx: ctx, source: source, parts: parts, extent: extent}
	if err := reader.fill(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	compressed := docObjectIsCompressed(reader.buffer)
	stream, closeStream, err := docObjectReader(reader, compressed)
	work := &legacyCaptureDecodeWork{reader: reader, stream: stream, closeStream: closeStream, compressed: compressed}
	if err != nil {
		work.draining, work.streamError = true, domain.ErrIntegrity
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, work.Close())
		}
	}()
	work.decoded, err = newLegacyDocSpool()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return work, nil
}

func (work *legacyCaptureDecodeWork) stopStream() {
	if work.closeStream != nil {
		work.closeStream()
	}
	work.stream, work.closeStream = nil, nil
}

func (work *legacyCaptureDecodeWork) finishStream(failure error) error {
	work.stopStream()
	work.draining, work.streamError = true, failure
	if work.reader.offset != work.reader.extent {
		return nil
	}
	if err := work.reader.check(); err != nil {
		work.terminal = err
		return err
	}
	work.complete, work.draining, work.terminal = failure == nil, false, failure
	work.reader.buffer, work.reader.parts, work.reader.ctx, work.reader.source = nil, nil, nil, nil
	return failure
}

func (work *legacyCaptureDecodeWork) Step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.closed || work.reader == nil || work.decoded == nil {
		return false, os.ErrClosed
	}
	if work.terminal != nil {
		return false, work.terminal
	}
	if work.complete {
		return true, nil
	}
	work.reader.ctx = ctx
	if work.draining {
		_, err := work.reader.Read(make([]byte, domain.ConversationManifestChunkBytes))
		if err != nil && !errors.Is(err, io.EOF) {
			work.terminal = err
			return false, err
		}
		if errors.Is(err, io.EOF) || work.reader.offset == work.reader.extent {
			if err := ctx.Err(); err != nil {
				work.terminal = err
				return false, err
			}
			work.complete, work.draining, work.terminal = work.streamError == nil, false, work.streamError
			work.reader.buffer, work.reader.parts, work.reader.ctx, work.reader.source = nil, nil, nil, nil
			return work.complete, work.terminal
		}
		return false, nil
	}
	length := min(domain.ConversationManifestChunkBytes, maxLegacyReadBytes+1-work.decoded.size)
	if length <= 0 || work.stream == nil {
		return false, domain.ErrIntegrity
	}
	body := make([]byte, length)
	used := 0
	var readError error
	for used < len(body) {
		read, err := work.stream.Read(body[used:])
		used += read
		if canceled := ctx.Err(); canceled != nil {
			work.terminal = canceled
			return false, canceled
		}
		if work.reader.failure != nil {
			work.terminal = work.reader.failure
			return false, work.terminal
		}
		if err != nil {
			readError = err
			break
		}
		if read == 0 {
			readError = io.ErrNoProgress
			break
		}
	}
	if readError != nil && !errors.Is(readError, io.EOF) {
		return false, work.finishStream(domain.ErrIntegrity)
	}
	if used > maxLegacyReadBytes-work.decoded.size {
		return false, work.finishStream(domain.ErrDocumentResourceLimit)
	}
	if used != 0 {
		hash, err := domain.HashContentContext(ctx, body[:used])
		if err != nil {
			work.terminal = err
			return false, err
		}
		if err := work.decoded.Append(ctx, body[:used]); err != nil {
			work.terminal = err
			return false, err
		}
		work.partHeap.allocation(int64(len(hash)), 1)
		if work.partHeap.err != nil {
			work.terminal = work.partHeap.err
			return false, work.terminal
		}
		work.parts = append(work.parts, legacyCapturePart{hash: hash, bytes: used})
		for offset := 0; offset < used; offset += domain.LegacyNormalizationPartBytes {
			length := min(domain.LegacyNormalizationPartBytes, used-offset)
			hash, err := domain.HashContentContext(ctx, body[offset:offset+length])
			if err != nil {
				work.terminal = err
				return false, err
			}
			work.partHeap.allocation(int64(len(hash)), 1)
			if work.partHeap.err != nil {
				work.terminal = work.partHeap.err
				return false, work.terminal
			}
			work.normalizationParts = append(work.normalizationParts, legacyCapturePart{hash: hash, bytes: length})
		}
	}
	if errors.Is(readError, io.EOF) {
		if err := work.finishStream(nil); err != nil {
			return false, err
		}
	}
	return work.complete, nil
}

func verifiedLegacyCaptureBytes(ctx context.Context, source *legacyDocSpool, parts []legacyCapturePart, extent int) ([]byte, error) {
	reader := &legacyCaptureReader{ctx: ctx, source: source, parts: parts, extent: extent}
	if err := reader.check(); err != nil {
		return nil, err
	}
	body := make([]byte, extent)
	for offset := 0; offset < len(body); {
		read, err := reader.Read(body[offset:min(offset+domain.ConversationManifestChunkBytes, len(body))])
		if err != nil {
			return nil, err
		}
		if read == 0 {
			return nil, domain.ErrPreparedDocRangeObject
		}
		offset += read
	}
	if err := reader.check(); err != nil {
		return nil, err
	}
	return body, ctx.Err()
}

func (work *legacyCaptureDecodeWork) Bytes(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work == nil || work.closed || work.decoded == nil {
		return nil, os.ErrClosed
	}
	if !work.complete {
		return nil, domain.ErrDocRangeIndexPending
	}
	return verifiedLegacyCaptureBytes(ctx, work.decoded, work.parts, work.decoded.size)
}

func (work *legacyCaptureDecodeWork) proofSource(ctx context.Context) (domain.LegacyByteSource, error) {
	return work.byteSource(ctx, false)
}

func (work *legacyCaptureDecodeWork) normalizationSource(ctx context.Context) (domain.LegacyByteSource, error) {
	return work.byteSource(ctx, true)
}

func (work *legacyCaptureDecodeWork) byteSource(ctx context.Context, normalization bool) (domain.LegacyByteSource, error) {
	if err := ctx.Err(); err != nil {
		return domain.LegacyByteSource{}, err
	}
	if work == nil || work.closed || !work.complete || work.decoded == nil {
		return domain.LegacyByteSource{}, os.ErrClosed
	}
	selected, partBytes := work.parts, 0
	if normalization {
		selected, partBytes = work.normalizationParts, domain.LegacyNormalizationPartBytes
	}
	parts := make([]domain.ConversationManifestChunk, len(selected))
	for ordinal, part := range selected {
		if err := ctx.Err(); err != nil {
			return domain.LegacyByteSource{}, err
		}
		parts[ordinal] = domain.ConversationManifestChunk{Hash: part.hash, Bytes: int64(part.bytes)}
	}
	return domain.LegacyByteSource{Bytes: work.decoded.size, PartBytes: partBytes, Parts: parts, Read: func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if work.closed || work.decoded == nil {
			return nil, os.ErrClosed
		}
		return checkedLegacySpoolRead(ctx, work.decoded, offset, length)
	}}, nil
}

func (work *legacyCaptureDecodeWork) RetainedResources() (outbound.RootPublicationResources, error) {
	if work == nil || work.closed || work.decoded == nil || work.reader == nil {
		return outbound.RootPublicationResources{}, os.ErrClosed
	}
	info, err := work.decoded.file.Stat()
	if err != nil {
		return outbound.RootPublicationResources{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
	}
	if info.Size() < 0 || info.Size() != int64(work.decoded.size) || work.partHeap.err != nil {
		return outbound.RootPublicationResources{}, domain.ErrPreparedDocRangeObject
	}
	estimate := rootPublicationHeapEstimate{bytes: work.partHeap.bytes}
	estimate.allocation(1, int64(unsafe.Sizeof(*work)+unsafe.Sizeof(*work.reader)+unsafe.Sizeof(*work.decoded)))
	estimate.allocation(int64(cap(work.parts)), int64(unsafe.Sizeof(legacyCapturePart{})))
	estimate.allocation(int64(cap(work.normalizationParts)), int64(unsafe.Sizeof(legacyCapturePart{})))
	estimate.allocation(int64(cap(work.reader.buffer)), 1)
	estimate.allocation(int64(len(work.decoded.file.Name())), 1)
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	return outbound.RootPublicationResources{HeapBytes: estimate.bytes, SpoolBytes: info.Size(), KeepPermit: work.compressed && work.stream != nil}, nil
}

func (work *legacyCaptureDecodeWork) Close() error {
	if work == nil {
		return nil
	}
	work.once.Do(func() {
		work.closed = true
		work.stopStream()
		if work.decoded != nil {
			work.err = work.decoded.Close()
		}
		work.reader, work.decoded, work.parts, work.normalizationParts = nil, nil, nil, nil
		work.partHeap = rootPublicationHeapEstimate{}
	})
	return work.err
}
