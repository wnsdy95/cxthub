package sessionrecords

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// claudePartsSession writes a session file of twelve own records, m01 to
// m12, of different sizes, among lines that are not its records: a record
// another session holds (z00), a compaction boundary, a second line under an
// identifier, and a last line that is still being written.
func claudePartsSession(t *testing.T, dir string) (string, []string) {
	t.Helper()
	text := func(length int, letter string) string { return strings.Repeat(letter, length) }
	file := claudeContentLine(t, "user", "z00", "this record belongs to an earlier session") +
		claudeContentLine(t, "user", "m01", "commit it "+text(40, "a")) +
		claudeContentLine(t, "assistant", "m02", []any{map[string]any{"type": "text", "text": "running " + text(120, "b")}, map[string]any{"type": "tool_use", "id": "call-1", "name": "Bash", "input": map[string]any{"command": "git commit"}}}) +
		`{"type":"system","uuid":"s1","subtype":"compact_boundary","timestamp":"2031-01-02T03:00:00.000Z"}` + "\n" +
		claudeContentLine(t, "user", "m03", []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "[main abc1234] first"}}) +
		claudeContentLine(t, "user", "m03", "a later line under the same identifier") +
		claudeContentLine(t, "user", "m04", text(300, "c")) +
		claudeContentLine(t, "assistant", "m05", text(10, "d")) +
		claudeContentLine(t, "user", "m06", text(10, "e")) +
		claudeContentLine(t, "assistant", "m07", text(500, "f")) +
		claudeContentLine(t, "user", "m08", text(90, "g")) +
		claudeContentLine(t, "assistant", "m09", text(90, "h")) +
		claudeContentLine(t, "user", "m10", text(90, "i")) +
		claudeContentLine(t, "assistant", "m11", text(200, "j")) +
		claudeContentLine(t, "user", "m12", text(20, "k")) +
		`{"type":"user","uuid":"m13","message":{"content":"still being wr`
	path := filepath.Join(dir, "session-1.jsonl")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, []string{"m01", "m02", "m03", "m04", "m05", "m06", "m07", "m08", "m09", "m10", "m11", "m12"}
}

// claudePartsAfter is what a run remembers after the first kept parts of a
// read, and how many records those parts hold.
func claudePartsAfter(read domain.SessionPartsRead, kept int) (domain.SessionPartResume, int) {
	from, records := domain.SessionPartResume{}, 0
	for _, part := range read.Parts[:kept] {
		from = domain.SessionPartResume{Envelope: part.Envelope, Events: from.Events + len(part.Events), Token: part.Token}
		records += part.Records()
	}
	return from, records
}

// askedContext is cancelled from the time it has been asked a number of
// times whether it is; with no such number it only counts.
type askedContext struct {
	context.Context
	asked, cancelledAt int
}

func (c *askedContext) Err() error {
	c.asked++
	if c.cancelledAt > 0 && c.asked >= c.cancelledAt {
		return context.Canceled
	}
	return nil
}

// partsReadStopsWhenCancelled reads with a context that is cancelled at every
// moment the read asks, and sees it stop each time. A read asks at least once
// for every part it converts, beyond what it asks for no records at all.
func partsReadStopsWhenCancelled(t *testing.T, read func(ctx context.Context, ids []string) (domain.SessionPartsRead, error), ids []string) {
	t.Helper()
	none, counting := &askedContext{Context: context.Background()}, &askedContext{Context: context.Background()}
	if _, err := read(none, nil); err != nil {
		t.Fatal(err)
	}
	whole, err := read(counting, ids)
	if err != nil || len(whole.Parts) < 3 || counting.asked-none.asked < len(whole.Parts) {
		t.Fatalf("a read of %d parts asked %d times, one of no records %d times: %v", len(whole.Parts), counting.asked, none.asked, err)
	}
	for at := 1; at <= counting.asked; at++ {
		if stopped, err := read(&askedContext{Context: context.Background(), cancelledAt: at}, ids); !errors.Is(err, context.Canceled) || len(stopped.Parts) != 0 {
			t.Fatalf("cancelled when asked the %d. time: %d parts, %v", at, len(stopped.Parts), err)
		}
	}
}

func TestClaudePartsStopWhenTheReadIsCancelled(t *testing.T) {
	dir := t.TempDir()
	path, ids := claudePartsSession(t, dir)
	reader := NewClaudeContent()
	reader.partBytes = 700
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path}
	partsReadStopsWhenCancelled(t, func(ctx context.Context, ids []string) (domain.SessionPartsRead, error) {
		return reader.ReadSessionParts(ctx, dir, records, ids, domain.SessionPartResume{})
	}, ids)
}

// A session's conversation is read in parts. Going on from what was
// remembered after any sealed part gives the parts that reading everything
// gives: the same events, the same counts, the same place to go on from.
func TestClaudePartsReadFromARememberedPlaceAreThePartsOfAWholeRead(t *testing.T) {
	dir := t.TempDir()
	path, ids := claudePartsSession(t, dir)
	reader, ctx := NewClaudeContent(), context.Background()
	reader.partBytes = 700
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path}
	whole, err := reader.ReadSessionParts(ctx, dir, records, ids, domain.SessionPartResume{})
	if err != nil || len(whole.Missing) != 0 {
		t.Fatalf("whole read: %+v %v", whole.Missing, err)
	}
	// Several sealed parts and an open one, holding every record once.
	held, events := 0, 0
	for index, part := range whole.Parts {
		last := index == len(whole.Parts)-1
		if part.Records() < 1 || part.Sealed == last || (part.Token == "") != last {
			t.Fatalf("part %d: counts=%v sealed=%v token=%q", index, part.Counts, part.Sealed, part.Token)
		}
		sum := 0
		for _, count := range part.Counts {
			sum += count
		}
		if sum != len(part.Events) {
			t.Fatalf("part %d: counts hold %d events, the part %d", index, sum, len(part.Events))
		}
		for _, event := range part.Events {
			if event.Seq != events {
				t.Fatalf("event %d has sequence %d", events, event.Seq)
			}
			events++
		}
		held += part.Records()
	}
	// m02 gives two events, every other record one.
	if len(whole.Parts) < 4 || held != len(ids) || events != len(ids)+1 {
		t.Fatalf("parts=%d records=%d events=%d", len(whole.Parts), held, events)
	}
	if envelope := whole.Parts[len(whole.Parts)-1].Envelope; envelope.SessionOriginID != "session-1" || envelope.Cwd != "/work/app" || envelope.SourceProvider != domain.ProviderClaude {
		t.Fatalf("envelope after the last part: %+v", envelope)
	}
	raw, _ := json.Marshal(whole.Parts)
	for _, absent := range []string{"earlier session", "compact_boundary", "still being", "a later line"} {
		if strings.Contains(string(raw), absent) {
			t.Fatalf("the parts hold %q", absent)
		}
	}
	// A part is a document of its own: canonical, naming the session, and no
	// folder or branch of this computer. The whole is its parts one after
	// another, with the envelope the last one left.
	for index, part := range whole.Parts {
		doc := part.Document()
		if _, err := domain.CanonicalBytes(doc); err != nil || doc.Envelope.Cwd != "" || doc.Envelope.GitBranch != "" || doc.Envelope.SessionOriginID != "session-1" || len(doc.Events) != len(part.Events) {
			t.Fatalf("the document of part %d: %+v %v", index, doc.Envelope, err)
		}
	}
	conversation := domain.SessionPartsConversation(whole.Parts)
	if len(conversation.Events) != events || conversation.Envelope.Cwd != "" || conversation.Envelope.SessionOriginID != "session-1" {
		t.Fatalf("the whole conversation: %d events, %+v", len(conversation.Events), conversation.Envelope)
	}
	// m01 is a message, m02 a message and a tool call, m03 the tool's result.
	if kinds := claudeContentKinds(conversation.Events[:4]); !reflect.DeepEqual(kinds, []string{"message", "message", "tool_call", "tool_result"}) {
		t.Fatalf("the first events: %v", kinds)
	}

	for kept := 1; kept < len(whole.Parts); kept++ {
		from, records := claudePartsAfter(whole, kept)
		for name, token := range map[string]string{
			"the remembered place":             from.Token,
			"no remembered place":              "",
			"a place that is not one":          "not a place",
			"a place one byte further":         strings.Replace(from.Token, `"offset":`, `"offset":1`, 1),
			"a place of another record":        strings.Replace(from.Token, `"id":"m`, `"id":"x`, 1),
			"a place of a line cut short":      strings.Replace(from.Token, `"length":`, `"length":1`, 1),
			"a place before the file":          strings.Replace(from.Token, `"offset":`, `"offset":-`, 1),
			"a place past the end of the file": strings.Replace(from.Token, `"offset":`, `"offset":99999`, 1),
		} {
			resume := from
			resume.Token = token
			rest, err := reader.ReadSessionParts(ctx, dir, domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path}, ids[records:], resume)
			if err != nil || len(rest.Missing) != 0 || !reflect.DeepEqual(rest.Parts, whole.Parts[kept:]) {
				t.Fatalf("after %d parts, from %s: %d parts, missing %v, %v", kept, name, len(rest.Parts), rest.Missing, err)
			}
		}
	}

	// The session goes on. What was sealed stays; the open part is read again
	// with what was added, and the whole is what a read from the start gives.
	more := claudeContentLine(t, "assistant", "m13", strings.Repeat("l", 400)) + claudeContentLine(t, "user", "m14", strings.Repeat("m", 30)) + claudeContentLine(t, "assistant", "m15", strings.Repeat("n", 900))
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The line that was still being written is now the record m13.
	if err := os.WriteFile(path, append(file[:strings.LastIndex(string(file), "\n")+1], more...), 0o600); err != nil {
		t.Fatal(err)
	}
	longer := append(append([]string(nil), ids...), "m13", "m14", "m15")
	grown, err := reader.ReadSessionParts(ctx, dir, records, longer, domain.SessionPartResume{})
	if err != nil || len(grown.Missing) != 0 || len(grown.Parts) <= len(whole.Parts) {
		t.Fatalf("grown read: %d parts %v %v", len(grown.Parts), grown.Missing, err)
	}
	sealed := len(whole.Parts) - 1
	if !reflect.DeepEqual(grown.Parts[:sealed], whole.Parts[:sealed]) {
		t.Fatal("a sealed part is another one after the session went on")
	}
	from, held := claudePartsAfter(whole, sealed)
	rest, err := reader.ReadSessionParts(ctx, dir, records, longer[held:], from)
	if err != nil || !reflect.DeepEqual(rest.Parts, grown.Parts[sealed:]) {
		t.Fatalf("going on after the sealed parts: %d parts, %v", len(rest.Parts), err)
	}
}

// The records after a remembered place are looked for after it, and nothing
// before it is read again: the tool only adds to its file.
func TestClaudePartsAreLookedForAfterTheRememberedPlace(t *testing.T) {
	dir := t.TempDir()
	path, ids := claudePartsSession(t, dir)
	reader, ctx := NewClaudeContent(), context.Background()
	reader.partBytes = 700
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path}
	whole, err := reader.ReadSessionParts(ctx, dir, records, ids, domain.SessionPartResume{})
	if err != nil {
		t.Fatal(err)
	}
	from, held := claudePartsAfter(whole, 1)
	remembered := from
	// A record that is before the remembered place is not found after it. The
	// whole file is read then, and the answer is the one a read without the
	// place gives; no place is remembered for records that are out of order.
	unordered, plain := append(append([]string(nil), ids[held:]...), "m01"), from
	plain.Token = ""
	want, err := reader.ReadSessionParts(ctx, dir, records, unordered, plain)
	if err != nil || len(want.Missing) != 0 || len(want.Parts) == 0 {
		t.Fatalf("records out of order, read from the start: %d parts, missing %v, %v", len(want.Parts), want.Missing, err)
	}
	got, err := reader.ReadSessionParts(ctx, dir, records, unordered, from)
	if err != nil || len(got.Missing) != 0 || !reflect.DeepEqual(got.Parts, want.Parts) {
		t.Fatalf("records out of order, read from the remembered place: %d parts, missing %v, %v", len(got.Parts), got.Missing, err)
	}
	for index, part := range got.Parts {
		if part.Token != "" {
			t.Fatalf("part %d remembers a place although its records were not found in order", index)
		}
	}
	// The record another session holds, at the start of the file, is given the
	// identifier of the last record. Read from the start it would now be that
	// record; after the remembered place the real one still is.
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(file), `"uuid":"z00"`, `"uuid":"m12"`, 1)
	if len(changed) != len(file) || changed == string(file) {
		t.Fatal("the fixture was not changed in place")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	rest, err := reader.ReadSessionParts(ctx, dir, records, ids[held:], from)
	if err != nil || !reflect.DeepEqual(rest.Parts, whole.Parts[1:]) {
		t.Fatalf("read after the remembered place: %d parts, %v", len(rest.Parts), err)
	}
	// Without the place the file is read from its start, where the changed line comes first.
	from.Token = ""
	other, err := reader.ReadSessionParts(ctx, dir, records, ids[held:], from)
	raw, _ := json.Marshal(other.Parts)
	if err != nil || !strings.Contains(string(raw), "earlier session") {
		t.Fatalf("read from the start: %v", err)
	}
	// A record found before the one named before it leaves no place to go on from.
	for index, part := range other.Parts {
		if part.Token != "" {
			t.Fatalf("part %d remembers a place although its records were not found in order", index)
		}
	}
	// The line at the remembered place now holds another record, as in a file
	// that was written anew. That is no place to go on from: the file is read
	// from its start.
	last := ids[held-1]
	anew := strings.Replace(changed, `"uuid":"`+last+`"`, `"uuid":"q`+last[1:]+`"`, 1)
	if len(anew) != len(changed) || anew == changed {
		t.Fatal("the record at the remembered place was not changed in place")
	}
	if err := os.WriteFile(path, []byte(anew), 0o600); err != nil {
		t.Fatal(err)
	}
	rewritten, err := reader.ReadSessionParts(ctx, dir, records, ids[held:], remembered)
	raw, _ = json.Marshal(rewritten.Parts)
	if err != nil || len(rewritten.Missing) != 0 || !strings.Contains(string(raw), "earlier session") {
		t.Fatalf("read from a place that holds another record: missing %v, %v", rewritten.Missing, err)
	}
}

func TestClaudePartsNameWhatCannotBeReadBack(t *testing.T) {
	dir := t.TempDir()
	path, ids := claudePartsSession(t, dir)
	reader, ctx := NewClaudeContent(), context.Background()
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path}
	// m13 is still being written; "gone" was never there.
	read, err := reader.ReadSessionParts(ctx, dir, records, append(append([]string(nil), ids...), "m13", "gone"), domain.SessionPartResume{})
	if err != nil || !reflect.DeepEqual(read.Missing, []string{"m13", "gone"}) || len(read.Parts) != 0 {
		t.Fatalf("missing=%v parts=%d %v", read.Missing, len(read.Parts), err)
	}
	// The limit of a part is the domain's.
	if reader.partBytes != domain.SessionPartBytes {
		t.Fatalf("a part is sealed at %d bytes", reader.partBytes)
	}
	// With the real limit this short session is one open part.
	if read, err := reader.ReadSessionParts(ctx, dir, records, ids, domain.SessionPartResume{}); err != nil || len(read.Parts) != 1 || read.Parts[0].Sealed || read.Parts[0].Token != "" || read.Parts[0].Records() != len(ids) {
		t.Fatalf("one open part: %+v %v", read.Parts, err)
	}
	if read, err := reader.ReadSessionParts(ctx, dir, records, nil, domain.SessionPartResume{}); err != nil || len(read.Parts) != 0 || len(read.Missing) != 0 {
		t.Fatalf("no records: %+v %v", read, err)
	}
	for name, other := range map[string]domain.SessionRecordRanges{
		"a Codex thread":               {Provider: domain.ProviderCodex, ID: "session-1", Source: path},
		"a session without its file":   {Provider: domain.ProviderClaude, ID: "session-1"},
		"a session whose file is gone": {Provider: domain.ProviderClaude, ID: "session-1", Source: filepath.Join(dir, "absent.jsonl")},
	} {
		if _, err := reader.ReadSessionParts(ctx, dir, other, ids, domain.SessionPartResume{}); err == nil {
			t.Errorf("%s was read", name)
		}
	}
}

func TestClaudePartsMaskLikeCapture(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".cxtsecrets"), []byte("synthetic-listed-value-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-1.jsonl")
	file := claudeContentLine(t, "user", "m1", "the value is synthetic-listed-value-123 at https://user:synthetic-password@example.invalid/path") +
		claudeContentLine(t, "user", "m2", []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "printed synthetic-listed-value-123"}})
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewClaudeContent()
	// A part for each record: both are masked, in whichever part they are.
	reader.partBytes = 1
	read, err := reader.ReadSessionParts(context.Background(), dir, domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path}, []string{"m1", "m2"}, domain.SessionPartResume{})
	if err != nil || len(read.Parts) != 2 {
		t.Fatalf("parts=%d %v", len(read.Parts), err)
	}
	raw, _ := json.Marshal(read.Parts)
	if strings.Contains(string(raw), "synthetic-listed-value-123") || strings.Contains(string(raw), "synthetic-password") {
		t.Fatalf("a secret reached the content: %s", raw)
	}
	// Both steps ran: the repository's listed value, and the pattern scrub.
	if !strings.Contains(string(raw), "deleted by security policy") || !strings.Contains(string(raw), "redacted:password") {
		t.Fatalf("masking markers are absent: %s", raw)
	}
}
