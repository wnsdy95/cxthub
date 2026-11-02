package domain

import "testing"

func TestLegacyNativeEventListReleasesCompletedCursor(test *testing.T) {
	fixture := legacyEventValuesCheckpointFixtures()[1]
	cut := fixture.cut
	suspendedNested := false
	fixture.cut = func(decoder *LegacyNormalizationDecoder) bool {
		if decoder.native != nil {
			for _, list := range decoder.native.object.events {
				if list != nil && list.cursor != nil && list.cursor != list {
					suspendedNested = true
				}
			}
		}
		return cut(decoder)
	}
	decoder, _ := legacyDecoderCheckpointCut(test, fixture)
	list, _ := legacyEventValuesCheckpointList(test, decoder)
	if !suspendedNested || list.phase != legacyNativeEventsDone {
		test.Fatal("actual parser did not suspend and finish its nested replacement")
	}
	if list.cursor != nil || list.parent != nil {
		test.Fatal("completed replacement retained its old execution path")
	}
	if list.object == nil || list.object.events != nil {
		test.Fatal("fixture did not release the completed child subtree")
	}
	if got := legacyDecoderCheckpointContinue(test, decoder); len(got) == 0 || got[len(got)-1] == "integrity" {
		test.Fatal("cursor release changed valid continued parsing", got)
	}
}
