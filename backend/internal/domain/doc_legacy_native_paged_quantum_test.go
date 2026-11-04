package domain

import (
	"context"
	"testing"
	"unsafe"
)

func TestLegacyNormalizationReplacementDoesNotGrowContiguousNativeArray(test *testing.T) {
	fixture := legacyNativeEventsL06Source(test, legacyNativeEventsL06Case{children: 8192, text: 64})
	buffer := &legacySourceProofBuffer{body: fixture.raw}
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	for quantum := 0; quantum < 4096; quantum++ {
		step, err := decoder.Step(context.Background())
		if err != nil {
			if decoder.nativeCache != nil {
				test.Logf("normalizer phase=%d object=%+v", decoder.phase, decoder.nativeCache.object)
				for _, list := range decoder.nativeCache.object.events {
					if list != nil {
						test.Logf("list phase=%d count=%d paged=%t values=%p heap=%+v borrowed=%+v", list.phase, list.count, list.paged, list.values, list.estimate, list.borrowed)
					}
				}
			}
			test.Fatal(quantum, err)
		}
		if decoder.nativeCache != nil {
			for _, list := range decoder.nativeCache.object.events {
				if list != nil && uintptr(cap(list.decoded))*unsafe.Sizeof(CIREvent{}) > LegacyCanonicalEncodingBytes {
					test.Fatal("actual normalization grew an indivisible native replacement allocation", quantum, cap(list.decoded), uintptr(cap(list.decoded))*unsafe.Sizeof(CIREvent{}))
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
			test.Fatal("actual replacement fixture did not emit an event")
		}
	}
	test.Fatal("actual normalization replacement exhausted its finite quantum allowance")
}
