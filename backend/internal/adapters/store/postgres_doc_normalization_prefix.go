//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const legacyNormalizationPrefixFanout = 32
const legacyNormalizationReceiptBytes = 36
const legacyNormalizationReceiptChunkBytes = legacyNormalizationReceiptBytes * legacyNormalizationPrefixFanout

type legacyNormalizationPrefixTree struct {
	Hash      domain.ContentHash `json:"h"`
	Bytes     int                `json:"n"`
	DataBytes int                `json:"s"`
	Leaves    int                `json:"l"`
	Height    uint8              `json:"d"`
}

type legacyNormalizationPrefix struct {
	Stage      string                        `json:"i"`
	Generation uint64                        `json:"g"`
	ChunkBytes int                           `json:"c"`
	Tree       legacyNormalizationPrefixTree `json:"t"`
}

func legacyNormalizationPrefixLimit(chunkBytes int) int {
	switch chunkBytes {
	case domain.LegacyCanonicalEncodingBytes:
		return domain.MaxFinalizedDocBytes
	case legacyNormalizationReceiptChunkBytes:
		return ((domain.MaxFinalizedDocBytes + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes) * legacyNormalizationReceiptBytes
	default:
		return 0
	}
}

func legacyNormalizationPrefixCapacity(height uint8) int {
	capacity := 1
	for level := uint8(0); level < height; level++ {
		capacity *= legacyNormalizationPrefixFanout
	}
	return capacity
}

func (tree legacyNormalizationPrefixTree) valid(chunkBytes int) bool {
	limit := legacyNormalizationPrefixLimit(chunkBytes)
	if limit == 0 {
		return false
	}
	if tree == (legacyNormalizationPrefixTree{}) {
		return true
	}
	maxLeaves := (limit + chunkBytes - 1) / chunkBytes
	if tree.Height > 3 || tree.Leaves < 1 || tree.Leaves > maxLeaves || tree.Leaves > legacyNormalizationPrefixCapacity(tree.Height) ||
		tree.DataBytes <= (tree.Leaves-1)*chunkBytes || tree.DataBytes > tree.Leaves*chunkBytes || tree.DataBytes > limit ||
		tree.Bytes < 1 || tree.Bytes > domain.LegacyCanonicalEncodingBytes || domain.ValidateContentHash(tree.Hash) != nil {
		return false
	}
	return tree.Height != 0 || tree.Leaves == 1 && tree.Bytes == tree.DataBytes
}

func (prefix legacyNormalizationPrefix) valid() bool {
	if prefix == (legacyNormalizationPrefix{}) {
		return true
	}
	if len(prefix.Stage) != 36 || prefix.Generation == 0 || !prefix.Tree.valid(prefix.ChunkBytes) {
		return false
	}
	height, capacity := uint8(0), 1
	for capacity < prefix.Tree.Leaves {
		height++
		capacity *= legacyNormalizationPrefixFanout
	}
	return prefix.Tree.Height == height
}

type legacyNormalizationPrefixNode struct {
	Version  uint8                           `json:"v"`
	Children []legacyNormalizationPrefixTree `json:"e"`
}

func (node *legacyNormalizationPrefixNode) UnmarshalJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	for _, want := range []any{json.Delim('{'), "v"} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return domain.ErrIntegrity
		}
	}
	if err := decoder.Decode(&node.Version); err != nil {
		return domain.ErrIntegrity
	}
	for _, want := range []any{"e", json.Delim('[')} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return domain.ErrIntegrity
		}
	}
	node.Children = make([]legacyNormalizationPrefixTree, 0, legacyNormalizationPrefixFanout)
	for decoder.More() {
		if len(node.Children) == legacyNormalizationPrefixFanout {
			return domain.ErrIntegrity
		}
		var child legacyNormalizationPrefixTree
		if err := decoder.Decode(&child); err != nil {
			return domain.ErrIntegrity
		}
		node.Children = append(node.Children, child)
	}
	for _, want := range []any{json.Delim(']'), json.Delim('}')} {
		got, err := decoder.Token()
		if err != nil || got != want {
			return domain.ErrIntegrity
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return domain.ErrIntegrity
	}
	return nil
}

type legacyNormalizationPrefixEditor struct {
	chunkBytes int
	read       func(context.Context, domain.ContentHash, int) ([]byte, error)
	objects    []legacyByteCheckpointObject
}

func (editor *legacyNormalizationPrefixEditor) body(ctx context.Context, tree legacyNormalizationPrefixTree) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !tree.valid(editor.chunkBytes) || tree.Leaves == 0 || editor.read == nil {
		return nil, domain.ErrIntegrity
	}
	body, err := editor.read(ctx, tree.Hash, tree.Bytes)
	if err != nil {
		return nil, err
	}
	if len(body) != tree.Bytes {
		return nil, domain.ErrPreparedDocRangeObject
	}
	owned := bytes.Clone(body)
	hash, err := domain.HashContentContext(ctx, owned)
	if err != nil {
		return nil, err
	}
	if hash != tree.Hash {
		return nil, domain.ErrPreparedDocRangeObject
	}
	return owned, nil
}

func (editor *legacyNormalizationPrefixEditor) children(ctx context.Context, tree legacyNormalizationPrefixTree) ([]legacyNormalizationPrefixTree, error) {
	if tree.Height == 0 {
		return nil, domain.ErrIntegrity
	}
	body, err := editor.body(ctx, tree)
	if err != nil {
		return nil, err
	}
	var node legacyNormalizationPrefixNode
	if err := json.Unmarshal(body, &node); err != nil || node.Version != 1 || len(node.Children) == 0 {
		return nil, domain.ErrPreparedDocRangeObject
	}
	canonical, err := json.Marshal(node)
	if err != nil || !bytes.Equal(body, canonical) {
		return nil, domain.ErrPreparedDocRangeObject
	}
	dataBytes, leaves := 0, 0
	for ordinal, child := range node.Children {
		if !child.valid(editor.chunkBytes) || child.Leaves == 0 || child.Height+1 != tree.Height ||
			ordinal+1 < len(node.Children) && (child.Leaves != legacyNormalizationPrefixCapacity(child.Height) || child.DataBytes != child.Leaves*editor.chunkBytes) ||
			child.DataBytes > tree.DataBytes-dataBytes || child.Leaves > tree.Leaves-leaves {
			return nil, domain.ErrPreparedDocRangeObject
		}
		dataBytes, leaves = dataBytes+child.DataBytes, leaves+child.Leaves
	}
	if dataBytes != tree.DataBytes || leaves != tree.Leaves {
		return nil, domain.ErrPreparedDocRangeObject
	}
	return node.Children, nil
}

func (editor *legacyNormalizationPrefixEditor) put(ctx context.Context, body []byte) (legacyNormalizationPrefixTree, error) {
	if err := ctx.Err(); err != nil {
		return legacyNormalizationPrefixTree{}, err
	}
	if len(body) < 1 || len(body) > domain.LegacyCanonicalEncodingBytes || len(editor.objects) >= domain.DocLegacyByteBackfillBatchBlocks {
		return legacyNormalizationPrefixTree{}, domain.ErrIntegrity
	}
	owned := bytes.Clone(body)
	hash, err := domain.HashContentContext(ctx, owned)
	if err != nil {
		return legacyNormalizationPrefixTree{}, err
	}
	editor.objects = append(editor.objects, legacyByteCheckpointObject{hash: hash, data: owned})
	return legacyNormalizationPrefixTree{Hash: hash, Bytes: len(owned)}, nil
}

func (editor *legacyNormalizationPrefixEditor) branch(ctx context.Context, children []legacyNormalizationPrefixTree) (legacyNormalizationPrefixTree, error) {
	if len(children) < 1 || len(children) > legacyNormalizationPrefixFanout || children[0].Height >= 3 {
		return legacyNormalizationPrefixTree{}, domain.ErrIntegrity
	}
	body, err := json.Marshal(legacyNormalizationPrefixNode{Version: 1, Children: children})
	if err != nil {
		return legacyNormalizationPrefixTree{}, err
	}
	tree, err := editor.put(ctx, body)
	if err != nil {
		return tree, err
	}
	tree.Height = children[0].Height + 1
	for _, child := range children {
		if !child.valid(editor.chunkBytes) || child.Height+1 != tree.Height {
			return legacyNormalizationPrefixTree{}, domain.ErrIntegrity
		}
		tree.DataBytes, tree.Leaves = tree.DataBytes+child.DataBytes, tree.Leaves+child.Leaves
	}
	if !tree.valid(editor.chunkBytes) {
		return legacyNormalizationPrefixTree{}, domain.ErrIntegrity
	}
	editor.objects[len(editor.objects)-1].structure = 1
	return tree, nil
}

func (editor *legacyNormalizationPrefixEditor) path(ctx context.Context, leaf legacyNormalizationPrefixTree, height uint8) (legacyNormalizationPrefixTree, error) {
	for leaf.Height < height {
		var err error
		leaf, err = editor.branch(ctx, []legacyNormalizationPrefixTree{leaf})
		if err != nil {
			return legacyNormalizationPrefixTree{}, err
		}
	}
	return leaf, nil
}

func (editor *legacyNormalizationPrefixEditor) append(ctx context.Context, tree, leaf legacyNormalizationPrefixTree) (legacyNormalizationPrefixTree, error) {
	if tree.Leaves == 0 {
		return leaf, nil
	}
	if tree.Leaves == legacyNormalizationPrefixCapacity(tree.Height) {
		path, err := editor.path(ctx, leaf, tree.Height)
		if err != nil {
			return legacyNormalizationPrefixTree{}, err
		}
		return editor.branch(ctx, []legacyNormalizationPrefixTree{tree, path})
	}
	children, err := editor.children(ctx, tree)
	if err != nil {
		return legacyNormalizationPrefixTree{}, err
	}
	last := len(children) - 1
	if children[last].Leaves == legacyNormalizationPrefixCapacity(children[last].Height) {
		path, err := editor.path(ctx, leaf, tree.Height-1)
		if err != nil {
			return legacyNormalizationPrefixTree{}, err
		}
		children = append(children, path)
	} else {
		children[last], err = editor.append(ctx, children[last], leaf)
		if err != nil {
			return legacyNormalizationPrefixTree{}, err
		}
	}
	return editor.branch(ctx, children)
}

func (editor *legacyNormalizationPrefixEditor) replaceTail(ctx context.Context, tree, leaf legacyNormalizationPrefixTree, body []byte) (legacyNormalizationPrefixTree, error) {
	if tree.Height == 0 {
		if len(body) < tree.DataBytes {
			return legacyNormalizationPrefixTree{}, domain.ErrPreparedDocRangeObject
		}
		hash, err := domain.HashContentContext(ctx, body[:tree.DataBytes])
		if err != nil {
			return legacyNormalizationPrefixTree{}, err
		}
		if hash != tree.Hash {
			return legacyNormalizationPrefixTree{}, domain.ErrPreparedDocRangeObject
		}
		return leaf, nil
	}
	children, err := editor.children(ctx, tree)
	if err != nil {
		return legacyNormalizationPrefixTree{}, err
	}
	last := len(children) - 1
	children[last], err = editor.replaceTail(ctx, children[last], leaf, body)
	if err != nil {
		return legacyNormalizationPrefixTree{}, err
	}
	return editor.branch(ctx, children)
}

func (storage *PostgresStore) stageLegacyNormalizationPrefix(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, previous legacyNormalizationPrefix, generation uint64, chunkBytes, physicalBytes int, read func(context.Context, int, int) ([]byte, error)) (legacyNormalizationPrefix, error) {
	if err := ctx.Err(); err != nil {
		return legacyNormalizationPrefix{}, err
	}
	limit := legacyNormalizationPrefixLimit(chunkBytes)
	if !previous.valid() || generation == 0 || limit == 0 || physicalBytes < 0 || physicalBytes > limit ||
		chunkBytes == legacyNormalizationReceiptChunkBytes && physicalBytes%legacyNormalizationReceiptBytes != 0 {
		return legacyNormalizationPrefix{}, domain.ErrValidation
	}
	if previous.Stage != "" && previous.Stage != expected.stage.incarnation || generation < previous.Generation {
		return legacyNormalizationPrefix{}, domain.ErrConflict
	}
	prefix := previous
	if prefix.Generation != generation || physicalBytes == 0 {
		prefix = legacyNormalizationPrefix{Stage: expected.stage.incarnation, Generation: generation, ChunkBytes: chunkBytes}
	}
	if prefix.ChunkBytes != chunkBytes || prefix.Tree.DataBytes > physicalBytes {
		return legacyNormalizationPrefix{}, domain.ErrConflict
	}
	if prefix.Tree.DataBytes == physicalBytes {
		if err := storage.touchLegacyNormalizationPrefix(ctx, job, expected, prefix.Tree); err != nil {
			return legacyNormalizationPrefix{}, err
		}
		return prefix, nil
	}
	if read == nil {
		return legacyNormalizationPrefix{}, domain.ErrValidation
	}
	if err := storage.touchLegacyNormalizationPrefix(ctx, job, expected, prefix.Tree); err != nil {
		return legacyNormalizationPrefix{}, err
	}
	offset := prefix.Tree.DataBytes
	replace := offset%chunkBytes != 0
	if replace {
		offset -= offset % chunkBytes
	}
	length := min(chunkBytes, physicalBytes-offset)
	body, err := read(ctx, offset, length)
	if canceled := ctx.Err(); canceled != nil {
		return legacyNormalizationPrefix{}, canceled
	}
	if err != nil {
		return legacyNormalizationPrefix{}, err
	}
	if len(body) != length {
		return legacyNormalizationPrefix{}, domain.ErrPreparedDocRangeObject
	}
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return legacyNormalizationPrefix{}, err
	}
	defer rollbackPG(transaction)
	editor := legacyNormalizationPrefixEditor{chunkBytes: chunkBytes, read: func(ctx context.Context, hash domain.ContentHash, size int) ([]byte, error) {
		return readLegacyNormalizationCheckpointObject(ctx, transaction, job, state, hash, size)
	}}
	leaf, err := editor.put(ctx, body)
	if err != nil {
		return legacyNormalizationPrefix{}, err
	}
	leaf.DataBytes, leaf.Leaves = len(body), 1
	if replace {
		prefix.Tree, err = editor.replaceTail(ctx, prefix.Tree, leaf, editor.objects[0].data)
	} else {
		prefix.Tree, err = editor.append(ctx, prefix.Tree, leaf)
	}
	if err != nil {
		return legacyNormalizationPrefix{}, err
	}
	if !prefix.valid() {
		return legacyNormalizationPrefix{}, domain.ErrIntegrity
	}
	objects, err := legacyByteCheckpointObjects(ctx, editor.objects)
	if err != nil {
		return legacyNormalizationPrefix{}, err
	}
	if err := putLegacyByteCheckpointObjects(ctx, transaction, job, state, objects); err != nil {
		return legacyNormalizationPrefix{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return legacyNormalizationPrefix{}, err
	}
	return prefix, nil
}

func (storage *PostgresStore) readLegacyNormalizationPrefix(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, prefix legacyNormalizationPrefix, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !prefix.valid() || prefix.Generation == 0 || offset < 0 || length < 0 || length > prefix.ChunkBytes || offset > prefix.Tree.DataBytes || length > prefix.Tree.DataBytes-offset {
		return nil, domain.ErrIntegrity
	}
	if prefix.Stage != expected.stage.incarnation {
		return nil, domain.ErrConflict
	}
	transaction, state, err := storage.openLegacyByteCheckpoint(ctx, job, expected.stage, &expected, false)
	if err != nil {
		return nil, err
	}
	defer rollbackPG(transaction)
	result, err := readLegacyNormalizationPrefix(ctx, transaction, job, state, prefix, offset, length)
	if err != nil {
		return nil, err
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		return nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func readLegacyNormalizationPrefix(ctx context.Context, transaction pgx.Tx, job domain.DocRangeIndexJob, state legacyByteCheckpoint, prefix legacyNormalizationPrefix, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !prefix.valid() || prefix.Generation == 0 || offset < 0 || length < 0 || length > prefix.ChunkBytes || offset > prefix.Tree.DataBytes || length > prefix.Tree.DataBytes-offset {
		return nil, domain.ErrIntegrity
	}
	if prefix.Stage != state.stage.incarnation {
		return nil, domain.ErrConflict
	}
	editor := legacyNormalizationPrefixEditor{chunkBytes: prefix.ChunkBytes, read: func(ctx context.Context, hash domain.ContentHash, size int) ([]byte, error) {
		return readLegacyNormalizationCheckpointObject(ctx, transaction, job, state, hash, size)
	}}
	result := make([]byte, 0, length)
	for len(result) < length {
		tree, start := prefix.Tree, 0
		position := offset + len(result)
		for tree.Height > 0 {
			children, err := editor.children(ctx, tree)
			if err != nil {
				return nil, err
			}
			found := false
			for _, child := range children {
				if position-start < child.DataBytes {
					tree, found = child, true
					break
				}
				start += child.DataBytes
			}
			if !found {
				return nil, domain.ErrPreparedDocRangeObject
			}
		}
		body, err := editor.body(ctx, tree)
		if err != nil {
			return nil, err
		}
		from := position - start
		if from < 0 || from >= len(body) {
			return nil, domain.ErrPreparedDocRangeObject
		}
		count := min(length-len(result), len(body)-from)
		result = append(result, body[from:from+count]...)
	}
	return result, ctx.Err()
}
