package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestLegacyNormalizationStageRejectsSameKeyStoredCorruption(test *testing.T) {
	for _, damage := range []string{"unchanged", "key-text", "value-record"} {
		test.Run(damage, func(test *testing.T) {
			items := make([]any, 64)
			for position := range items {
				items[position] = map[string]any{"shared-key": strings.Repeat("x", 2048), "tail-key": true}
			}
			event := CIREvent{Kind: EventToolResult, Seq: 7, CallID: "fresh-key-proof", Output: map[string]any{"items": items}}
			raw, err := json.Marshal(event)
			if err != nil {
				test.Fatal(err)
			}
			work, buffer := legacyCanonicalInitialCheckpointStage(test, string(raw))
			legacyNormalizationPreparationAdvance(test, work, func() bool {
				return work.phase == legacyNormalizationStage && work.next == 0 && work.pending[0].Encoding.preparation == nil
			})
			capture, err := work.BeginCheckpoint(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			defer capture.Close()
			control, err := capture.Control(context.Background())
			if err != nil || !control.Staging() || control.Written == 0 {
				test.Fatal("fixture did not retain actual partial-output preparation", control, err)
			}
			store, objects := legacyNativeCheckpointStoreFixture(test)
			keyHash := HashContent([]byte("shared-key"))
			var recordHash, damagedHash ContentHash
			var corrupted []byte
			freshChecks := 0
			put := store.Put
			store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				if damagedHash != "" && hash == damagedHash {
					freshChecks++
				}
				if err := put(ctx, hash, body); err != nil {
					return err
				}
				if recordHash == "" {
					var record legacyCanonicalPreparedValueRecord
					if json.Unmarshal(body, &record) == nil && record.Kind == legacyCanonicalPreparedValueKind &&
						record.Tag == legacyCanonicalPreparedString && record.Texts[0].Tree.Ref.Hash == keyHash {
						recordHash = hash
					}
				}
				return nil
			}
			binding := HashContent(buffer.body)
			checkpointIO := LegacyNormalizationParserCheckpointIO{Put: store.Put, Read: store.Read}
			for range legacyDecoderCheckpointSteps {
				ref, ready, err := capture.StageStep(context.Background(), binding, checkpointIO, legacyNormalizationStageFenceBudget)
				if err != nil {
					if damage == "unchanged" || !errors.Is(err, ErrIntegrity) || damagedHash == "" || freshChecks == 0 ||
						ready || ref != (LegacyNormalizationParserCheckpointRef{}) {
						test.Fatal("repeated-key corruption did not fail through current checkpoint I/O", ref, ready, freshChecks, err)
					}
					if !bytes.Equal(objects[damagedHash], corrupted) {
						test.Fatal("failed key reuse overwrote corrupt immutable bytes")
					}
					legacyNormalizationStageFenceReleased(test, work, capture)
					return
				}
				if ready {
					if damage != "unchanged" || recordHash == "" || ref == (LegacyNormalizationParserCheckpointRef{}) {
						test.Fatal("checkpoint acknowledged a repeated key without current corruption validation", damage, freshChecks)
					}
					if HashContent(objects[keyHash]) != keyHash || HashContent(objects[recordHash]) != recordHash {
						test.Fatal("unchanged repeated key lost its stored dependencies")
					}
					return
				}
				if damagedHash == "" && recordHash != "" && damage != "unchanged" {
					damagedHash = keyHash
					if damage == "value-record" {
						damagedHash = recordHash
					}
					original := objects[damagedHash]
					if len(original) == 0 || HashContent(original) != damagedHash {
						test.Fatal("first key dependency was not durably accepted by the I/O fixture")
					}
					corrupted = bytes.Clone(original)
					corrupted[0] ^= 1
					objects[damagedHash] = bytes.Clone(corrupted)
					if len(corrupted) != len(original) || HashContent(corrupted) == damagedHash {
						test.Fatal("fixture did not install same-size stored corruption")
					}
				}
			}
			test.Fatal("repeated-key checkpoint did not reach its bounded outcome", damage, freshChecks)
		})
	}
}
