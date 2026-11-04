package domain

import (
	"context"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyNormalizationBlocksDoNotGrowContiguousNativeArrays(test *testing.T) {
	block := `{"type":"text","text":"` + strings.Repeat("x", 64) + `"}`
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","role":"assistant","blocks":[` +
		strings.Repeat(block+",", 8191) + block + `]}]}`)
	buffer := &legacySourceProofBuffer{body: raw}
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	for quantum := 0; quantum < 4096; quantum++ {
		step, err := decoder.Step(context.Background())
		if err != nil {
			test.Fatal(quantum, err)
		}
		if decoder.nativeCache != nil {
			for _, list := range decoder.nativeCache.object.blocks {
				if list == nil {
					continue
				}
				allocations := []uintptr{
					uintptr(cap(list.decoded)) * unsafe.Sizeof(ContentBlock{}),
					uintptr(cap(list.previous)) * unsafe.Sizeof(ContentBlock{}),
					uintptr(cap(list.payloads)) * unsafe.Sizeof(docRangeHeaderHeapEstimate{}),
					uintptr(cap(list.previousHeaps)) * unsafe.Sizeof(docRangeHeaderHeapEstimate{}),
					uintptr(cap(list.excess)) * unsafe.Sizeof(int64(0)),
					uintptr(cap(list.previousExtra)) * unsafe.Sizeof(int64(0)),
				}
				for _, allocation := range allocations {
					if allocation > LegacyCanonicalEncodingBytes {
						test.Fatal("actual normalization grew an indivisible native block allocation", quantum, allocation)
					}
				}
			}
		}
		if len(step.Events) != 0 {
			for _, event := range step.Events {
				event.Encoding.release()
			}
			return
		}
		if step.Complete {
			test.Fatal("actual block fixture did not emit an event")
		}
	}
	test.Fatal("actual normalization blocks exhausted their finite quantum allowance")
}
