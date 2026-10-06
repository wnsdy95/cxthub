package chunkcas

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPortableManifestOrderedEntryBounds(t *testing.T) {
	hash := domain.HashContent([]byte("repeated body"))
	for _, count := range []int{0, 1, MaxPortableManifestChunks, MaxPortableManifestChunks + 1} {
		manifest := Manifest{Format: FormatV2, Envelope: json.RawMessage(`{}`)}
		for range count {
			manifest.Chunks = append(manifest.Chunks, hash)
		}
		if got, want := PortableManifest(manifest), count > 0 && count <= MaxPortableManifestChunks; got != want {
			t.Fatalf("%d repeated ordered entries: got %v want %v", count, got, want)
		}
	}
}

func TestPortableManifestEncodedByteBounds(t *testing.T) {
	for _, format := range []string{FormatV2, FormatV1, ""} {
		for _, extra := range []int{0, 1} {
			manifest := Manifest{Format: format, Envelope: json.RawMessage(`{"cwd":""}`), Chunks: []domain.ContentHash{domain.HashContent([]byte("body"))}}
			normalized := manifest
			normalized.Format = normalizeFormat(format)
			base, err := json.Marshal(normalized)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Envelope = json.RawMessage(`{"cwd":"` + strings.Repeat("x", MaxPortableManifestBytes-len(base)+extra) + `"}`)
			if got := PortableManifest(manifest); got != (extra == 0) {
				t.Fatalf("format=%q encoded limit+%d: %v", format, extra, got)
			}
		}
	}
	// encoding/json escapes HTML-sensitive characters. Raw envelope length
	// alone cannot establish the encoded manifest size admitted by the server.
	manifest := Manifest{Format: FormatV2, Envelope: json.RawMessage(`{"cwd":"` + strings.Repeat("<", MaxPortableManifestBytes/5) + `"}`), Chunks: []domain.ContentHash{domain.HashContent([]byte("body"))}}
	if len(manifest.Envelope) >= MaxPortableManifestBytes || PortableManifest(manifest) {
		t.Fatal("encoded expansion escaped the byte bound")
	}
	manifest.Envelope = json.RawMessage(`{"broken"`)
	if PortableManifest(manifest) {
		t.Fatal("invalid envelope could not be encoded but was considered portable")
	}
}
