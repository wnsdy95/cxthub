package domain

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Fixed synthetic fixtures exercise the complete stateless root verifier,
// including chunk reads, hashes, framing and typed canonical event checks.
func BenchmarkConversationCanonicalVerifier(b *testing.B) {
	for _, shape := range []string{"message", "tool_io", "replacement"} {
		b.Run(shape, func(b *testing.B) {
			doc := CIRDocument{Envelope: Envelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, SessionOriginID: "synthetic-canonical-479", Fidelity: FidelityFull}}
			text := strings.Repeat("synthetic plain text ", 410)
			for i := 0; i < 128; i++ {
				e := Event{Kind: EventMessage, Seq: i, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: fmt.Sprintf("%03d %s", i, text)}}}
				switch shape {
				case "tool_io":
					if i%2 == 0 {
						e = Event{Kind: EventToolCall, Seq: i, CallID: fmt.Sprint(i), ToolName: "synthetic", Input: map[string]any{"text": text, "nested": []any{nil, true, map[string]any{"z": float64(i), "a": "<&>"}}}}
					} else {
						e = Event{Kind: EventToolResult, Seq: i, CallID: fmt.Sprint(i - 1), Output: map[string]any{"text": text, "nested": []any{false, float64(i), nil}}}
					}
				case "replacement":
					e.Seq = 1
					e = Event{Kind: EventCompaction, Seq: i, ReplacementComplete: true, Replacement: []Event{e, {Kind: EventTurn, Seq: 0, Role: "user"}}}
				}
				doc.Events = append(doc.Events, e)
			}
			manifest, bodies, err := ConversationManifestForCIR(doc)
			if err != nil {
				b.Fatal(err)
			}
			root, err := ConversationManifestHash(manifest)
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("fixture=%s root=%s bytes=%d events=%d chunks=%d", shape, root, manifest.StreamBytes, manifest.EventCount, len(manifest.Chunks))
			ctx := context.Background()
			load := func(_ context.Context, h ContentHash) ([]byte, error) { return bodies[h], nil }
			b.SetBytes(manifest.StreamBytes)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := VerifyConversationManifest(ctx, root, manifest, load); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
