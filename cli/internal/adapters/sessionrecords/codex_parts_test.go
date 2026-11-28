package sessionrecords

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// A Codex thread's conversation is read in parts too. Going on from what was
// remembered after a sealed part gives the parts that reading everything
// gives.
func TestCodexPartsReadAfterASealedPartAreThePartsOfAWholeRead(t *testing.T) {
	long := strings.Repeat("parser ", 40)
	home := codexContentHome(t,
		codexContentInsert("lead", "t1", "u", 1, map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "Add the parser, it's needed. " + long}}})+
			codexContentInsert("lead", "t1", "c", 2, map[string]any{"type": "commandExecution", "command": "git commit -m parser", "cwd": "/work/app", "aggregatedOutput": "[main abc1234] parser", "exitCode": 0, "status": "completed"})+
			codexContentInsert("lead", "t2", "a", 3, map[string]any{"type": "agentMessage", "text": "Committed the parser. " + long})+
			codexContentInsert("lead", "t2", "n", 4, map[string]any{"type": "holoDeck", "scene": "bridge"})+
			codexContentInsert("lead", "t3", "u", 5, map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "Now test it."}}})+
			codexContentInsert("other", "t1", "u", 1, map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "Another thread."}}}))
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".cxtsecrets"), []byte("abc1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, ctx := NewCodexContent(home), context.Background()
	if reader.partBytes != domain.SessionPartBytes {
		t.Fatalf("a part is sealed at %d bytes", reader.partBytes)
	}
	reader.partBytes = 300
	records := domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}
	ids := []string{"t1/u", "t1/c", "t2/a", "t2/n", "t3/u"}
	whole, err := reader.ReadSessionParts(ctx, repo, records, ids, domain.SessionPartResume{})
	if err != nil || len(whole.Missing) != 0 || len(whole.Parts) < 3 {
		t.Fatalf("whole read: %d parts, missing %v, %v", len(whole.Parts), whole.Missing, err)
	}
	held, events := 0, 0
	for index, part := range whole.Parts {
		if part.Records() < 1 || part.Sealed == (index == len(whole.Parts)-1) || part.Token != "" || part.Envelope.SessionOriginID != "lead" || part.Envelope.SourceProvider != domain.ProviderCodex {
			t.Fatalf("part %d: %+v", index, part)
		}
		for _, event := range part.Events {
			if event.Seq != events {
				t.Fatalf("event %d has sequence %d", events, event.Seq)
			}
			events++
		}
		held += part.Records()
	}
	// The command gives a call and its result; the item of an unknown type is kept as one event.
	if held != len(ids) || events != 6 {
		t.Fatalf("records=%d events=%d", held, events)
	}
	raw, _ := json.Marshal(whole.Parts)
	if strings.Contains(string(raw), "abc1234") || strings.Contains(string(raw), "Another thread") {
		t.Fatalf("a listed value or another thread's item is in the parts: %s", raw)
	}
	for kept := 1; kept < len(whole.Parts); kept++ {
		from, records := claudePartsAfter(whole, kept)
		rest, err := reader.ReadSessionParts(ctx, repo, domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}, ids[records:], from)
		if err != nil || len(rest.Missing) != 0 || !reflect.DeepEqual(rest.Parts, whole.Parts[kept:]) {
			t.Fatalf("after %d parts: %d parts, missing %v, %v", kept, len(rest.Parts), rest.Missing, err)
		}
	}
	// An item that is no longer there is named, and nothing is returned.
	if read, err := reader.ReadSessionParts(ctx, repo, records, []string{"t1/u", "t9/gone"}, domain.SessionPartResume{}); err != nil || !reflect.DeepEqual(read.Missing, []string{"t9/gone"}) || len(read.Parts) != 0 {
		t.Fatalf("a missing item: %+v %v", read, err)
	}
	for name, other := range map[string]domain.SessionRecordRanges{
		"a Claude Code session":           {Provider: domain.ProviderClaude, ID: "lead"},
		"an identifier that is no thread": {Provider: domain.ProviderCodex, ID: "lead'; DROP"},
	} {
		if _, err := reader.ReadSessionParts(ctx, repo, other, ids, domain.SessionPartResume{}); err == nil {
			t.Errorf("%s was read", name)
		}
	}
	if _, err := NewCodexContent(t.TempDir()).ReadSessionParts(ctx, repo, records, ids, domain.SessionPartResume{}); err == nil || !strings.Contains(err.Error(), "no Codex thread history database") {
		t.Fatalf("a home without a thread history: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if read, err := reader.ReadSessionParts(cancelled, repo, records, ids, domain.SessionPartResume{}); err == nil || len(read.Parts) != 0 {
		t.Fatalf("a cancelled read: %d parts %v", len(read.Parts), err)
	}
	// Cancelled while it converts, the read stops between the parts.
	partsReadStopsWhenCancelled(t, func(ctx context.Context, ids []string) (domain.SessionPartsRead, error) {
		return reader.ReadSessionParts(ctx, repo, records, ids, domain.SessionPartResume{})
	}, ids)
}
