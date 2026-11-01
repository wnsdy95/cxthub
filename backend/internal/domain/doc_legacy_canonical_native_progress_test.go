package domain

import (
	"context"
	"fmt"
	"testing"
	"unsafe"
)

func assertLegacyCanonicalNumberPendingConsumerState(test testing.TB, encoding *LegacyCanonicalEncoding, preparedKeyWork ...uint64) {
	test.Helper()
	var prepared uint64
	if len(preparedKeyWork) > 1 {
		test.Fatal("numeric consumer received more than one preparation baseline")
	}
	if len(preparedKeyWork) == 1 {
		prepared = preparedKeyWork[0]
	}
	if encoding.keyWork != prepared || prepared != 0 && (encoding.nativeOwner == nil || encoding.inspectWork == 0 || encoding.mapKeys.length() == 0) {
		test.Fatal("canonical ordering advanced during numeric validation or lacks completed native preparation")
	}
	if encoding.nativeOwner == nil {
		assertLegacyCanonicalNumberPendingState(test, encoding)
		return
	}
	state := *encoding
	state.nativeOwner = nil
	state.keyWork = 0
	assertLegacyCanonicalNumberPendingState(test, &state)
	var expected docRangeHeaderHeapEstimate
	expected.bytes, expected.err = state.RetainedBytes()
	expected.allocation(1, int64(unsafe.Sizeof(*encoding.nativeOwner)))
	retained, err := encoding.RetainedBytes()
	if err != nil || expected.err != nil || retained != expected.bytes {
		test.Fatal("pending encoding omitted native ownership storage", retained, expected.bytes, err)
	}
}

type legacyCanonicalNativeProgressFailure string

type legacyCanonicalNativeProgressProbe struct {
	testing.TB
}

func (probe legacyCanonicalNativeProgressProbe) Fatal(arguments ...any) {
	panic(legacyCanonicalNativeProgressFailure(fmt.Sprint(arguments...)))
}

func TestLegacyCanonicalNativeCollectorsRejectUninitializedPreparation(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		collect func(testing.TB, *LegacyCanonicalEncoding)
		failure legacyCanonicalNativeProgressFailure
	}{
		{
			name:    "encoding",
			failure: legacyCanonicalNativeProgressFailure(fmt.Sprint(ErrIntegrity)),
			collect: func(probe testing.TB, encoding *LegacyCanonicalEncoding) {
				collectLegacyCanonicalEncoding(probe, encoding)
			},
		},
		{
			name:    "number-pending",
			failure: legacyCanonicalNativeProgressFailure(fmt.Sprint("pending encoding failed or escaped its output quantum", 0, 0, ErrIntegrity)),
			collect: func(probe testing.TB, encoding *LegacyCanonicalEncoding) {
				collectLegacyCanonicalNumberPending(probe, encoding)
			},
		},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			encoding := &LegacyCanonicalEncoding{
				root:        true,
				initialized: true,
				preparation: &legacyCanonicalPreparation{},
			}
			encoding.numbers.prepare("1")
			defer func() {
				recovered := recover()
				failure, failed := recovered.(legacyCanonicalNativeProgressFailure)
				if !failed || failure != candidate.failure {
					test.Fatal("collector accepted an uninitialized preparation or failed for another reason", recovered)
				}
			}()
			candidate.collect(legacyCanonicalNativeProgressProbe{TB: test}, encoding)
		})
	}
}

func TestLegacyCanonicalNativeNumericConsumerFreezesCompletedPreparationOrdering(test *testing.T) {
	fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"compaction","replacement":[{"kind":"turn","seq":2,"provider_metadata":{"create_time":1e-7}},{"kind":"turn","seq":1}]}`)
	ready := false
	for quantum := 0; quantum < 128; quantum++ {
		_, complete, err := fixture.step(test, context.Background(), 0)
		if err != nil {
			test.Fatal(err)
		}
		if complete {
			ready = true
			break
		}
	}
	if !ready {
		test.Fatal("native numeric fixture did not finish")
	}
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil {
		test.Fatal(err)
	}
	defer encoding.release()
	for quantum := 0; encoding.preparation != nil && quantum < 128; quantum++ {
		body, complete, err := encoding.Next(context.Background())
		if err != nil || len(body) != 0 || complete {
			test.Fatal("native numeric fixture emitted before preparation", complete, err)
		}
	}
	prepared := encoding.keyWork
	if encoding.preparation != nil || prepared == 0 || encoding.numbers.next == encoding.numbers.pending.length() {
		test.Fatal("fixture did not retain actual completed ordering and pending numbers")
	}
	assertLegacyCanonicalNumberPendingConsumerState(test, encoding, prepared)
	for _, mutation := range []string{"ordering", "emission", "owner"} {
		test.Run(mutation, func(test *testing.T) {
			state := *encoding
			if mutation == "ordering" {
				state.keyWork++
			} else if mutation == "owner" {
				state.nativeOwner = nil
			} else {
				state.frames = legacyCanonicalPages[legacyCanonicalEncodingFrame]{}
				if err := state.frames.append(legacyCanonicalEncodingFrame{kind: 'o'}); err != nil {
					test.Fatal(err)
				}
			}
			defer func() {
				if _, rejected := recover().(legacyCanonicalNativeProgressFailure); !rejected {
					test.Fatal("numeric consumer accepted changed ordering, started emission, or missing native ownership")
				}
			}()
			assertLegacyCanonicalNumberPendingConsumerState(legacyCanonicalNativeProgressProbe{TB: test}, &state, prepared)
		})
	}
}
