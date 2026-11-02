//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyNormalizationPrefixPGIncrementalTailAndGeneration(test *testing.T) {
	storage, ctx, _, job, expected := legacyCheckpointPGStart(test)
	spools, err := newLegacyNormalizationSpool(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer spools.Close()
	const chunk = domain.LegacyCanonicalEncodingBytes
	first := bytes.Repeat([]byte("a"), chunk+17)
	if err := spools.Append(ctx, 0, first); err != nil {
		test.Fatal(err)
	}
	var reads [][2]int
	read := func(ctx context.Context, offset, length int) ([]byte, error) {
		reads = append(reads, [2]int{offset, length})
		return spools.Read(ctx, 0, offset, length)
	}
	var prefix legacyNormalizationPrefix
	for prefix.Tree.DataBytes < len(first) {
		prefix, err = storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, chunk, len(first), read)
		if err != nil {
			test.Fatal(err)
		}
	}
	old := prefix
	if len(reads) != 2 || reads[0] != [2]int{0, chunk} || reads[1] != [2]int{chunk, 17} {
		test.Fatal("initial prefix did not advance in bounded selected ranges", reads)
	}
	if _, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, chunk, len(first), read); err != nil || len(reads) != 2 {
		test.Fatal("unchanged prefix replayed file bodies", reads, err)
	}
	extra := bytes.Repeat([]byte("b"), chunk+50)
	if err := spools.Append(ctx, 0, extra); err != nil {
		test.Fatal(err)
	}
	want := append(bytes.Clone(first), extra...)
	for prefix.Tree.DataBytes < len(want) {
		prefix, err = storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, chunk, len(want), read)
		if err != nil {
			test.Fatal(err)
		}
	}
	if len(reads) != 4 || reads[2] != [2]int{chunk, chunk} || reads[3] != [2]int{2 * chunk, 67} {
		test.Fatal("append replayed the sealed prefix instead of replacing its partial tail", reads)
	}
	body, err := storage.readLegacyNormalizationPrefix(ctx, job, expected, prefix, chunk-9, 40)
	if err != nil || !bytes.Equal(body, want[chunk-9:chunk+31]) {
		test.Fatal("cross-leaf range did not return exact current bytes", err)
	}
	body, err = storage.readLegacyNormalizationPrefix(ctx, job, expected, old, chunk-9, 26)
	if err != nil || !bytes.Equal(body, first[chunk-9:]) {
		test.Fatal("tail replacement overwrote the old immutable prefix", err)
	}
	if err := spools.Reset(ctx, 0); err != nil {
		test.Fatal(err)
	}
	if err := spools.Append(ctx, 0, []byte("reset")); err != nil {
		test.Fatal(err)
	}
	reset, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 2, chunk, 5, read)
	if err != nil || reset.Generation != 2 || reset.Tree.DataBytes != 5 || reset.Tree.Leaves != 1 {
		test.Fatal("reset reused a prior generation's data/index", reset, err)
	}
	if body, err := storage.readLegacyNormalizationPrefix(ctx, job, expected, reset, 0, 5); err != nil || string(body) != "reset" {
		test.Fatal(err)
	}
	retired, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, reset, 2, chunk, 0, nil)
	if err != nil || retired.Tree != (legacyNormalizationPrefixTree{}) {
		test.Fatal("logical retirement kept unused data reachable", retired, err)
	}
}

func TestLegacyNormalizationPrefixPGTreeGrowthAndReceipts(test *testing.T) {
	storage, ctx, _, job, expected := legacyCheckpointPGStart(test)
	for _, chunk := range []int{domain.LegacyCanonicalEncodingBytes, legacyNormalizationReceiptChunkBytes} {
		limit := 33*chunk + chunk/2
		if chunk == legacyNormalizationReceiptChunkBytes {
			limit = 31*chunk + 12*legacyNormalizationReceiptBytes
		}
		body := bytes.Repeat([]byte("x"), limit)
		var prefix legacyNormalizationPrefix
		reads := 0
		for prefix.Tree.DataBytes < len(body) {
			before := prefix.Tree.DataBytes
			next, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, chunk, len(body), func(ctx context.Context, offset, length int) ([]byte, error) {
				reads++
				if offset != before || length > chunk {
					test.Fatal("tree growth reread the full prefix", offset, before, length)
				}
				return body[offset : offset+length], ctx.Err()
			})
			if err != nil {
				test.Fatal(err)
			}
			prefix = next
		}
		if prefix.Tree.Height != 2 && chunk == domain.LegacyCanonicalEncodingBytes || reads != (len(body)+chunk-1)/chunk {
			test.Fatal("fixture did not cross the immutable index fanout", prefix, reads)
		}
		for _, offset := range []int{0, chunk - 3, 17 * chunk, len(body) - 11} {
			length := min(19, len(body)-offset)
			selected, err := storage.readLegacyNormalizationPrefix(ctx, job, expected, prefix, offset, length)
			if err != nil || !bytes.Equal(selected, body[offset:offset+length]) {
				test.Fatal("selected prefix range differs across index depth", offset, err)
			}
		}
	}
}

func TestLegacyNormalizationPrefixPGCurrentRootAndClaimFences(test *testing.T) {
	storage, ctx, fixture, job, expected := legacyCheckpointPGStart(test)
	called := false
	read := func(ctx context.Context, offset, length int) ([]byte, error) {
		called = true
		return []byte("data"), ctx.Err()
	}
	prefix, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, legacyNormalizationPrefix{}, 1, domain.LegacyCanonicalEncodingBytes, 4, read)
	if err != nil {
		test.Fatal(err)
	}
	root := legacyCheckpointPGObject("new-root")
	current, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, expected, root.hash, len(root.data), []legacyByteCheckpointObject{root})
	if err != nil {
		test.Fatal(err)
	}
	called = false
	if _, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 2, domain.LegacyCanonicalEncodingBytes, 4, read); !errors.Is(err, domain.ErrConflict) || called {
		test.Fatal("stale root read a local file before rejecting its fence", called, err)
	}
	if _, err := storage.readLegacyNormalizationPrefix(ctx, job, expected, prefix, 0, 1); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale root returned prefix bytes", err)
	}
	legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "lease expiry")
	if _, err := storage.stageLegacyNormalizationPrefix(ctx, job, current, prefix, 2, domain.LegacyCanonicalEncodingBytes, 4, read); !errors.Is(err, domain.ErrConflict) || called {
		test.Fatal("expired claim read a local file before rejecting its fence", called, err)
	}
}

func TestLegacyNormalizationPrefixPGRejectsMalformedIndex(test *testing.T) {
	storage, ctx, _, job, expected := legacyCheckpointPGStart(test)
	leaf := legacyCheckpointPGObject("leaf")
	child := legacyNormalizationPrefixTree{Hash: leaf.hash, Bytes: len(leaf.data), DataBytes: len(leaf.data), Leaves: 1}
	body, err := json.Marshal(legacyNormalizationPrefixNode{Version: 1, Children: []legacyNormalizationPrefixTree{child, child}})
	if err != nil {
		test.Fatal(err)
	}
	index := legacyByteCheckpointObject{hash: domain.HashContent(body), data: body}
	if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, expected, []legacyByteCheckpointObject{leaf, index}); err != nil {
		test.Fatal(err)
	}
	prefix := legacyNormalizationPrefix{Stage: expected.stage.incarnation, Generation: 1, ChunkBytes: domain.LegacyCanonicalEncodingBytes,
		Tree: legacyNormalizationPrefixTree{Hash: index.hash, Bytes: len(body), DataBytes: domain.LegacyCanonicalEncodingBytes + len(leaf.data), Leaves: 2, Height: 1}}
	if _, err := storage.readLegacyNormalizationPrefix(ctx, job, expected, prefix, 0, 1); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("index accepted incomplete non-tail children or false byte totals", err)
	}
}

func TestLegacyNormalizationPrefixPGRejectsRecreatedStageAndBackwardGeneration(test *testing.T) {
	storage, ctx, _, job, expected := legacyCheckpointPGStart(test)
	read := func(ctx context.Context, offset, length int) ([]byte, error) { return []byte("data"), ctx.Err() }
	prefix, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, legacyNormalizationPrefix{}, 2, domain.LegacyCanonicalEncodingBytes, 4, read)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, domain.LegacyCanonicalEncodingBytes, 4, read); !errors.Is(err, domain.ErrConflict) {
		test.Error("prefix generation moved backward without changing its checkpoint", err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash); err != nil {
		test.Fatal(err)
	}
	source := expected.stage
	source.incarnation = ""
	if _, err := storage.ensureLegacyNormalizationStage(ctx, job, source); err != nil {
		test.Fatal(err)
	}
	current, _, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || current.stage.incarnation == expected.stage.incarnation {
		test.Fatal("fixture did not recreate its stage", current, err)
	}
	called := false
	if _, err := storage.stageLegacyNormalizationPrefix(ctx, job, current, prefix, 2, domain.LegacyCanonicalEncodingBytes, 4, func(ctx context.Context, offset, length int) ([]byte, error) {
		called = true
		return []byte("data"), ctx.Err()
	}); !errors.Is(err, domain.ErrConflict) || called {
		test.Fatal("unchanged prefix shortcut reused an old stage's absent objects", called, err)
	}
}

func TestLegacyNormalizationPrefixPGRejectsChangingPartialTail(test *testing.T) {
	storage, ctx, _, job, expected := legacyCheckpointPGStart(test)
	prefix, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, legacyNormalizationPrefix{}, 1, domain.LegacyCanonicalEncodingBytes, 4,
		func(ctx context.Context, offset, length int) ([]byte, error) { return []byte("data"), ctx.Err() })
	if err != nil {
		test.Fatal(err)
	}
	before := legacyByteUsagePGRead(test, storage, ctx)
	if _, err := storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, domain.LegacyCanonicalEncodingBytes, 5,
		func(ctx context.Context, offset, length int) ([]byte, error) { return []byte("Xata!"), ctx.Err() }); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("tail replacement changed already-stored bytes in the same generation", err)
	}
	if after := legacyByteUsagePGRead(test, storage, ctx); after != before {
		test.Fatal("rejected tail replacement leaked immutable objects or accounting", before, after)
	}
	if body, err := storage.readLegacyNormalizationPrefix(ctx, job, expected, prefix, 0, 4); err != nil || string(body) != "data" {
		test.Fatal("rejected tail replacement damaged its prior immutable root", err)
	}
}

func TestLegacyNormalizationPrefixPGPreservesFenceErrorDuringCopy(test *testing.T) {
	storage, ctx, _, job, expected := legacyCheckpointPGStart(test)
	const chunk = domain.LegacyCanonicalEncodingBytes
	body := bytes.Repeat([]byte("x"), 3*chunk)
	read := func(ctx context.Context, offset, length int) ([]byte, error) {
		return body[offset : offset+length], ctx.Err()
	}
	var prefix legacyNormalizationPrefix
	var err error
	for prefix.Tree.DataBytes < 2*chunk {
		prefix, err = storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, chunk, 2*chunk, read)
		if err != nil {
			test.Fatal(err)
		}
	}
	root := legacyCheckpointPGObject("after-local-read")
	_, err = storage.stageLegacyNormalizationPrefix(ctx, job, expected, prefix, 1, chunk, len(body), func(ctx context.Context, offset, length int) ([]byte, error) {
		if _, err := storage.saveLegacyNormalizationCheckpoint(ctx, job, expected, root.hash, len(root.data), []legacyByteCheckpointObject{root}); err != nil {
			test.Fatal(err)
		}
		return read(ctx, offset, length)
	})
	if !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("root conflict during metadata lookup was misclassified as source corruption", err)
	}
}
