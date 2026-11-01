package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func legacyCanonicalPreparedMachineFixture(test *testing.T, cut string) (*legacyNormalizationNativeEvent, *LegacyCanonicalEncoding, []byte) {
	test.Helper()
	output := make(map[string]any)
	for index := 0; index < 32; index++ {
		output[strings.Repeat("shared-order-prefix", 700)+fmt.Sprintf("-%04d", index)] = index
	}
	output["a-array"] = []any{strings.Repeat("array <&>\ud55c🙂", 42000), map[string]any{"next": []any{true, nil, "tail"}}, "last"}
	output["z-body"] = strings.Repeat("body <&>\ud55c🙂\"\n", 42000)
	event := CIREvent{Kind: EventToolResult, Seq: 9, CallID: "prepared-machine", Output: output}
	if cut == "repeated-keys" {
		items := make([]any, 64)
		for position := range items {
			items[position] = map[string]any{"shared-key": strings.Repeat("x", 2048), "tail-key": true}
		}
		event.Output = map[string]any{"items": items}
	}
	raw, err := json.Marshal(event)
	if err != nil {
		test.Fatal(err)
	}
	if cut == "number-proof" {
		raw = []byte(`{"kind":"reasoning","seq":9,"status":"done","provider_metadata":{"turn_id":"number-cursor","create_time":` + strings.Repeat("9", 90000) + `}}`)
	}
	native, typed, _ := legacyCanonicalNativePreparationDecode(test, string(raw))
	encoding := legacyCanonicalNativePreparationNew(test, native)
	want := legacyCanonicalNativeOwnerReviewOracle(test, typed)
	return native, encoding, want
}

func TestLegacyCanonicalPreparedMachineContinuesActualOutput(test *testing.T) {
	for _, cut := range []string{"array-child", "body-string", "key-order", "number-proof", "repeated-keys"} {
		test.Run(cut, func(test *testing.T) {
			native, encoding, want := legacyCanonicalPreparedMachineFixture(test, cut)
			ctx := context.Background()
			var output []byte
			reached := false
			for steps := 0; steps < 1<<17; steps++ {
				body, complete, err := encoding.Next(ctx)
				if err != nil || complete || len(body) > LegacyCanonicalEncodingBytes {
					test.Fatal("actual encoding passed its requested unfinished output cut", complete, err)
				}
				output = append(output, body...)
				if encoding.preparation != nil {
					continue
				}
				switch cut {
				case "repeated-keys":
					reached = len(output) > 0
				case "number-proof":
					reached = encoding.numbers.position > 0 && encoding.numbers.next < encoding.numbers.pending.length()
				case "key-order":
					reached = encoding.ordering && encoding.order.compareOffset > 0
				default:
					frame := encoding.frames.at(encoding.frames.length() - 1)
					if frame != nil && frame.kind == 's' && frame.position > 0 && len(frame.text) > LegacyCanonicalEncodingBytes {
						for index := 0; index < encoding.frames.length(); index++ {
							parent := encoding.frames.at(index)
							if cut == "array-child" && parent.kind == 'a' && parent.position > 0 || cut == "body-string" && strings.HasPrefix(frame.text, "body ") {
								reached = true
							}
						}
					}
				}
				if reached {
					break
				}
			}
			if !reached {
				test.Fatal("actual encoder never reached the required cut", cut)
			}
			grant, err := native.object.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			storage := legacyCanonicalPreparedValueTestStorage(test)
			keyWrites := 0
			put := storage.io.Put
			storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				if hash == HashContent([]byte("shared-key")) {
					keyWrites++
				}
				return put(ctx, hash, body)
			}
			binding := HashContent([]byte("prepared-machine-" + cut))
			retained, err := encoding.RetainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			writer, err := newLegacyCanonicalPreparedCheckpoint(ctx, encoding, binding, storage.io, (64<<20)-retained)
			if err != nil {
				test.Fatal(err)
			}
			before, keyWork := encoding.inspectWork, encoding.keyWork
			ready := false
			for steps := 0; steps < 1<<18; steps++ {
				storage.bytes = 0
				ready, err = writer.step(ctx)
				if err != nil || encoding.inspectWork != before || encoding.keyWork != keyWork {
					test.Fatal("checkpoint replayed or advanced the actual output machine", err)
				}
				if storage.bytes > LegacyCanonicalEncodingBytes {
					test.Fatal("checkpoint control batching exceeded its existing I/O quantum", steps, storage.bytes)
				}
				if ready {
					break
				}
			}
			if !ready {
				test.Fatal("prepared output checkpoint did not finish within its finite allowance")
			}
			if cut == "repeated-keys" && keyWrites != 1 {
				test.Fatal("one capture repeatedly serialized identical short field keys", keyWrites)
			}
			ref, err := writer.take(ctx)
			grant.release()
			if err != nil {
				test.Fatal(err)
			}
			if writer.keyRefs != (legacyCanonicalPreparedKeyRefs{}) {
				test.Fatal("completed checkpoint retained its key references")
			}
			if cut == "repeated-keys" {
				freshGrant, err := native.object.mutation.beginCapture()
				if err != nil {
					test.Fatal(err)
				}
				fresh, err := newLegacyCanonicalPreparedCheckpoint(ctx, encoding, binding, storage.io, (64<<20)-retained)
				if err != nil {
					test.Fatal(err)
				}
				for steps := 0; steps < 1<<18 && fresh.keyRefs.find("shared-key").Bytes == 0; steps++ {
					if _, err := fresh.step(ctx); err != nil {
						test.Fatal(err)
					}
				}
				if keyWrites != 2 || fresh.keyRefs.find("shared-key").Bytes == 0 {
					test.Fatal("new capture reused an earlier capture's key authority", keyWrites)
				}
				fresh.close()
				freshGrant.release()
			}
			encoding.release()
			restore, err := newLegacyCanonicalPreparedRestore(ctx, ref, binding, storage.io, 64<<20)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			ready = false
			for steps := 0; steps < 1<<18; steps++ {
				ready, err = restore.step(ctx)
				if err != nil {
					test.Fatal("fresh output machine restoration failed", err)
				}
				if ready {
					break
				}
			}
			if !ready {
				test.Fatal("fresh output machine did not restore within its finite allowance")
			}
			resumed, err := restore.take(ctx)
			if err != nil || resumed.inspectWork != before || resumed.keyWork != keyWork {
				test.Fatal("restoration changed the saved semantic work counters", err)
			}
			complete := false
			for steps := 0; steps < 1<<18; steps++ {
				body, done, err := resumed.Next(ctx)
				if err != nil || len(body) > LegacyCanonicalEncodingBytes || cap(body) > LegacyCanonicalEncodingBytes {
					test.Fatal("resumed output did not retain the original bounded Next contract", err)
				}
				output = append(output, body...)
				if done {
					complete = true
					break
				}
			}
			if !complete || !bytes.Equal(output, want) {
				test.Fatal("resumed output repeated or omitted the canonical prefix", complete, len(output), len(want))
			}
		})
	}
}
