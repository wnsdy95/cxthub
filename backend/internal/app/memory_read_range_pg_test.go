//go:build postgres

package app

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryReadRangeMaximumPageAndSkippedPrefix(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx := systemTestContext()
	snapshot := collaborationSnapshot(test, storage, repo, "range page limits")
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "page fixture"))
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: effectiveOID(3)}, Limit: 50}
	captured, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal(err)
	}
	input, err := captured.Export(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.WriteMemoryCapture(ctx, input, time.Now().Add(time.Hour)); err != nil {
		test.Fatal(err)
	}
	original, err := BuildMemoryRead(ctx, input, memoryBundleFactory{})
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []string{"fifty-small-chunks", "large-skipped-prefix-and-tail"} {
		test.Run(scenario, func(test *testing.T) {
			sizes := make([]int, 60)
			for ordinal := range sizes {
				sizes[ordinal] = 512
			}
			offset, want := 0, 50
			if scenario == "large-skipped-prefix-and-tail" {
				sizes = []int{effectiveMemoryPageBytes - 1024, 512, effectiveMemoryPageBytes - 1024, 1536, effectiveMemoryPageBytes - 2048}
				offset, want = 1, 2
			}
			items := make([]domain.EffectiveMemoryItem, len(sizes))
			for ordinal, size := range sizes {
				items[ordinal] = memoryReadSizedItem(test, snapshot, ordinal, size)
			}
			header := original.Header
			header.Chunks = nil
			header.View.Total = len(items)
			header.View.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(ctx, repo, header.Origin, header.View, items)
			if err != nil {
				test.Fatal(err)
			}
			bundle, err := domain.BuildMemoryReadBundle(ctx, header, items)
			if err != nil {
				test.Fatal(err)
			}
			if scenario == "fifty-small-chunks" {
				bundle.Header.Chunks, bundle.Chunks = nil, nil
				for ordinal, item := range items {
					raw, err := json.Marshal([]domain.EffectiveMemoryItem{item})
					if err != nil {
						test.Fatal(err)
					}
					digest := domain.HashContent(raw)
					bundle.Chunks = append(bundle.Chunks, domain.MemoryReadChunk{Hash: digest, Bytes: raw})
					bundle.Header.Chunks = append(bundle.Header.Chunks, domain.MemoryReadChunkRef{Hash: digest, Start: ordinal, Count: 1, Bytes: len(raw)})
				}
				raw, err := domain.MemoryReadHeaderBytes(ctx, bundle.Header)
				if err != nil {
					test.Fatal(err)
				}
				bundle.ID = domain.HashContent(raw)
			} else if len(bundle.Chunks) != 3 {
				test.Fatal("large boundary fixture did not span three chunks")
			}
			if err := storage.WriteMemoryRead(ctx, bundle); err != nil {
				test.Fatal(err)
			}
			request.Cursor = memoryReadCursor(test, header.View.StateHash, offset)
			page, err := ReadMemoryReadPage(ctx, storage, repo, bundle.ID, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[offset:offset+want]) || page.NextCursor != memoryReadCursor(test, header.View.StateHash, offset+want) {
				test.Fatal("valid full page rejected or changed", len(page.Items), err)
			}
			request.Cursor = page.NextCursor
			page, err = ReadMemoryReadPage(ctx, storage, repo, bundle.ID, request)
			if err != nil || !reflect.DeepEqual(page.Items, items[offset+want:]) || page.NextCursor != "" {
				test.Fatal("continuation lost items", len(page.Items), err)
			}
		})
	}
}
