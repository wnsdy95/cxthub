package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func rootEventsFixture(test *testing.T) (ConversationManifest, map[ContentHash][]byte, CIRDocument) {
	test.Helper()
	stream := `{"blocks":[{"text":"first","type":"text"}],"kind":"message","role":"user","seq":0},` +
		`{"blocks":[{"text":"` + strings.Repeat("x", 3*ConversationManifestChunkBytes) + "\ud55c" + `\\\"","type":"text"}],"kind":"message","role":"assistant","seq":1},` +
		`{"blocks":[{"text":"last","type":"text"}],"kind":"message","role":"user","seq":2}`
	envelope := conversationTestEnvelope(test, false)
	manifest, bodies := conversationTestPlan(test, envelope, []byte(stream), 3)
	var document CIRDocument
	if err := json.Unmarshal(AssembleCanonicalDoc(envelope, [][]byte{[]byte(stream)}), &document); err != nil {
		test.Fatal(err)
	}
	return manifest, bodies, document
}
