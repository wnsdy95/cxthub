package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDocRangeMetadataDecoderPreservesAnonymousTypeErrors(test *testing.T) {
	for _, raw := range []string{`{"kind":4}`, `{"tool_name":false}`, `{"compact_summary":"yes"}`} {
		var metadata struct {
			Seq             docRangeJSONView `json:"seq"`
			Kind            EventKind        `json:"kind"`
			Role            string           `json:"role"`
			ToolName        string           `json:"tool_name"`
			CompactSummary  bool             `json:"compact_summary"`
			Blocks          docRangeJSONView `json:"blocks"`
			RedactedSummary docRangeJSONView `json:"redacted_summary"`
		}
		want := json.Unmarshal([]byte(raw), &metadata)
		_, got := docRangeEntryWithSequence(0, 0, []byte(raw), 0, false)
		if want == nil || got == nil || !reflect.DeepEqual(got, want) || got.Error() != want.Error() {
			test.Fatalf("metadata error changed: got=%#v want=%#v", got, want)
		}
	}
}
