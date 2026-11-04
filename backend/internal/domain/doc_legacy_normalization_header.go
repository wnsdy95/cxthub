package domain

import (
	"context"
	"unsafe"
)

type legacyNormalizationHeaderChunk struct {
	body []byte
	next *legacyNormalizationHeaderChunk
}

type legacyNormalizationHeader struct {
	encoding *LegacyEnvelopeEncoding
	native   *legacyNativeObject
	first    *legacyNormalizationHeaderChunk
	last     *legacyNormalizationHeaderChunk
	heap     docRangeHeaderHeapEstimate
	header   []byte
	output   []byte
	length   int
	position int
	copying  bool
}

func (header *legacyNormalizationHeader) append(chunk []byte) error {
	if len(chunk) > int(^uint(0)>>1)-header.length {
		return ErrIntegrity
	}
	header.length += len(chunk)
	for len(chunk) != 0 {
		if header.last == nil || len(header.last.body) == cap(header.last.body) {
			capacity := 256
			if header.last != nil {
				capacity = min(LegacyCanonicalEncodingBytes, 2*cap(header.last.body))
			}
			next := &legacyNormalizationHeaderChunk{body: make([]byte, 0, capacity)}
			if header.last == nil {
				header.first = next
			} else {
				header.last.next = next
			}
			header.last = next
			header.heap.allocation(1, int64(unsafe.Sizeof(*next)))
			header.heap.allocation(int64(capacity), 1)
			if header.heap.err != nil {
				return header.heap.err
			}
		}
		length := min(len(chunk), cap(header.last.body)-len(header.last.body))
		header.last.body = append(header.last.body, chunk[:length]...)
		chunk = chunk[length:]
	}
	return nil
}

func (header *legacyNormalizationHeader) nextWork() (int, int) {
	return header.encoding.stepWork()
}

func (header *legacyNormalizationHeader) step(ctx context.Context) ([]byte, bool, error) {
	work := 0
	for actions := 0; actions < legacyCanonicalEncodingActions; actions++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		remaining := ConversationManifestChunkBytes - work
		if header.encoding != nil {
			operation, output := header.nextWork()
			if operation+2*output > remaining {
				return nil, false, nil
			}
			chunk, complete, err := header.encoding.Step(ctx)
			if err != nil {
				return nil, false, err
			}
			if len(chunk) > output {
				return nil, false, ErrIntegrity
			}
			if err := header.append(chunk); err != nil {
				return nil, false, err
			}
			work += operation + 2*len(chunk)
			if complete {
				header.encoding = nil
				header.native = nil
				header.header = make([]byte, header.length)
			}
			continue
		}
		if header.first != nil {
			length := len(header.first.body)
			if 2*length > remaining {
				return nil, false, nil
			}
			copy(header.header[header.position:], header.first.body)
			header.position += length
			work += 2 * length
			var released docRangeHeaderHeapEstimate
			released.allocation(1, int64(unsafe.Sizeof(*header.first)))
			released.allocation(int64(cap(header.first.body)), 1)
			if released.err != nil || header.heap.bytes < released.bytes {
				return nil, false, ErrIntegrity
			}
			header.heap.bytes -= released.bytes
			header.first = header.first.next
			if header.first == nil {
				header.last = nil
			}
			continue
		}
		if !header.copying {
			if header.position != header.length {
				return nil, false, ErrIntegrity
			}
			header.output = make([]byte, header.length)
			header.position, header.copying = 0, true
		}
		if header.position == header.length {
			output := header.output
			header.output = nil
			return output, true, nil
		}
		length := min(header.length-header.position, LegacyCanonicalEncodingBytes, remaining/2)
		if length <= 0 {
			return nil, false, nil
		}
		copy(header.output[header.position:header.position+length], header.header[header.position:header.position+length])
		header.position += length
		work += 2 * length
	}
	return nil, false, ctx.Err()
}

func (header *legacyNormalizationHeader) retained() (int64, error) {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*header)))
	estimate.include(header.heap)
	estimate.allocation(int64(cap(header.header)), 1)
	estimate.allocation(int64(cap(header.output)), 1)
	if header.encoding != nil {
		if header.encoding.initialized {
			retained, err := header.encoding.RetainedBytes()
			if err != nil {
				return 0, err
			}
			estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
		} else {
			estimate.allocation(1, int64(unsafe.Sizeof(*header.encoding)))
			estimate.include(header.encoding.envelopeHeap)
		}
	}
	if header.native != nil {
		retained, err := header.native.retainedBase()
		var shared docRangeHeaderHeapEstimate
		for _, field := range header.native.heaps {
			shared.include(field)
		}
		if err != nil || shared.err != nil || retained < shared.bytes || header.encoding == nil {
			return 0, ErrIntegrity
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained - shared.bytes})
	}
	return estimate.bytes, estimate.err
}
