package chunkcas

import (
	"bytes"
	"encoding/json"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"testing"
)

func TestChunkStreamFramingAndOwnership(t *testing.T) {
	for _, bad := range []string{
		`{"envelope":{},"events":[],"extra":[]}`,
		`{"envelope":{},"events":[0],"events":[1]}`,
		`{"envelope":{},"events":[0],"extra":[]}`,
		`{"envelope":{},"events":[`,
		`{"envelope":{},"events":[]}`,
	} {
		if _, ok := PlanDoc([]byte(bad)); ok {
			t.Fatalf("accepted framing %s", bad)
		}
	}
	original := []byte(`{"envelope":{"path":"quoted \" braces }[]"},"events":[{"text":"a \" b \\ c"}]}`)
	source := append([]byte(nil), original...)
	p, ok := PlanDoc(source)
	if !ok {
		t.Fatal("valid framing refused")
	}
	for i := range source {
		source[i] = 'x'
	}
	chunks := make([][]byte, 0, len(p.Order))
	for _, h := range p.Order {
		chunks = append(chunks, p.Bodies[h])
	}
	got, err := AssembleChunks(p.Manifest, chunks, domain.HashContent(original))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("plan aliases source: %v", err)
	}
}
func FuzzChunkStreamRoundtrip(f *testing.F) {
	f.Add(`{"text":"hello"}`)
	f.Add(`{"a":{"nested":[]},"b":[9007199254740993,"\ud55c\uae00"]}`)
	f.Fuzz(func(t *testing.T, event string) {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(event)) != nil {
			return
		}
		cb := append([]byte(`{"envelope":{},"events":[`), compact.Bytes()...)
		cb = append(cb, ']', '}')
		p, ok := PlanDoc(cb)
		if !ok {
			t.Fatal("canonical plan unavailable")
		}
		chunks := make([][]byte, 0, len(p.Order))
		for _, h := range p.Order {
			chunks = append(chunks, p.Bodies[h])
		}
		got, err := AssembleChunks(p.Manifest, chunks, domain.HashContent(cb))
		if err != nil || !bytes.Equal(got, cb) {
			t.Fatalf("roundtrip: %v", err)
		}
	})
}
