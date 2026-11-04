package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
)

const LegacyNormalizationSlots = 4
const legacyNormalizationRecordBytes = 64
const legacyNormalizationRunRecords = 1024
const legacyNormalizationBufferBytes = 64 << 10

type LegacyNormalizationStorage interface {
	Reset(context.Context, int) error
	Append(context.Context, int, []byte) error
	Read(context.Context, int, int, int) ([]byte, error)
}

type legacyNormalizationFile struct {
	storage    LegacyNormalizationStorage
	slot       int
	generation uint64
	size       int
	buffer     []byte
	parts      []ConversationManifestChunk
	partHeap   docRangeHeaderHeapEstimate
	pageBytes  int
	hashState  []byte
}

func (file *legacyNormalizationFile) reset(ctx context.Context) error {
	if file.generation == ^uint64(0) {
		return ErrIntegrity
	}
	if err := file.storage.Reset(ctx, file.slot); err != nil {
		return errors.Join(ErrPreparedDocRangeObject, err)
	}
	file.size, file.buffer, file.parts = 0, nil, nil
	file.partHeap = docRangeHeaderHeapEstimate{}
	file.pageBytes, file.hashState = 0, nil
	file.generation++
	return ctx.Err()
}

func (file *legacyNormalizationFile) append(ctx context.Context, raw []byte) error {
	for len(raw) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(len(raw), legacyNormalizationBufferBytes-len(file.buffer))
		if file.size > int(^uint(0)>>1)-count {
			return ErrDocumentResourceLimit
		}
		needed := len(file.buffer) + count
		if needed > cap(file.buffer) {
			capacity := min(legacyNormalizationBufferBytes, max(needed, 2*cap(file.buffer)))
			buffer := make([]byte, len(file.buffer), capacity)
			copy(buffer, file.buffer)
			file.buffer = buffer
		}
		file.buffer = append(file.buffer, raw[:count]...)
		file.size += count
		raw = raw[count:]
		if len(file.buffer) == legacyNormalizationBufferBytes {
			if err := file.drain(ctx); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (file *legacyNormalizationFile) flush(ctx context.Context) error {
	if err := file.drain(ctx); err != nil {
		return err
	}
	if file.pageBytes != 0 {
		digest := sha256.New()
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(file.hashState); err != nil {
			return ErrIntegrity
		}
		hash := ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
		file.parts = append(file.parts, ConversationManifestChunk{Hash: hash, Bytes: int64(file.pageBytes)})
		file.partHeap.allocation(int64(len(hash)), 1)
		file.pageBytes, file.hashState = 0, nil
	}
	if file.partHeap.err != nil {
		return file.partHeap.err
	}
	return ctx.Err()
}

func (file *legacyNormalizationFile) drain(ctx context.Context) error {
	if len(file.buffer) == 0 {
		return ctx.Err()
	}
	digest := sha256.New()
	if file.pageBytes != 0 {
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(file.hashState); err != nil {
			return ErrIntegrity
		}
	}
	length := len(file.buffer)
	if length > ConversationManifestChunkBytes-file.pageBytes {
		return ErrIntegrity
	}
	digest.Write(file.buffer)
	state, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	if err := file.storage.Append(ctx, file.slot, file.buffer); err != nil {
		if errors.Is(err, ErrResourceOverloaded) {
			return err
		}
		return errors.Join(ErrPreparedDocRangeObject, err)
	}
	file.buffer = nil
	file.pageBytes += length
	file.hashState = state
	if file.pageBytes == ConversationManifestChunkBytes {
		hash := ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
		file.parts = append(file.parts, ConversationManifestChunk{Hash: hash, Bytes: int64(file.pageBytes)})
		file.partHeap.allocation(int64(len(hash)), 1)
		file.pageBytes, file.hashState = 0, nil
	}
	if file.partHeap.err != nil {
		return file.partHeap.err
	}
	return ctx.Err()
}

func (file *legacyNormalizationFile) exact(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > ConversationManifestChunkBytes || offset > file.size || length > file.size-offset || len(file.buffer) != 0 || file.pageBytes != 0 {
		return nil, ErrPreparedDocRangeObject
	}
	raw, err := file.storage.Read(ctx, file.slot, offset, length)
	if err != nil || len(raw) != length {
		return nil, errors.Join(ErrPreparedDocRangeObject, err)
	}
	raw = bytes.Clone(raw)
	return raw, ctx.Err()
}

func (file *legacyNormalizationFile) eof(ctx context.Context) error {
	err := legacySourceEOF(ctx, func(ctx context.Context, offset, length int) ([]byte, error) {
		return file.storage.Read(ctx, file.slot, offset, length)
	}, file.size)
	if err != nil {
		return errors.Join(ErrPreparedDocRangeObject, err)
	}
	return ctx.Err()
}

type legacyNormalizationWindow struct {
	files    [2]int
	ordinals [2]int
	pages    [2][]byte
	current  int
}

func (window *legacyNormalizationWindow) read(ctx context.Context, file *legacyNormalizationFile, offset, length int) ([]byte, error) {
	ordinal := offset / ConversationManifestChunkBytes
	if offset < 0 || length <= 0 || ordinal >= len(file.parts) || offset%ConversationManifestChunkBytes+length > int(file.parts[ordinal].Bytes) {
		return nil, ErrPreparedDocRangeObject
	}
	for index, page := range window.pages {
		if page != nil && window.files[index] == file.slot && window.ordinals[index] == ordinal {
			from := offset % ConversationManifestChunkBytes
			return page[from : from+length], ctx.Err()
		}
	}
	part := file.parts[ordinal]
	raw, err := file.exact(ctx, ordinal*ConversationManifestChunkBytes, int(part.Bytes))
	if err != nil {
		return nil, err
	}
	hash, err := HashContentContext(ctx, raw)
	if err != nil || hash != part.Hash {
		return nil, errors.Join(ErrPreparedDocRangeObject, err)
	}
	index := window.current
	window.current = 1 - index
	window.files[index], window.ordinals[index], window.pages[index] = file.slot, ordinal, raw
	from := offset % ConversationManifestChunkBytes
	return raw[from : from+length], nil
}
