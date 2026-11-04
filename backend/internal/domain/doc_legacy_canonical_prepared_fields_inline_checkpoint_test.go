package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLegacyCanonicalPreparedFieldsInlineActualCheckpoint(test *testing.T) {
	for _, cut := range []string{"head-", "tail-"} {
		test.Run(cut, func(test *testing.T) {
			fields := map[string]any{"a-head": strings.Repeat("head-", 20000), "z-tail": strings.Repeat("tail-", 20000)}
			values := []any{"short", "\ud55c\U0001f642", nil, false, true, ""}
			for position := 0; position < 64; position++ {
				fields[fmt.Sprintf("k%04d", position)] = values[position%len(values)]
			}
			event := CIREvent{Kind: EventToolResult, Seq: 9, CallID: "inline-fields", Output: fields}
			raw, err := json.Marshal(event)
			if err != nil {
				test.Fatal(err)
			}
			native, typed, _ := legacyCanonicalNativePreparationDecode(test, string(raw))
			encoding := legacyCanonicalNativePreparationNew(test, native)
			defer encoding.release()
			expected := legacyCanonicalNativeOwnerReviewOracle(test, typed)
			ctx := context.Background()
			var output []byte
			reached := false
			for steps := 0; steps < 1<<17; steps++ {
				body, complete, err := encoding.Next(ctx)
				if err != nil || complete || len(body) > LegacyCanonicalEncodingBytes {
					test.Fatal("encoder missed unfinished field prefix", complete, err)
				}
				output = append(output, body...)
				if encoding.preparation == nil {
					frame := encoding.frames.at(encoding.frames.length() - 1)
					if frame != nil && frame.kind == 's' && frame.position > 0 && strings.HasPrefix(frame.text, cut) {
						reached = true
						break
					}
				}
			}
			if !reached || len(output) == 0 {
				test.Fatal("actual prepared fields never reached prefix cut", cut)
			}
			before, keyWork := encoding.inspectWork, encoding.keyWork
			retained, err := encoding.RetainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			storage := legacyCanonicalPreparedValueTestStorage(test)
			binding := HashContent(raw)
			ref := func() legacyNativeCheckpointRef {
				grant, err := native.object.mutation.beginCapture()
				if err != nil {
					test.Fatal(err)
				}
				defer grant.release()
				put := storage.io.Put
				defer func() { storage.io.Put = put }()
				storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
					if !grant.valid(native.object.mutation) || encoding.inspectWork != before || encoding.keyWork != keyWork {
						test.Fatal("inline capture changed source grant or work counters")
					}
					if err := native.object.mutation.enter(); !errors.Is(err, ErrLegacyNormalizationBusy) {
						test.Fatal("inline page callback could mutate frozen source", err)
					}
					return put(ctx, hash, body)
				}
				writer, err := newLegacyCanonicalPreparedCheckpoint(ctx, encoding, binding, storage.io, legacyCanonicalPreparedValueTestBudget-retained)
				if err != nil {
					test.Fatal(err)
				}
				defer writer.close()
				for steps := 0; steps < 1<<18; steps++ {
					storage.bytes = 0
					ready, err := writer.step(ctx)
					if err != nil || storage.bytes > LegacyCanonicalEncodingBytes || encoding.inspectWork != before || encoding.keyWork != keyWork {
						test.Fatal("inline checkpoint changed bounded work or counters", steps, storage.bytes, err)
					}
					if ready {
						ref, err := writer.take(ctx)
						if err != nil {
							test.Fatal(err)
						}
						return ref
					}
				}
				test.Fatal("actual inline checkpoint did not finish")
				return legacyNativeCheckpointRef{}
			}()
			after, err := encoding.RetainedBytes()
			if err != nil || after != retained {
				test.Fatal("inline checkpoint changed frozen source heap", retained, after, err)
			}
			inlineRows := 0
			for _, body := range storage.objects {
				inlineRows += bytes.Count(body, []byte(`"inline":{"binding":`))
			}
			if inlineRows < 64 {
				test.Fatal("actual checkpoint did not exercise inline field records", inlineRows)
			}
			encoding.release()
			restore, err := newLegacyCanonicalPreparedRestore(ctx, ref, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			ready := false
			for steps := 0; steps < 1<<18; steps++ {
				storage.bytes = 0
				ready, err = restore.step(ctx)
				if err != nil || storage.bytes > LegacyCanonicalEncodingBytes {
					test.Fatal("actual inline restore failed", steps, storage.bytes, err)
				}
				if ready {
					break
				}
			}
			if !ready {
				test.Fatal("actual inline restore did not finish")
			}
			resumed, err := restore.take(ctx)
			if err != nil || resumed.inspectWork != before || resumed.keyWork != keyWork {
				test.Fatal("inline restore changed semantic action counters", err)
			}
			defer resumed.release()
			complete := false
			for steps := 0; steps < 1<<18; steps++ {
				body, done, err := resumed.Next(ctx)
				if err != nil || len(body) > LegacyCanonicalEncodingBytes || cap(body) > LegacyCanonicalEncodingBytes {
					test.Fatal("resumed inline fields lost output bound", err)
				}
				output = append(output, body...)
				if done {
					complete = true
					break
				}
			}
			if !complete || !bytes.Equal(output, expected) {
				test.Fatal("inline fields repeated or omitted consumed canonical prefix", complete, len(output), len(expected))
			}
		})
	}
}
