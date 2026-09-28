package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// This is the previous whole-document wire algorithm, kept as a compatibility
// oracle. Content-addressed identities must never change for existing records.
func legacyCanonicalForTest(doc CIRDocument) ([]byte, error) {
	if err := ValidateCIRVersion(doc); err != nil {
		return nil, err
	}
	doc.Events = canonicalEvents(doc.Events)
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}
func FuzzCanonicalStreamParity(f *testing.F) {
	for _, s := range []string{
		`{"envelope":{"cir_version":"1"},"events":[]}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"tool_call","seq":2,"call_id":"n","tool_name":"test","input":{"z":9007199254740993,"a":[false,null,"<\ud55c\uae00>\\\""]}},{"kind":"message","seq":0,"role":"user","blocks":[{"type":"text","text":"hello"}]}]}`,
		`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","seq":2,"replacement":[{"kind":"message","seq":2,"role":"user","blocks":[]},{"kind":"message","seq":1,"role":"assistant","blocks":[]}]}]}`,
	} {
		var seed CIRDocument
		if err := json.Unmarshal([]byte(s), &seed); err != nil {
			f.Fatal(err)
		}
		if _, err := legacyCanonicalForTest(seed); err != nil {
			f.Fatal(err)
		}
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var doc CIRDocument
		if json.Unmarshal([]byte(s), &doc) != nil {
			return
		}
		want, e1 := legacyCanonicalForTest(doc)
		got, e2 := CanonicalBytes(doc)
		if (e1 == nil) != (e2 == nil) || (e1 == nil && !bytes.Equal(got, want)) {
			t.Fatalf("canonical identity changed: old=%q new=%q errors %v %v", want, got, e1, e2)
		}
	})
}
func BenchmarkLargeCanonicalStream(b *testing.B) {
	var raw strings.Builder
	raw.WriteString(`{"envelope":{"cir_version":"1"},"events":[`)
	for i := 0; i < 256; i++ {
		if i > 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, `{"kind":"message","seq":%d,"role":"user","blocks":[{"type":"text","text":%q}]}`, 255-i, strings.Repeat("context ", 8192))
	}
	raw.WriteString(`]}`)
	var doc CIRDocument
	if err := json.Unmarshal([]byte(raw.String()), &doc); err != nil {
		b.Fatal(err)
	}
	for _, variant := range []struct {
		name string
		fn   func(CIRDocument) ([]byte, error)
	}{{"legacy", legacyCanonicalForTest}, {"current", CanonicalBytes}} {
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(raw.Len()))
			for i := 0; i < b.N; i++ {
				if _, err := variant.fn(doc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
