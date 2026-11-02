package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeEventsLargeReplacementSourceQuantum(test *testing.T) {
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}`
	raw := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", 8191) + child + `],"replacement_complete":true}`
	if len(raw) <= 2*ConversationManifestChunkBytes {
		test.Fatal("replacement fixture does not span more than two authenticated source pages")
	}
	var expected CIREvent
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal(err)
	}
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	for quantum := 0; quantum < 1024; quantum++ {
		event, complete, err := fixture.step(test, context.Background(), 0)
		if err != nil {
			test.Fatal("native replacement source decode failed", quantum, err)
		}
		if !complete {
			continue
		}
		if quantum == 0 || len(event.Replacement) != 8192 || !reflect.DeepEqual(event, expected) || !bytes.Equal(fixture.buffer.body, []byte(raw)) {
			test.Fatal("replacement source quantum changed typed values, presence, or the original bytes", quantum)
		}
		test.Logf("source bytes=%d decode quanta=%d authenticated reads=%d; per-quantum bounds are checked by the unchanged native fixture", len(raw), quantum+1, len(fixture.buffer.reads))
		return
	}
	test.Fatal("replacement source decoder exhausted its finite allowance")
}
