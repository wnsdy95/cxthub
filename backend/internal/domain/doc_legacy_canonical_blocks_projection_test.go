package domain

import (
	"fmt"
	"testing"
)

func TestLegacyCanonicalBlocksProjectionMessageAndTypedOutput(test *testing.T) {
	backing := []ContentBlock{{Type: "text", Text: "visible"}, {Type: "hidden", Text: "unused capacity"}}
	for _, candidate := range []struct {
		name   string
		blocks []ContentBlock
	}{
		{name: "nil"},
		{name: "empty", blocks: []ContentBlock{}},
		{name: "zero-fields", blocks: []ContentBlock{{}}},
		{name: "sparse", blocks: []ContentBlock{{Type: "text"}, {Text: "text without type"}}},
		{name: "escaping", blocks: []ContentBlock{{Type: "text<&>\u2028", Text: "\"\\\n\x00<&>\u2028\u2029\ud55c\uad6d🙂\xff"}}},
		{name: "spare-capacity", blocks: backing[:1]},
	} {
		for _, kind := range []EventKind{EventMessage, EventToolResult} {
			for _, nested := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%s/nested-%t", candidate.name, kind, nested), func(test *testing.T) {
					event := CIREvent{Kind: kind, Seq: 9007199254740993}
					if kind == EventMessage {
						event.Role, event.Blocks = RoleAssistant, candidate.blocks
					} else {
						event.CallID, event.Output = "call", candidate.blocks
					}
					if nested {
						event = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{
							Kind: EventCompaction, Replacement: []CIREvent{event},
						}}}
					}
				})
			}
		}
	}
}
