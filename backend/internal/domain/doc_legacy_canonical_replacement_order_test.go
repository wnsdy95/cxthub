package domain

import (
	"fmt"
	"testing"
)

func LegacyReplacementOrderEvents(count int, prefix string) []CIREvent {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	sequences := [...]int{maximum, 0, minimum, -1, maximum - 1, minimum + 1, 1, -2}
	events := make([]CIREvent, count)
	for ordinal := range events {
		marker := fmt.Sprintf("%s-%03d", prefix, count-ordinal)
		events[ordinal] = CIREvent{
			Kind: EventMessage, ID: marker, Seq: sequences[ordinal%len(sequences)], Role: RoleAssistant,
			Blocks: []ContentBlock{{Type: "text", Text: marker + " <&> \ud55c\uad6d🙂"}},
		}
	}
	return events
}

func TestLegacyReplacementOrderStableOriginalOrdinal(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, sequence := range []int{-maximum - 1, -1, 0, maximum} {
		for _, count := range []int{8, 64, 65, 129} {
			test.Run(fmt.Sprintf("seq-%d/children-%d", sequence, count), func(test *testing.T) {
				events := LegacyReplacementOrderEvents(count, "reverse-id")
				for ordinal := range events {
					events[ordinal].Seq = sequence
				}
			})
		}
	}
}
