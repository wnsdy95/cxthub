package codec

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// claudeCountedLines is a short synthetic session: a prompt, an answer of
// three blocks, a line that is no message, a compaction boundary, a tool
// result, and an answer that names a model and its tokens.
var claudeCountedLines = []string{
	`{"type":"user","uuid":"m1","sessionId":"s","timestamp":"2031-01-02T03:04:05.000Z","cwd":"/work/app","gitBranch":"main","message":{"role":"user","content":"commit it"}}`,
	`{"type":"assistant","uuid":"m2","sessionId":"s","timestamp":"2031-01-02T03:04:06.000Z","message":{"role":"assistant","model":"model-a","usage":{"input_tokens":10,"output_tokens":7},"content":[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"I will."},{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"git commit"}}]}}`,
	`{"type":"queue-operation","uuid":"q1","sessionId":"s","timestamp":"2031-01-02T03:04:07.000Z"}`,
	`{"type":"system","subtype":"compact_boundary","uuid":"c1","sessionId":"s","timestamp":"2031-01-02T03:04:08.000Z"}`,
	`{"type":"user","uuid":"m3","sessionId":"s","timestamp":"2031-01-02T03:04:09.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"[main abc1234] done"}]}}`,
	`{"type":"assistant","uuid":"m4","sessionId":"s","timestamp":"2031-01-02T03:04:10.000Z","message":{"role":"assistant","model":"model-b","usage":{"input_tokens":30,"cache_read_input_tokens":5,"output_tokens":11},"content":[{"type":"text","text":"committed"}]}}`,
}

// Every record is converted on its own: the counts say how many events each
// gave, and converting the records in two runs, the second going on from
// where the first ended, gives what converting them in one run gives. That is
// what lets a session's conversation be built in parts.
func TestClaudeRecordsAreCountedAndConvertInRunsAsInOne(t *testing.T) {
	codec, ctx := NewClaudeCodec(), context.Background()
	join := func(lines []string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
	whole, counts, err := codec.DecodeAppendCounted(ctx, join(claudeCountedLines), domain.Envelope{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts, []int{1, 3, 0, 1, 1, 1}) || len(whole.Events) != 7 {
		t.Fatalf("counts=%v events=%d", counts, len(whole.Events))
	}
	if whole.Envelope.OutputTokens != 18 || whole.Envelope.ContextTokens != 35 || whole.Envelope.CompactionCount != 1 || whole.Envelope.Cwd != "/work/app" {
		t.Fatalf("envelope: %+v", whole.Envelope)
	}
	// An empty line is no record and has no count.
	spaced := strings.Join(claudeCountedLines[:2], "\n\n  \n") + "\n\n"
	if doc, counts, err := codec.DecodeAppendCounted(ctx, []byte(spaced), domain.Envelope{}, 0); err != nil || !reflect.DeepEqual(counts, []int{1, 3}) || len(doc.Events) != 4 {
		t.Fatalf("with empty lines: counts=%v events=%d %v", counts, len(doc.Events), err)
	}
	if doc, counts, err := codec.DecodeAppendCounted(ctx, nil, domain.Envelope{}, 0); err != nil || len(counts) != 0 || len(doc.Events) != 0 {
		t.Fatalf("no records: counts=%v events=%d %v", counts, len(doc.Events), err)
	}
	for cut := 0; cut <= len(claudeCountedLines); cut++ {
		first, firstCounts, err := codec.DecodeAppendCounted(ctx, join(claudeCountedLines[:cut]), domain.Envelope{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cut == 0 {
			first, firstCounts = domain.CIRDocument{}, nil
		}
		second, secondCounts, err := codec.DecodeAppendCounted(ctx, join(claudeCountedLines[cut:]), first.Envelope, len(first.Events))
		if err != nil {
			t.Fatal(err)
		}
		if cut == len(claudeCountedLines) {
			second.Envelope, secondCounts = first.Envelope, nil
		}
		events := append(append([]domain.Event(nil), first.Events...), second.Events...)
		if !reflect.DeepEqual(events, whole.Events) || !reflect.DeepEqual(second.Envelope, whole.Envelope) || !reflect.DeepEqual(append(append([]int{}, firstCounts...), secondCounts...), counts) {
			t.Fatalf("cut after %d records: %d events, envelope %+v", cut, len(events), second.Envelope)
		}
	}
	// A record that cannot be read fails the whole run.
	if _, counts, err := codec.DecodeAppendCounted(ctx, []byte(claudeCountedLines[0]+"\n{not json}\n"), domain.Envelope{}, 0); err == nil || counts != nil {
		t.Fatalf("a broken record: counts=%v %v", counts, err)
	}
}
