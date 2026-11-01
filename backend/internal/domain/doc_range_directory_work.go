package domain

import (
	"context"
	"unsafe"
)

type docRangeDirectoryWork struct {
	hashes     []ContentHash
	parents    []ContentHash
	hashHeap   docRangeHeaderHeapEstimate
	parentHeap docRangeHeaderHeapEstimate
	position   int
	level      int
	span       int
	root       DocRangeDirectoryRoot
	done       bool
	failure    error
}

func newDocRangeDirectoryWork(hashes []ContentHash) (*docRangeDirectoryWork, error) {
	if len(hashes) > MaxDocRangePages {
		return nil, ErrIntegrity
	}
	if len(hashes) == 0 {
		return startDocRangeDirectoryWork(nil, docRangeHeaderHeapEstimate{})
	}
	heap := docRangeHeaderHeapEstimate{}
	for _, hash := range hashes[:cap(hashes)] {
		heap.allocation(int64(len(hash)), 1)
	}
	return startDocRangeDirectoryWork(hashes, heap)
}

func startDocRangeDirectoryWork(hashes []ContentHash, heap docRangeHeaderHeapEstimate) (*docRangeDirectoryWork, error) {
	if len(hashes) > MaxDocRangePages {
		return nil, ErrIntegrity
	}
	if heap.err != nil {
		return nil, heap.err
	}
	if len(hashes) == 0 {
		return &docRangeDirectoryWork{span: 1, done: true}, nil
	}
	return &docRangeDirectoryWork{hashes: hashes, hashHeap: heap, span: 1,
		parents: make([]ContentHash, 0, (len(hashes)+DocRangeDirectoryFanout-1)/DocRangeDirectoryFanout),
		root:    DocRangeDirectoryRoot{Count: len(hashes)}, done: len(hashes) == 0}, nil
}

func (work *docRangeDirectoryWork) step(ctx context.Context, emit func(DocRangeObject) error) (complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.failure != nil {
		return false, ErrIntegrity
	}
	if work.done {
		return true, nil
	}
	defer func() {
		if failure != nil {
			work.failure = failure
		}
	}()
	if emit == nil {
		return false, ErrValidation
	}
	end := min(work.position+DocRangeDirectoryFanout, len(work.hashes))
	directory := docRangeDirectory{Level: work.level, Start: work.position * work.span, Hashes: work.hashes[work.position:end]}
	raw, err := canonicalJSON(directory)
	if err != nil || len(raw) > MaxDocRangeDirectoryBytes {
		return false, ErrIntegrity
	}
	hash := HashContent(raw)
	if err := emit(DocRangeObject{Hash: hash, Bytes: raw}); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	work.parents = append(work.parents, hash)
	work.parentHeap.allocation(int64(len(hash)), 1)
	if work.parentHeap.err != nil {
		return false, work.parentHeap.err
	}
	work.position = end
	if end == len(work.hashes) {
		if len(work.parents) == 1 {
			work.root.Hash, work.done = work.parents[0], true
			work.hashes, work.parents = nil, nil
			work.hashHeap, work.parentHeap = docRangeHeaderHeapEstimate{}, docRangeHeaderHeapEstimate{}
		} else {
			work.hashes, work.parents = work.parents, make([]ContentHash, 0, (len(work.parents)+DocRangeDirectoryFanout-1)/DocRangeDirectoryFanout)
			work.hashHeap, work.parentHeap = work.parentHeap, docRangeHeaderHeapEstimate{}
			work.position, work.level, work.span = 0, work.level+1, work.span*DocRangeDirectoryFanout
		}
	}
	return work.done, nil
}

func (work *docRangeDirectoryWork) retained(estimate *docRangeHeaderHeapEstimate) {
	if work == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*work)))
	for _, values := range [][]ContentHash{work.hashes, work.parents} {
		estimate.allocation(int64(cap(values)), int64(unsafe.Sizeof(ContentHash(""))))
	}
	estimate.include(work.hashHeap)
	estimate.include(work.parentHeap)
	estimate.allocation(int64(len(work.root.Hash)), 1)
}

type docRangeChunkDirectoryWork struct {
	chunks    []ConversationManifestChunk
	position  int
	pages     []ContentHash
	chunkHeap docRangeHeaderHeapEstimate
	pageHeap  docRangeHeaderHeapEstimate
	directory *docRangeDirectoryWork
	root      DocRangeDirectoryRoot
	done      bool
	failure   error
}

func newDocRangeChunkDirectoryWork(chunks []ConversationManifestChunk) (*docRangeChunkDirectoryWork, error) {
	count := (len(chunks) + DocRangeChunkPageSize - 1) / DocRangeChunkPageSize
	if count > MaxDocRangePages {
		return nil, ErrIntegrity
	}
	if count == 0 {
		return &docRangeChunkDirectoryWork{}, nil
	}
	heap := docRangeHeaderHeapEstimate{}
	for _, chunk := range chunks[:cap(chunks)] {
		heap.allocation(int64(len(chunk.Hash)), 1)
	}
	if heap.err != nil {
		return nil, heap.err
	}
	return &docRangeChunkDirectoryWork{chunks: chunks, chunkHeap: heap, pages: make([]ContentHash, 0, count)}, nil
}

func (work *docRangeChunkDirectoryWork) step(ctx context.Context, emit func(DocRangeObject) error) (complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.failure != nil {
		return false, ErrIntegrity
	}
	if work.done {
		return true, nil
	}
	defer func() {
		if failure != nil {
			work.failure = failure
		}
	}()
	if emit == nil {
		return false, ErrValidation
	}
	if work.position < len(work.chunks) {
		end := min(work.position+DocRangeChunkPageSize, len(work.chunks))
		raw, err := canonicalJSON(work.chunks[work.position:end])
		if err != nil || len(raw) > MaxDocRangeDirectoryBytes {
			return false, ErrIntegrity
		}
		hash := HashContent(raw)
		if err := emit(DocRangeObject{Hash: hash, Bytes: raw}); err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		work.pages, work.position = append(work.pages, hash), end
		work.pageHeap.allocation(int64(len(hash)), 1)
		if work.pageHeap.err != nil {
			return false, work.pageHeap.err
		}
		return false, nil
	}
	if work.directory == nil {
		var err error
		work.directory, err = startDocRangeDirectoryWork(work.pages, work.pageHeap)
		if err != nil {
			return false, err
		}
	}
	complete, failure = work.directory.step(ctx, emit)
	if failure == nil && complete {
		work.root, work.done = work.directory.root, true
		work.chunks, work.pages, work.directory = nil, nil, nil
		work.chunkHeap, work.pageHeap = docRangeHeaderHeapEstimate{}, docRangeHeaderHeapEstimate{}
	}
	return complete, failure
}

func (work *docRangeChunkDirectoryWork) retained(estimate *docRangeHeaderHeapEstimate) {
	if work == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*work)))
	estimate.allocation(int64(cap(work.chunks)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	estimate.include(work.chunkHeap)
	estimate.allocation(int64(cap(work.pages)), int64(unsafe.Sizeof(ContentHash(""))))
	estimate.include(work.pageHeap)
	estimate.allocation(int64(len(work.root.Hash)), 1)
	work.directory.retained(estimate)
}
