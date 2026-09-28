package domain

import (
	"bytes"
	"encoding/json"
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
		if _, ok := PlanDocChunks([]byte(bad)); ok {
			t.Fatalf("accepted framing %s", bad)
		}
	}
	original := []byte(`{"envelope":{"path":"quoted \" braces }[]"},"events":[{"text":"a \" b \\ c"}]}`)
	source := append([]byte(nil), original...)
	p, ok := PlanDocChunks(source)
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
	got, err := AssembleDocChunks(p.Manifest, chunks, HashContent(original))
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
		p, ok := PlanDocChunks(cb)
		if !ok {
			t.Fatal("canonical plan unavailable")
		}
		chunks := make([][]byte, 0, len(p.Order))
		for _, h := range p.Order {
			chunks = append(chunks, p.Bodies[h])
		}
		got, err := AssembleDocChunks(p.Manifest, chunks, HashContent(cb))
		if err != nil || !bytes.Equal(got, cb) {
			t.Fatalf("roundtrip: %v", err)
		}
	})
}
