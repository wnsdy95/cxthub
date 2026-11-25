package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func sessionUploadFixture(t *testing.T) (*SessionUploadFiles, string, domain.AnchoredSessionUpload, *domain.CIRDocument) {
	t.Helper()
	root := t.TempDir()
	files := NewSessionUploadFiles(func(context.Context, string) (string, bool) { return root, true })
	upload := domain.AnchoredSessionUpload{Provider: domain.ProviderClaude, SessionID: "session-1", Body: domain.AnchoredSessionBody{
		RuleVersion: 1, Records: 2, Segments: []domain.AnchoredSegment{}, Content: &domain.AnchoredContent{Events: 2, Parts: []domain.AnchoredContentPart{{Doc: "sha256:" + domain.ContentHash(strings.Repeat("a", 64)), Events: 2}}},
	}}
	doc := &domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: "session-1"},
		Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic question"}}}, {Seq: 1, Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic answer"}}}}}
	return files, root, upload, doc
}

func sessionUploadEntry(at string, upload domain.AnchoredSessionUpload, state domain.SegmentPushState, revision int64) domain.SessionUploadLogEntry {
	entry := domain.SessionUploadLogEntry{At: at, Report: state, Revision: revision, ReportHash: domain.Fingerprint(upload.Body), Content: domain.SegmentContentStored}
	if upload.Body.Content != nil {
		entry.Doc, entry.Parts, entry.Events = upload.Body.Content.Last(), len(upload.Body.Content.Parts), upload.Body.Content.Events
	}
	return entry
}

func TestSessionUploadFilesKeepWhatWasSent(t *testing.T) {
	files, root, upload, doc := sessionUploadFixture(t)
	ctx := context.Background()
	key := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}
	if err := files.KeepSessionUpload(ctx, root, upload, doc, sessionUploadEntry("2031-01-02T03:00:00Z", upload, domain.SegmentPushStored, 1)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".cxt", "sessions", "claude", "session-1")
	// The three files are the user's alone, and the conversation is the
	// document that was sent, packed.
	for _, name := range []string{"report.json", "conversation.json.gz", "log.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	packed, _ := os.ReadFile(filepath.Join(dir, "conversation.json.gz"))
	reader, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(reader)
	var kept domain.CIRDocument
	if err := json.Unmarshal(plain, &kept); err != nil || len(kept.Events) != 2 || kept.Events[1].Blocks[0].Text != "synthetic answer" {
		t.Fatalf("conversation copy: %v %s", err, plain)
	}
	record, err := files.ReadSessionUpload(ctx, root, key, true)
	if err != nil || record.Report == nil || !reflect.DeepEqual(*record.Report, upload.Body) || record.Conversation == nil || len(record.Conversation.Events) != 2 || len(record.Log) != 1 {
		t.Fatalf("record: %+v %v", record, err)
	}
	if record.Log[0].Report != domain.SegmentPushStored || record.Log[0].Revision != 1 || record.Log[0].ReportHash != domain.Fingerprint(upload.Body) {
		t.Fatalf("log: %+v", record.Log)
	}

	// A run in which nothing was sent and nothing failed leaves no line and
	// rewrites no file.
	before, _ := os.Stat(filepath.Join(dir, "conversation.json.gz"))
	if err := files.KeepSessionUpload(ctx, root, upload, nil, sessionUploadEntry("2031-01-02T03:01:00Z", upload, domain.SegmentPushUnchanged, 0)); err != nil {
		t.Fatal(err)
	}
	if err := files.KeepSessionUpload(ctx, root, upload, doc, sessionUploadEntry("2031-01-02T03:02:00Z", upload, domain.SegmentPushUnchanged, 1)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, "conversation.json.gz"))
	if record, _ := files.ReadSessionUpload(ctx, root, key, false); len(record.Log) != 1 || !after.ModTime().Equal(before.ModTime()) || record.Conversation != nil {
		t.Fatalf("an uneventful run was logged or rewrote the conversation: %+v", record.Log)
	}

	// The session went on: a new report, a new document, one new line.
	grown := upload
	grown.Body.Records, grown.Body.Content = 3, &domain.AnchoredContent{Events: 3, Parts: []domain.AnchoredContentPart{{Doc: "sha256:" + domain.ContentHash(strings.Repeat("b", 64)), Events: 3}}}
	longer := *doc
	longer.Events = append(append([]domain.Event(nil), doc.Events...), domain.Event{Seq: 2, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "one more"}}})
	if err := files.KeepSessionUpload(ctx, root, grown, &longer, sessionUploadEntry("2031-01-02T03:03:00Z", grown, domain.SegmentPushStored, 2)); err != nil {
		t.Fatal(err)
	}
	record, _ = files.ReadSessionUpload(ctx, root, key, true)
	if len(record.Log) != 2 || record.Report.Records != 3 || len(record.Conversation.Events) != 3 || record.Log[1].Doc != grown.Body.Content.Last() {
		t.Fatalf("grown session: %+v", record)
	}

	// A failure is logged once however often it repeats, and again when it
	// changes. A refused report leaves the copy of the report as it was.
	refused := grown
	refused.Body.Records = 4
	for _, at := range []string{"2031-01-02T03:04:00Z", "2031-01-02T03:05:00Z"} {
		entry := domain.SessionUploadLogEntry{At: at, Automatic: true, Report: domain.SegmentPushRefused, ReportHash: domain.Fingerprint(refused.Body), Reason: "synthetic refusal"}
		if err := files.KeepSessionUpload(ctx, root, refused, nil, entry); err != nil {
			t.Fatal(err)
		}
	}
	record, _ = files.ReadSessionUpload(ctx, root, key, false)
	if len(record.Log) != 3 || record.Log[2].Reason != "synthetic refusal" || !record.Log[2].Automatic || record.Report.Records != 3 {
		t.Fatalf("a repeated refusal: %+v report records %d", record.Log, record.Report.Records)
	}

	// Something done to the session that is not an upload has its own line.
	if err := files.LogSessionAction(ctx, root, key, domain.SessionUploadLogEntry{At: "2031-01-02T03:06:00Z", Action: "removed"}); err != nil {
		t.Fatal(err)
	}
	if record, _ := files.ReadSessionUpload(ctx, root, key, false); len(record.Log) != 4 || record.Log[3].Action != "removed" {
		t.Fatalf("action: %+v", record.Log)
	}
}

func TestSessionUploadFilesListReadAndRefuse(t *testing.T) {
	files, root, upload, doc := sessionUploadFixture(t)
	ctx := context.Background()
	other := upload
	other.Provider, other.SessionID = domain.ProviderCodex, "thread-1"
	for _, each := range []domain.AnchoredSessionUpload{upload, other} {
		if err := files.KeepSessionUpload(ctx, root, each, doc, sessionUploadEntry("2031-01-02T03:00:00Z", each, domain.SegmentPushStored, 1)); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := files.ListSessionUploads(ctx, root)
	if want := []domain.SessionUploadKey{{Provider: domain.ProviderClaude, SessionID: "session-1"}, {Provider: domain.ProviderCodex, SessionID: "thread-1"}}; err != nil || !reflect.DeepEqual(keys, want) {
		t.Fatalf("list: %+v %v", keys, err)
	}
	if _, err := files.ReadSessionUpload(ctx, root, domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "absent"}, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a session nothing is kept for: %v", err)
	}
	// A key that could leave the folder is refused, and nothing is written.
	for _, key := range []domain.SessionUploadKey{{Provider: domain.ProviderClaude, SessionID: "../escape"}, {Provider: "other", SessionID: "session-1"}, {Provider: domain.ProviderClaude, SessionID: ".."}} {
		bad := upload
		bad.Provider, bad.SessionID = key.Provider, key.SessionID
		if err := files.KeepSessionUpload(ctx, root, bad, doc, sessionUploadEntry("2031-01-02T03:00:00Z", bad, domain.SegmentPushStored, 1)); err == nil {
			t.Fatalf("%+v was kept", key)
		}
		if _, err := files.ReadSessionUpload(ctx, root, key, false); err == nil {
			t.Fatalf("%+v was read", key)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt", "escape")); err == nil {
		t.Fatal("a file was written outside the sessions folder")
	}
	// A log line cut short by a crash is left out, and the next line starts
	// on a line of its own.
	log := filepath.Join(root, ".cxt", "sessions", "claude", "session-1", "log.jsonl")
	file, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = file.WriteString(`{"at":"2031-01-02T03:0`)
	_ = file.Close()
	key := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}
	if err := files.LogSessionAction(ctx, root, key, domain.SessionUploadLogEntry{At: "2031-01-02T03:09:00Z", Action: "archived"}); err != nil {
		t.Fatal(err)
	}
	if record, err := files.ReadSessionUpload(ctx, root, key, false); err != nil || len(record.Log) != 2 || record.Log[1].Action != "archived" {
		t.Fatalf("after a cut line: %+v %v", record.Log, err)
	}
	// A symbolic link in place of the folder is not followed.
	elsewhere := t.TempDir()
	linked := NewSessionUploadFiles(func(context.Context, string) (string, bool) { return elsewhere, true })
	if err := os.MkdirAll(filepath.Join(elsewhere, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(elsewhere, ".cxt", "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := linked.KeepSessionUpload(ctx, elsewhere, upload, doc, sessionUploadEntry("2031-01-02T03:00:00Z", upload, domain.SegmentPushStored, 1)); err == nil {
		t.Fatal("a symbolic link was followed")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("files were written through the link: %v", entries)
	}
	// Where cxt is not set up nothing is kept and nothing fails.
	nowhere := NewSessionUploadFiles(func(context.Context, string) (string, bool) { return "", false })
	if err := nowhere.KeepSessionUpload(ctx, root, upload, doc, sessionUploadEntry("2031-01-02T03:00:00Z", upload, domain.SegmentPushStored, 1)); err != nil {
		t.Fatal(err)
	}
	if keys, err := nowhere.ListSessionUploads(ctx, root); err != nil || len(keys) != 0 {
		t.Fatalf("not set up: %v %v", keys, err)
	}
}
