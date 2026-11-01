package domain

import (
	"context"
	"errors"
	"testing"
)

func TestLegacyNormalizationPipelineReservationNearAggregateLimit(test *testing.T) {
	const total = int64(64 << 20)
	const reservation = int64(128 << 10)
	for _, reserved := range []bool{false, true} {
		name := "full-budget"
		budget := total
		if reserved {
			name = "pipeline-reserved"
			budget -= reservation
		}
		test.Run(name, func(test *testing.T) {
			work, capture, buffer, store := legacyNormalizationStageFenceStart(test, false)
			binding := HashContent(buffer.body)
			var root LegacyNormalizationParserCheckpointRef
			for range legacyDecoderCheckpointSteps {
				ref, complete, err := capture.StageStep(context.Background(), binding, store, budget)
				if err != nil {
					test.Fatal(err)
				}
				if complete {
					root = ref
					break
				}
			}
			if root == (LegacyNormalizationParserCheckpointRef{}) {
				test.Fatal("fixture did not finish its real Stage root")
			}
			before := legacyNormalizationStageFenceRetained(test, work, capture)
			previous := capture.control.HashState
			target := total - reservation/2
			capacity := int((target-before)/2) + cap(previous)
			capture.control.HashState = make([]byte, len(previous), capacity)
			copy(capture.control.HashState, previous)
			retained := legacyNormalizationStageFenceRetained(test, work, capture)
			if retained <= total-reservation || retained >= total {
				test.Fatal("fixture missed the reserved near-limit interval", retained)
			}
			store.Put = func(context.Context, ContentHash, []byte) error {
				test.Fatal("cached root attempted unexpected checkpoint writes")
				return ErrIntegrity
			}
			ref, complete, err := capture.StageStep(context.Background(), binding, store, budget)
			if reserved {
				if !errors.Is(err, ErrDocumentResourceLimit) || complete || ref != (LegacyNormalizationParserCheckpointRef{}) {
					test.Fatal("reserved pipeline heap was available to the cached Stage root", ref, complete, err)
				}
				legacyNormalizationStageFenceReleased(test, work, capture)
			} else if err != nil || !complete || ref != root {
				test.Fatal("control fixture rejected a valid full-budget cached root", ref, complete, err)
			}
			test.Logf("budget=%d retained=%d reservation=%d", budget, retained, reservation)
		})
	}
}
