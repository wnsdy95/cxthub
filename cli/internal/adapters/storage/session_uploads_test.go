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

// sessionUploadDoc is the hash a synthetic document is known by.
func sessionUploadDoc(letter string) domain.ContentHash {
	return "sha256:" + domain.ContentHash(strings.Repeat(letter, 64))
}

// sessionUploadPartOf is a document of one message for each text, the
// first at position first of its conversation.
func sessionUploadPartOf(letter string, first int, texts ...string) domain.SessionPartDocument {
	doc := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: "session-1"}}
	for index, text := range texts {
		doc.Events = append(doc.Events, domain.Event{Seq: first + index, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	return domain.SessionPartDocument{Doc: sessionUploadDoc(letter), Document: doc}
}

// sessionUploadOf is a report whose conversation is the given documents.
func sessionUploadOf(parts ...domain.SessionPartDocument) domain.AnchoredSessionUpload {
	content := &domain.AnchoredContent{}
	for _, part := range parts {
		content.Parts = append(content.Parts, domain.AnchoredContentPart{Doc: part.Doc, Events: len(part.Document.Events)})
		content.Events += len(part.Document.Events)
	}
	return domain.AnchoredSessionUpload{Provider: domain.ProviderClaude, SessionID: "session-1", Body: domain.AnchoredSessionBody{
		RuleVersion: 1, Records: content.Events, Segments: []domain.AnchoredSegment{}, Content: content}}
}

func sessionUploadFixture(t *testing.T) (*SessionUploadFiles, string, domain.AnchoredSessionUpload, []domain.SessionPartDocument) {
	t.Helper()
	root := t.TempDir()
	files := NewSessionUploadFiles(func(context.Context, string) (string, bool) { return root, true })
	built := []domain.SessionPartDocument{sessionUploadPartOf("a", 0, "synthetic question", "synthetic answer")}
	return files, root, sessionUploadOf(built...), built
}

func sessionUploadEntry(at string, upload domain.AnchoredSessionUpload, state domain.SegmentPushState, revision int64) domain.SessionUploadLogEntry {
	entry := domain.SessionUploadLogEntry{At: at, Report: state, Revision: revision, ReportHash: domain.Fingerprint(upload.Body), Content: domain.SegmentContentStored}
	if upload.Body.Content != nil {
		entry.Doc, entry.Parts, entry.Events = upload.Body.Content.Last(), len(upload.Body.Content.Parts), upload.Body.Content.Events
	}
	return entry
}

// sessionUploadKept lists the files of the conversation folder with when
// each was last written.
func sessionUploadKept(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "conversation"))
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]int64{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", entry.Name(), info, err)
		}
		kept[strings.TrimSuffix(entry.Name(), ".json.gz")[:1]] = info.ModTime().UnixNano()
	}
	return kept
}

func TestSessionUploadFilesKeepWhatWasSent(t *testing.T) {
	files, root, upload, built := sessionUploadFixture(t)
	ctx := context.Background()
	key := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}
	if err := files.KeepSessionUpload(ctx, root, upload, built, sessionUploadEntry("2031-01-02T03:00:00Z", upload, domain.SegmentPushStored, 1)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".cxt", "sessions", "claude", "session-1")
	// The files are the user's alone, and the conversation is the document
	// that was sent, packed, under the hash it is known by.
	first := filepath.Join("conversation", strings.Repeat("a", 64)+".json.gz")
	for _, name := range []string{"report.json", first, "log.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	packed, _ := os.ReadFile(filepath.Join(dir, first))
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
	if err != nil || record.Report == nil || !reflect.DeepEqual(*record.Report, upload.Body) || !record.ConversationKept || !reflect.DeepEqual(record.Parts, built) || len(record.Log) != 1 {
		t.Fatalf("record: %+v %v", record, err)
	}
	if record.Log[0].Report != domain.SegmentPushStored || record.Log[0].Revision != 1 || record.Log[0].ReportHash != domain.Fingerprint(upload.Body) {
		t.Fatalf("log: %+v", record.Log)
	}

	// A run in which nothing was sent and nothing failed leaves no line and
	// rewrites no file.
	before := sessionUploadKept(t, dir)
	if err := files.KeepSessionUpload(ctx, root, upload, nil, sessionUploadEntry("2031-01-02T03:01:00Z", upload, domain.SegmentPushUnchanged, 0)); err != nil {
		t.Fatal(err)
	}
	if err := files.KeepSessionUpload(ctx, root, upload, built, sessionUploadEntry("2031-01-02T03:02:00Z", upload, domain.SegmentPushUnchanged, 1)); err != nil {
		t.Fatal(err)
	}
	// Without the conversation asked for, the record says only that it is kept.
	if record, _ := files.ReadSessionUpload(ctx, root, key, false); len(record.Log) != 1 || !reflect.DeepEqual(sessionUploadKept(t, dir), before) || !record.ConversationKept || record.Parts != nil {
		t.Fatalf("an uneventful run was logged or rewrote the conversation: %+v", record.Log)
	}

	// The session went on: a further document. The first is not built again
	// and its file stays as it is; one new line.
	third := sessionUploadPartOf("c", 2, "one more")
	grown := sessionUploadOf(built[0], third)
	if err := files.KeepSessionUpload(ctx, root, grown, []domain.SessionPartDocument{third}, sessionUploadEntry("2031-01-02T03:03:00Z", grown, domain.SegmentPushStored, 2)); err != nil {
		t.Fatal(err)
	}
	record, _ = files.ReadSessionUpload(ctx, root, key, true)
	if after := sessionUploadKept(t, dir); len(after) != 2 || after["a"] != before["a"] || len(record.Log) != 2 || record.Report.Records != 3 ||
		!reflect.DeepEqual(record.Parts, []domain.SessionPartDocument{built[0], third}) || record.Log[1].Doc != third.Doc || record.Log[1].Parts != 2 {
		t.Fatalf("grown session: files %v record %+v", after, record)
	}

	// The last document is another one: its file replaces the earlier last
	// one, and a document that was built and is not named is not kept.
	fourth := sessionUploadPartOf("d", 2, "one more", "and its answer")
	replaced := sessionUploadOf(built[0], fourth)
	if err := files.KeepSessionUpload(ctx, root, replaced, []domain.SessionPartDocument{fourth, sessionUploadPartOf("e", 9, "not named")}, sessionUploadEntry("2031-01-02T03:03:30Z", replaced, domain.SegmentPushStored, 3)); err != nil {
		t.Fatal(err)
	}
	record, _ = files.ReadSessionUpload(ctx, root, key, true)
	after := sessionUploadKept(t, dir)
	if _, gone := after["c"]; gone || len(after) != 2 || after["a"] != before["a"] || !reflect.DeepEqual(record.Parts, []domain.SessionPartDocument{built[0], fourth}) || len(record.Log) != 3 {
		t.Fatalf("replaced last document: files %v record %+v", after, record)
	}

	// A failure is logged once however often it repeats, and again when it
	// changes. A refused report leaves the copy of the report as it was, and
	// the documents kept are those of the report that is kept.
	fifth := sessionUploadPartOf("f", 4, "refused with its report")
	refused := sessionUploadOf(built[0], fourth, fifth)
	for _, at := range []string{"2031-01-02T03:04:00Z", "2031-01-02T03:05:00Z"} {
		entry := domain.SessionUploadLogEntry{At: at, Automatic: true, Report: domain.SegmentPushRefused, ReportHash: domain.Fingerprint(refused.Body), Reason: "synthetic refusal", Content: domain.SegmentContentStored}
		if err := files.KeepSessionUpload(ctx, root, refused, []domain.SessionPartDocument{fifth}, entry); err != nil {
			t.Fatal(err)
		}
	}
	record, _ = files.ReadSessionUpload(ctx, root, key, true)
	if len(record.Log) != 4 || record.Log[3].Reason != "synthetic refusal" || !record.Log[3].Automatic || record.Report.Records != 4 ||
		!reflect.DeepEqual(sessionUploadKept(t, dir), after) || len(record.Parts) != 2 {
		t.Fatalf("a repeated refusal: %+v report records %d files %v", record.Log, record.Report.Records, sessionUploadKept(t, dir))
	}

	// Something done to the session that is not an upload has its own line.
	if err := files.LogSessionAction(ctx, root, key, domain.SessionUploadLogEntry{At: "2031-01-02T03:06:00Z", Action: "removed"}); err != nil {
		t.Fatal(err)
	}
	if record, _ := files.ReadSessionUpload(ctx, root, key, false); len(record.Log) != 5 || record.Log[4].Action != "removed" {
		t.Fatalf("action: %+v", record.Log)
	}

	// The conversation is kept only while every document the report names
	// is: with one gone, nothing of it is read.
	if err := os.Remove(filepath.Join(dir, first)); err != nil {
		t.Fatal(err)
	}
	for _, withConversation := range []bool{false, true} {
		if record, err := files.ReadSessionUpload(ctx, root, key, withConversation); err != nil || record.ConversationKept || record.Parts != nil || record.Report == nil {
			t.Fatalf("with a document gone (conversation asked for: %v): %+v %v", withConversation, record, err)
		}
	}
	// A run that built only the other document cannot make the copy whole. It
	// says so, after it kept what it has and logged the run.
	lacking := files.KeepSessionUpload(ctx, root, replaced, []domain.SessionPartDocument{fourth}, sessionUploadEntry("2031-01-02T03:06:30Z", replaced, domain.SegmentPushStored, 4))
	if lacking == nil || !strings.Contains(lacking.Error(), "lacks 1 of the documents") {
		t.Fatalf("a copy with a document gone was kept: %v", lacking)
	}
	if record, err := files.ReadSessionUpload(ctx, root, key, false); err != nil || record.ConversationKept || len(record.Log) != 6 || record.Log[5].Revision != 4 {
		t.Fatalf("after a run that could not make the copy whole: %+v %v", record.Log, err)
	}
	// The file an earlier build kept the whole conversation in goes when the
	// documents are kept, and the document that was gone is written again.
	if err := os.WriteFile(filepath.Join(dir, "conversation.json.gz"), []byte("an earlier copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := files.KeepSessionUpload(ctx, root, replaced, []domain.SessionPartDocument{built[0], fourth}, sessionUploadEntry("2031-01-02T03:07:00Z", replaced, domain.SegmentPushUnchanged, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "conversation.json.gz")); !os.IsNotExist(err) {
		t.Fatalf("the earlier copy of the whole conversation: %v", err)
	}
	if record, err := files.ReadSessionUpload(ctx, root, key, true); err != nil || !record.ConversationKept || len(record.Parts) != 2 || len(record.Log) != 7 {
		t.Fatalf("after the documents were written again: %+v %v", record, err)
	}
	// What names no document names no file.
	outside := domain.SessionPartDocument{Doc: "../outside", Document: built[0].Document}
	bad := sessionUploadOf(outside)
	if err := files.KeepSessionUpload(ctx, root, bad, []domain.SessionPartDocument{outside}, sessionUploadEntry("2031-01-02T03:08:00Z", bad, domain.SegmentPushStored, 4)); err == nil {
		t.Fatal("a report that names no document by its hash was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside.json.gz")); !os.IsNotExist(err) {
		t.Fatalf("a file was written beside the conversation folder: %v", err)
	}
	if err := files.KeepSessionUpload(ctx, root, replaced, []domain.SessionPartDocument{{Doc: "../outside", Document: built[0].Document}}, sessionUploadEntry("2031-01-02T03:09:00Z", replaced, domain.SegmentPushStored, 4)); err == nil {
		t.Fatal("a document without a hash was kept")
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
