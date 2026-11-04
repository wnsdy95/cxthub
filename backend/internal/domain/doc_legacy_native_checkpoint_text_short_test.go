package domain

import (
	"bytes"
	"context"
	"strconv"
	"testing"
)

func TestLegacyNativeCheckpointShortTextKeepsReadWriteBoundary(test *testing.T) {
	for _, size := range []int{1, LegacyCanonicalEncodingBytes - 1, LegacyCanonicalEncodingBytes} {
		test.Run(strconv.Itoa(size), func(test *testing.T) {
			ctx := context.Background()
			store := legacyNativeCheckpointStringsOracleStorage(test)
			body := bytes.Repeat([]byte("x"), size)
			original := bytes.Clone(body)
			reads := 0
			work, err := newLegacyNativeCheckpointTextWriter(ctx, size, func(current context.Context, offset, length int) ([]byte, error) {
				if err := current.Err(); err != nil {
					return nil, err
				}
				reads++
				if offset != 0 || length != size {
					test.Fatal("short text changed the original single leaf range", offset, length)
				}
				return body, nil
			}, store.io)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if ready, err := work.step(ctx); ready || err != nil || reads != 1 || store.writes != 0 {
				test.Fatal("read did not yield before CAS", ready, err, reads, store.writes)
			}
			body[0] ^= 255
			if ready, err := work.step(ctx); !ready || err != nil || reads != 1 || store.writes != 1 || store.bytes != size {
				test.Fatal("single-leaf write needed empty indexing steps or escaped IO bounds", ready, err, reads, store.writes, store.bytes)
			}
			saved, err := work.take(ctx)
			if err != nil || saved.Bytes != size || saved.Tree.Count != uint64(size) || saved.Tree.Height != 0 || saved.Tree.Ref.Hash != HashContent(original) || saved.Tree.Ref.Bytes != size || !bytes.Equal(store.objects[saved.Tree.Ref.Hash], original) {
				test.Fatal("single-leaf completion changed the owned bytes or existing tree", saved, err)
			}
			if retained, err := work.retainedBytes(); retained != 0 || err != nil {
				test.Fatal("completed text retained owned bytes", retained, err)
			}
		})
	}
}
