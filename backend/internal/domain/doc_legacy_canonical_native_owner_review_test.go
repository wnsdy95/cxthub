package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

type legacyCanonicalNativeOwnerReviewClaimContext struct {
	context.Context
	owner *legacyNativeCanonicalOwner
}

func (ctx legacyCanonicalNativeOwnerReviewClaimContext) Err() error {
	if ctx.owner.seal != ctx.owner {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func legacyCanonicalNativeOwnerReviewOracle(test *testing.T, event CIREvent) []byte {
	test.Helper()
	value, err := canonicalEventJSONObject(event)
	if err != nil {
		test.Fatal("owner review oracle rejected its event", err)
	}
	want, err := json.Marshal(value)
	if err != nil {
		test.Fatal("owner review oracle could not marshal its event", err)
	}
	return want
}

func legacyCanonicalNativeOwnerReviewDrain(test *testing.T, encoding *LegacyCanonicalEncoding) []byte {
	test.Helper()
	var output []byte
	for stepIndex := 0; stepIndex < 64; stepIndex++ {
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatal("owner review stream failed or exceeded its existing quantum", stepIndex, err, len(chunk), cap(chunk))
		}
		output = append(output, chunk...)
		if complete {
			return output
		}
	}
	test.Fatal("owner review exhausted its finite streaming allowance")
	return nil
}

func TestLegacyCanonicalNativeOwnerReviewConstructorPreclaimErrors(test *testing.T) {
	fixture := legacyNativeCanonicalOwnerStart(test, `{"kind":"tool_result","seq":7,"output":{"payload":"old"}}`)
	event := fixture.drain(test)
	want := legacyCanonicalNativeOwnerReviewOracle(test, event)
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil {
		test.Fatal("owner review transfer failed", err)
	}
	defer owner.release()
	heap := owner.heap
	reads := len(fixture.buffer.reads)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	encoding, err := newLegacyCanonicalEncodingNative(canceled, owner)
	if encoding != nil || !errors.Is(err, context.Canceled) || owner.seal != owner || owner.heap != heap || !reflect.DeepEqual(owner.event, event) {
		test.Fatal("preclaim constructor cancellation consumed or exposed the owner", encoding != nil, err)
	}
	copied := *owner
	encoding, err = newLegacyCanonicalEncodingNative(context.Background(), &copied)
	if encoding != nil || !errors.Is(err, ErrIntegrity) || owner.seal != owner || owner.heap != heap {
		test.Fatal("copied-owner constructor rejection consumed the original", encoding != nil, err)
	}
	copied.release()
	encoding, err = newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil || encoding == nil || encoding.nativeOwner != owner || owner.seal != nil {
		test.Fatal("preclaim constructor rejection prevented a valid retry", err)
	}
	defer encoding.release()
	retained, err := encoding.RetainedBytes()
	if err != nil {
		test.Fatal("live owner lost its retained estimate", err)
	}
	duplicate, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if duplicate != nil || !errors.Is(err, ErrIntegrity) || encoding.nativeOwner != owner || owner.heap != heap || !reflect.DeepEqual(owner.event, event) {
		test.Fatal("duplicate constructor rejection released a live encoder's owner", duplicate != nil, err)
	}
	after, err := encoding.RetainedBytes()
	if err != nil || after != retained || len(fixture.buffer.reads) != reads {
		test.Fatal("preclaim constructor errors changed retention or reread source", retained, after, err)
	}
	actual := legacyCanonicalNativeOwnerReviewDrain(test, encoding)
	if !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
		test.Fatal("preclaim constructor errors changed the original stream")
	}
}

func TestLegacyCanonicalNativeOwnerReviewConstructorPostclaimErrors(test *testing.T) {
	for _, failure := range []string{"projection", "projection-heap-overflow", "retained-overflow", "canceled-after-claim"} {
		test.Run(failure, func(test *testing.T) {
			raw := `{"kind":"tool_result","seq":7,"output":{"payload":"old"}}`
			if failure == "projection" {
				raw = `{"kind":"unknown","seq":7,"output":{"payload":"old"}}`
			}
			fixture := legacyNativeCanonicalOwnerStart(test, raw)
			event := fixture.drain(test)
			owner, err := fixture.native.takeCanonicalOwner(context.Background())
			if err != nil {
				test.Fatal("postclaim review transfer failed", err)
			}
			defer owner.release()
			reads := len(fixture.buffer.reads)
			ctx := context.Background()
			expected := error(ErrIntegrity)
			switch failure {
			case "projection":
				_, expected = legacyCanonicalEventJSONObject(event)
				if expected == nil {
					test.Fatal("projection fault fixture unexpectedly became canonical")
				}
			case "projection-heap-overflow":
				owner.heap.bytes = math.MaxInt64
			case "retained-overflow":
				value, err := legacyCanonicalEventJSONObject(event)
				if err != nil {
					test.Fatal("retained overflow fixture lost its projection", err)
				}
				projection := legacyCanonicalNativeProjectionHeap(event, value)
				if projection.err != nil || projection.bytes <= 0 {
					test.Fatal("retained overflow fixture lost its projection estimate", projection)
				}
				owner.heap.bytes = math.MaxInt64 - projection.bytes
			case "canceled-after-claim":
				ctx = legacyCanonicalNativeOwnerReviewClaimContext{Context: context.Background(), owner: owner}
				expected = context.Canceled
			}
			encoding, err := newLegacyCanonicalEncodingNative(ctx, owner)
			if encoding != nil || err == nil || err.Error() != expected.Error() || failure != "projection" && !errors.Is(err, expected) {
				test.Fatal("postclaim constructor failure exposed an encoder or changed the error", encoding != nil, err, expected)
			}
			if owner.seal != nil || owner.heap != (docRangeHeaderHeapEstimate{}) || !reflect.DeepEqual(owner.event, CIREvent{}) {
				test.Fatal("postclaim constructor failure retained consumed payload or accounting")
			}
			if event.Output.(map[string]any)["payload"] != "old" || len(fixture.buffer.reads) != reads {
				test.Fatal("constructor rollback mutated transferred backing or reread source")
			}
			encoding, err = newLegacyCanonicalEncodingNative(context.Background(), owner)
			if encoding != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("postclaim failure resurrected its consumed owner", encoding != nil, err)
			}
		})
	}
}

func TestLegacyCanonicalNativeOwnerReviewOldNewCacheIsolation(test *testing.T) {
	for _, termination := range []string{"complete", "canceled"} {
		test.Run(termination, func(test *testing.T) {
			oldRaw := `{"kind":"tool_result","seq":7,"output":{"tag":"old","items":[1,{"payload":"old"}]},"input":{"omitted":"old"},"blocks":[{"type":"text","text":"old-visible"},{"type":"text","text":"old-hidden"}],"blocks":[{"text":"old-updated"}]}`
			fixture := legacyNativeCanonicalOwnerStart(test, oldRaw)
			oldEvent := fixture.drain(test)
			oldWant := legacyCanonicalNativeOwnerReviewOracle(test, oldEvent)
			oldOwner, err := fixture.native.takeCanonicalOwner(context.Background())
			if err != nil {
				test.Fatal("old cache transfer failed", err)
			}
			defer oldOwner.release()
			oldEncoding, err := newLegacyCanonicalEncodingNative(context.Background(), oldOwner)
			if err != nil || oldEncoding == nil {
				test.Fatal("old cache encoder construction failed", err)
			}
			defer oldEncoding.release()
			chunk, complete, err := oldEncoding.Next(context.Background())
			if err != nil || complete || len(chunk) != 0 {
				test.Fatal("old cache encoder did not yield preparation without output", len(chunk), complete, err)
			}
			newRaw := `{"kind":"tool_result","seq":91,"output":{"tag":"new","items":[2,{"payload":"new"}]},"input":{"omitted":"new"},"blocks":[{"type":"text","text":"new-visible"},{"type":"text","text":"new-hidden"}],"blocks":[{"text":"new-updated"}]}`
			fixture.buffer = &legacySourceProofBuffer{body: []byte(newRaw)}
			fixture.source = fixture.buffer.input()
			fixture.frame = legacyJSONValueScanner{}
			fixture.native.reset(0, len(newRaw))
			fixture.native.object.prefix = true
			newEvent := fixture.drain(test)
			newWant := legacyCanonicalNativeOwnerReviewOracle(test, newEvent)
			newOwner, err := fixture.native.takeCanonicalOwner(context.Background())
			if err != nil {
				test.Fatal("new cache transfer failed", err)
			}
			defer newOwner.release()
			newEncoding, err := newLegacyCanonicalEncodingNative(context.Background(), newOwner)
			if err != nil || newEncoding == nil || newOwner == oldOwner {
				test.Fatal("cache reuse did not create independent encoder ownership", err)
			}
			defer newEncoding.release()
			fixture.native.reset(0, 0)
			fixture.native.releaseValues()
			if oldOwner.event.Output.(map[string]any)["tag"] != "old" || oldOwner.event.Input["omitted"] != "old" ||
				newOwner.event.Output.(map[string]any)["tag"] != "new" || newOwner.event.Input["omitted"] != "new" ||
				cap(oldEvent.Blocks) < 2 || cap(newEvent.Blocks) < 2 || &oldEvent.Blocks[0] == &newEvent.Blocks[0] ||
				oldEvent.Blocks[:cap(oldEvent.Blocks)][1].Text != "old-hidden" || newEvent.Blocks[:cap(newEvent.Blocks)][1].Text != "new-hidden" {
				test.Fatal("cache reuse aliased old/new payload or lost omitted capacity-tail values")
			}
			retained, err := newEncoding.RetainedBytes()
			if err != nil {
				test.Fatal("new cache encoder lost its live retained estimate", err)
			}
			if termination == "complete" {
				actual := legacyCanonicalNativeOwnerReviewDrain(test, oldEncoding)
				if !bytes.Equal(actual, oldWant) || HashContent(actual) != HashContent(oldWant) {
					test.Fatal("new cache decoding changed the old canonical stream")
				}
			} else {
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				chunk, complete, err = oldEncoding.Next(canceled)
				if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) {
					test.Fatal("old encoder cancellation exposed partial output", len(chunk), complete, err)
				}
			}
			oldEncoding.release()
			oldOwner.release()
			duplicate, err := newLegacyCanonicalEncodingNative(context.Background(), oldOwner)
			if duplicate != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("stale old owner was admitted after cache reuse", duplicate != nil, err)
			}
			after, err := newEncoding.RetainedBytes()
			if err != nil || after != retained || newEncoding.nativeOwner != newOwner || newOwner.event.Seq != 91 ||
				newOwner.event.Input["omitted"] != "new" || newEvent.Blocks[:cap(newEvent.Blocks)][1].Text != "new-hidden" {
				test.Fatal("old encoder termination released or changed new cache ownership", retained, after, err)
			}
			actual := legacyCanonicalNativeOwnerReviewDrain(test, newEncoding)
			if !bytes.Equal(actual, newWant) || HashContent(actual) != HashContent(newWant) {
				test.Fatal("old encoder termination changed the new canonical stream")
			}
		})
	}
}
