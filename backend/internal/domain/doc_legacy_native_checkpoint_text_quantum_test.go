package domain

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

func TestLegacyNativeCheckpointTextW86IOPhases(test *testing.T) {
	for _, parts := range []int{0, 1, 33} {
		test.Run(fmt.Sprintf("parts-%d", parts), func(test *testing.T) {
			storage := legacyNativeGoGraphSetW85Storage(test)
			ctx := context.Background()
			storage.current = ctx
			refs := make([]legacyNativeCheckpointRef, parts)
			var expected legacyNativeCheckpointTreeBuilder
			defer expected.close()
			for position := range refs {
				body := bytes.Repeat([]byte{byte(1 + position%251)}, LegacyCanonicalEncodingBytes)
				ref, err := legacyNativeCheckpointWrite(ctx, storage.io, body)
				if err != nil {
					test.Fatal(err)
				}
				refs[position] = ref
				if err := expected.append(ctx, storage.io, ref, uint64(len(body))); err != nil {
					test.Fatal(err)
				}
			}
			tree, err := expected.finish(ctx, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			storage.current = nil
			var borrowed []byte
			read := func(current context.Context, offset, length int) ([]byte, error) {
				if offset%LegacyCanonicalEncodingBytes != 0 || length != LegacyCanonicalEncodingBytes {
					test.Fatal("text writer changed its original leaf boundaries", offset, length)
				}
				part := refs[offset/LegacyCanonicalEncodingBytes]
				body, err := storage.io.Read(current, part.Hash, part.Bytes)
				borrowed = body
				return body, err
			}
			work, err := newLegacyNativeCheckpointTextWriter(ctx, parts*LegacyCanonicalEncodingBytes, read, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ready := false
			for steps := 0; steps < 8*max(parts, 1)+32; steps++ {
				ready, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointText](test, work, storage)
				if err != nil {
					test.Fatal(err)
				}
				if borrowed != nil {
					borrowed[0] ^= 255
					borrowed = nil
				}
				if ready {
					break
				}
				retained, err := work.retainedBytes()
				if err != nil || retained <= 0 || retained > legacyNativeAnyCheckpointScratch {
					test.Fatal("text phase did not account for its owned leaf", retained, err)
				}
			}
			if !ready {
				test.Fatal("text phases did not finish within their finite schedule")
			}
			reads, writes := storage.reads, storage.writes
			saved, err := work.take(ctx)
			if err != nil || saved.Bytes != parts*LegacyCanonicalEncodingBytes || saved.Tree != tree || storage.reads != reads || storage.writes != writes {
				test.Fatal("phased text changed canonical leaves or performed IO during handoff", err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointText](test, work)
		})
	}
}
