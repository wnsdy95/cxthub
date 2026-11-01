package domain

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyNativeScalarBackingSurvivesCanonicalHandoff(test *testing.T) {
	raw := legacyNativeEventDocument("2", `{"kind":"turn","id":"`+strings.Repeat("x", (8<<20)+1)+`"}`)
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil || step.Complete {
			test.Fatal("scalar handoff was not observable", err)
		}
		if len(step.Events) == 0 {
			continue
		}
		if len(step.Events) != 1 || step.Events[0].Encoding == nil {
			test.Fatal("unexpected handoff batch", len(step.Events))
		}
		retained, err := step.Events[0].Encoding.RetainedBytes()
		if err != nil || retained <= 16<<20 {
			test.Fatal("canonical handoff lost the scalar's 16MiB backing allocation", retained, err)
		}
		return
	}
	test.Fatal("scalar handoff exhausted its bounded progress allowance")
}
