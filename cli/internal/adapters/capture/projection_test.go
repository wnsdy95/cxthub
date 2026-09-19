package capture_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type countingAppendCodec struct {
	outbound.ProviderCodec
	bytes int
}

func (c *countingAppendCodec) DecodeAppend(ctx context.Context, b []byte, e domain.Envelope, n int) (domain.CIRDocument, error) {
	c.bytes += len(b)
	return c.ProviderCodec.(interface {
		DecodeAppend(context.Context, []byte, domain.Envelope, int) (domain.CIRDocument, error)
	}).DecodeAppend(ctx, b, e, n)
}
func TestCaptureProjectionMatchesFullDecode(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "native.jsonl")
			store := storage.NewFileStore(root)
			svc := capture.NewSessionCapture(store)
			var source outbound.CaptureSource
			var cdc outbound.ProviderCodec
			var lines []string
			if provider == domain.ProviderClaude {
				source = capture.NewClaudeCapture()
				cdc = codec.NewClaudeCodec()
				lines = []string{`{"type":"user","sessionId":"synthetic","message":{"role":"user","content":"first"}}`, `{"type":"assistant","message":{"model":"m1","content":"secret-one","usage":{"input_tokens":12,"output_tokens":7}}}`, `{"type":"system","subtype":"compact_boundary"}`, `{"type":"assistant","message":{"model":"m2","content":"last","usage":{"input_tokens":4,"output_tokens":3}}}`}
			} else {
				source = capture.NewCodexCapture()
				cdc = codec.NewCodexCodec()
				lines = []string{`{"type":"session_meta","payload":{"id":"synthetic","model":"m1"}}`, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"secret-one"}]}}`, `{"type":"compacted","payload":{"message":"compact","replacement_history":[]}}`, `{"type":"turn_context","payload":{"model":"m2"}}`, `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":7},"last_token_usage":{"input_tokens":4}}}}`}
			}
			counting := &countingAppendCodec{ProviderCodec: cdc}
			verify := func(raw string, decodeLimit int) {
				t.Helper()
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				counting.bytes = 0
				_, got, offset, _, err := svc.Project(context.Background(), root, path, source, counting, true)
				if err != nil {
					t.Fatal(err)
				}
				clean, _ := capture.ScrubSecrets([]byte(raw), root)
				if decodeLimit == len(raw) {
					decodeLimit = len(clean)
				}
				full, err := cdc.Decode(context.Background(), clean)
				if err != nil {
					t.Fatal(err)
				}
				full = capture.ScrubDoc(full, root)
				b, _ := domain.CanonicalBytes(full)
				if got != domain.HashContent(b) {
					t.Fatalf("incremental hash %s != full %s", got, domain.HashContent(b))
				}
				if offset != int64(len(raw)) {
					t.Fatal("wrong checkpoint offset")
				}
				if counting.bytes > decodeLimit {
					t.Fatalf("decoded %d bytes; budget %d", counting.bytes, decodeLimit)
				}
				if _, err := store.GetDoc(context.Background(), got); err != nil {
					t.Fatal(err)
				}
			}
			raw := ""
			for _, line := range lines {
				delta := line + "\n"
				raw += delta
				verify(raw, len(delta))
			}
			verify(raw, 0)
			// A same-size rewrite in the already processed prefix must rebuild.
			changed := strings.ReplaceAll(raw, "secret-one", "secret-two")
			verify(changed, len(changed))
			raw = changed
			// Policy changes invalidate old masked projections.
			if err := os.WriteFile(filepath.Join(root, ".cxtsecrets"), []byte("secret-two\n"), 0600); err != nil {
				t.Fatal(err)
			}
			verify(raw, len(raw))
			// An incomplete tail does not advance the durable cursor.
			broken := raw + `{"type":`
			if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := svc.Project(context.Background(), root, path, source, counting, false); err == nil {
				t.Fatal("explicit save accepted an incomplete record")
			}
			_, _, offset, _, err := svc.Project(context.Background(), root, path, source, counting, true)
			if err != nil {
				t.Fatal(err)
			}
			if offset != int64(len(raw)) {
				t.Fatal("partial record consumed")
			}
			delta := lines[len(lines)-1] + "\n"
			verify(raw+delta, len(delta))
			// Rotation/truncation is not interpreted as a negative or missing delta.
			verify(lines[0]+"\n", len(lines[0])+1)
		})
	}
}
