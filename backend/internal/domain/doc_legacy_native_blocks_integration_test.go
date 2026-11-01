package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyNativeBlocksIntegrationGiantTextAndDuplicateBacking(test *testing.T) {
	giant, err := json.Marshal(strings.Repeat("x", 5*ConversationManifestChunkBytes+73) + "한🙂<&>\n")
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name   string
		fields string
	}{
		{name: "selected-giant-text", fields: `"blocks":[{"type":"text","text":` + string(giant) + `}]`},
		{name: "hidden-tail-not-emitted", fields: `"blocks":[{"type":"text","text":"first"},{"type":"text","text":` + string(giant) + `}],"blocks":[{"text":"short"}]`},
		{name: "hidden-tail-reexposed", fields: `"blocks":[{"type":"text","text":"first"},{"type":"text","text":` + string(giant) + `}],"blocks":[{"text":"short"}],"BLOCKS":[null,{}]`},
		{name: "whole-null-clears-capacity", fields: `"blocks":[{"type":"text","text":` + string(giant) + `}],"blocks":null,"blocks":[{"type":"text","text":"fresh"}]`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"message","role":"user",`+fixture.fields+`}`)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, true)
			var physical int64
			for _, read := range input.reads {
				physical += int64(read[1])
			}
			test.Logf("source bytes=%d authenticated bytes read=%d ratio=%.3f", len(raw), physical, float64(physical)/float64(len(raw)))
		})
	}
}
