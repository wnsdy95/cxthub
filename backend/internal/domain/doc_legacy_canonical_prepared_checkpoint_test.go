package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func legacyCanonicalPreparedMachineFixture(test *testing.T, numbers bool) (*legacyNormalizationNativeEvent, *LegacyCanonicalEncoding, []byte) {
	test.Helper()
	output := make(map[string]any)
	for index := 0; index < 32; index++ {
		output[strings.Repeat("shared-order-prefix", 700)+fmt.Sprintf("-%04d", index)] = index
	}
	output["a-array"] = []any{strings.Repeat("array <&>한🙂", 42000), map[string]any{"next": []any{true, nil, "tail"}}, "last"}
	output["z-body"] = strings.Repeat("body <&>한🙂\"\n", 42000)
	event := CIREvent{Kind: EventToolResult, Seq: 9, CallID: "prepared-machine", Output: output}
	raw, err := json.Marshal(event)
	if err != nil {
		test.Fatal(err)
	}
	if numbers {
		raw = []byte(`{"kind":"reasoning","seq":9,"status":"done","provider_metadata":{"turn_id":"number-cursor","create_time":` + strings.Repeat("9", 90000) + `}}`)
	}
	native, typed, _ := legacyCanonicalNativePreparationDecode(test, string(raw))
	encoding := legacyCanonicalNativePreparationNew(test, native)
	want := legacyCanonicalNativeOwnerReviewOracle(test, typed)
	return native, encoding, want
}

func TestLegacyCanonicalPreparedMachineContinuesActualOutput(test *testing.T) {
	for _, cut := range []string{"array-child", "body-string", "key-order", "number-proof"} {
		test.Run(cut, func(test *testing.T) {
			native, encoding, want := legacyCanonicalPreparedMachineFixture(test, cut == "number-proof")
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
				ready, err = writer.step(ctx)
				if err != nil || encoding.inspectWork != before || encoding.keyWork != keyWork {
					test.Fatal("checkpoint replayed or advanced the actual output machine", err)
				}
				if ready {
					break
				}
			}
			if !ready {
				test.Fatal("prepared output checkpoint did not finish within its finite allowance")
			}
			ref, err := writer.take(ctx)
			grant.release()
			if err != nil {
				test.Fatal(err)
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
