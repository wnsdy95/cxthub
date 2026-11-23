package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type uploadRecordsFake struct {
	kept []uploadRecordKept
	err  error
}

type uploadRecordKept struct {
	cwd, session string
	events       int
	entry        domain.SessionUploadLogEntry
}

func (f *uploadRecordsFake) KeepSessionUpload(_ context.Context, cwd string, upload domain.AnchoredSessionUpload, conversation *domain.CIRDocument, entry domain.SessionUploadLogEntry) error {
	if f.err != nil {
		return f.err
	}
	events := -1
	if conversation != nil {
		events = len(conversation.Events)
	}
	if entry.ReportHash != domain.Fingerprint(upload.Body) {
		return errors.New("the entry does not name the report that was sent")
	}
	f.kept = append(f.kept, uploadRecordKept{cwd: cwd, session: domain.CollectorSessionKey(upload.Provider, upload.SessionID), events: events, entry: entry})
	return nil
}

func (f *uploadRecordsFake) LogSessionAction(context.Context, string, domain.SessionUploadKey, domain.SessionUploadLogEntry) error {
	return nil
}

func (f *uploadRecordsFake) ListSessionUploads(context.Context, string) ([]domain.SessionUploadKey, error) {
	return nil, nil
}

func (f *uploadRecordsFake) ReadSessionUpload(context.Context, string, domain.SessionUploadKey, bool) (domain.SessionUploadRecord, error) {
	return domain.SessionUploadRecord{}, domain.ErrNotFound
}

func TestSegmentUploadKeepsACopyOfWhatItSent(t *testing.T) {
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{}
	records := &uploadRecordsFake{}
	at := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source}).
		WithSummaries(summaries, summarizer).WithUploadRecords(records, func() time.Time { return at })
	result, err := service.PushSegments(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	// Every session the run sent has its copy: the report that was sent, the
	// conversation read in this run, and what the server answered. A session
	// whose identifier cannot be a key is not sent and has none.
	sent := len(result.Sessions) - result.Refused
	if result.Refused != 1 || result.LocalRecords == nil || result.LocalRecords.Kept != sent || result.LocalRecords.NotKept != "" || len(records.kept) != sent {
		t.Fatalf("records: %+v kept=%d sessions=%d refused=%d", result.LocalRecords, len(records.kept), len(result.Sessions), result.Refused)
	}
	first := records.kept[0]
	if first.cwd != "/work/app" || first.session != "claude/with-commit" || first.events != 6 {
		t.Fatalf("first copy: %+v", first)
	}
	if e := first.entry; e.At != "2031-01-02T03:04:05Z" || e.Automatic || e.Report != domain.SegmentPushStored || e.Revision != 1 || e.Content != domain.SegmentContentStored || e.Doc == "" || e.Events != 6 || e.Summaries != 1 {
		t.Fatalf("first entry: %+v", e)
	}

	// A copy that cannot be written is said in the result and stops nothing.
	broken := &uploadRecordsFake{err: errors.New("synthetic disk failure")}
	remote = &segmentRemoteFake{}
	service = segmentUploadFixture(remote).WithUploadRecords(broken, time.Now)
	result, err = service.PushSegments(context.Background(), "/work/app")
	if err != nil || result.Stored != 2 || result.LocalRecords.Kept != 0 || result.LocalRecords.NotKept != "synthetic disk failure" {
		t.Fatalf("a failing copy: %+v %v", result, err)
	}

	// A run that keeps no copies says nothing about them.
	if result, err := segmentUploadFixture(&segmentRemoteFake{}).PushSegments(context.Background(), "/work/app"); err != nil || result.LocalRecords != nil {
		t.Fatalf("no copies: %+v %v", result.LocalRecords, err)
	}
}

func TestCollectorKeepsCopiesAndMarksItsRuns(t *testing.T) {
	f := newCollectorFixture(t)
	records := &uploadRecordsFake{}
	f.service.WithUploadRecords(records, func() time.Time { return time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC) })
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	first := f.run(t)
	if first.LocalRecords == nil || first.LocalRecords.Kept != len(first.Sessions) || len(records.kept) == 0 {
		t.Fatalf("first run: %+v", first.LocalRecords)
	}
	for _, kept := range records.kept {
		if !kept.entry.Automatic {
			t.Fatalf("an automatic run was not marked: %+v", kept.entry)
		}
	}
	// A run that reads nothing back hands over no conversation and the same
	// report, which the keeper answers by writing nothing.
	before := len(records.kept)
	f.run(t)
	for _, kept := range records.kept[before:] {
		if kept.events != -1 || kept.entry.Report != domain.SegmentPushUnchanged || kept.entry.Eventful() {
			t.Fatalf("a run with nothing new: %+v", kept)
		}
	}
}

type uploadRecordsStore struct {
	uploadRecordsFake
	records map[domain.SessionUploadKey]domain.SessionUploadRecord
}

func (f *uploadRecordsStore) ListSessionUploads(context.Context, string) ([]domain.SessionUploadKey, error) {
	keys := []domain.SessionUploadKey{}
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for key := range f.records {
			if key.Provider == provider {
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}

func (f *uploadRecordsStore) ReadSessionUpload(_ context.Context, _ string, key domain.SessionUploadKey, withConversation bool) (domain.SessionUploadRecord, error) {
	record, ok := f.records[key]
	if !ok || withConversation {
		return domain.SessionUploadRecord{}, domain.ErrNotFound
	}
	return record, nil
}

func TestUploadLogListsEverySessionOrOneWithItsWholeLog(t *testing.T) {
	claude, codex := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "session-1"}, domain.SessionUploadKey{Provider: domain.ProviderCodex, SessionID: "thread-1"}
	body := domain.AnchoredSessionBody{RuleVersion: 1, Records: 5, Segments: []domain.AnchoredSegment{{}, {}}, Content: &domain.AnchoredContent{Doc: "sha256:doc", Events: 9}}
	store := &uploadRecordsStore{records: map[domain.SessionUploadKey]domain.SessionUploadRecord{
		claude: {Provider: claude.Provider, SessionID: claude.SessionID, Report: &body, ConversationKept: true, Log: []domain.SessionUploadLogEntry{
			{At: "2031-01-02T03:00:00Z", Report: domain.SegmentPushStored, Revision: 1}, {At: "2031-01-02T03:01:00Z", Report: domain.SegmentPushStored, Revision: 2}}},
		codex: {Provider: codex.Provider, SessionID: codex.SessionID, Log: []domain.SessionUploadLogEntry{{At: "2031-01-02T03:02:00Z", Report: domain.SegmentPushRefused, Reason: "synthetic refusal"}}},
	}}
	care := NewSegmentCareService(store)
	all, err := care.UploadLog(context.Background(), "/work/app", nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %+v %v", all, err)
	}
	// Every session with what its copies hold and its last line only.
	if first := all[0]; first.SessionID != "session-1" || !first.ReportKept || !first.ConversationKept || first.Segments != 2 || first.Records != 5 || first.Doc != "sha256:doc" || first.Events != 9 ||
		first.ReportHash != domain.Fingerprint(body) || first.Lines != 2 || len(first.Log) != 1 || first.Log[0].Revision != 2 {
		t.Fatalf("first: %+v", first)
	}
	if second := all[1]; second.ReportKept || second.ConversationKept || second.ReportHash != "" || second.Lines != 1 || second.Log[0].Reason != "synthetic refusal" {
		t.Fatalf("second: %+v", second)
	}
	// One session with every line.
	one, err := care.UploadLog(context.Background(), "/work/app", &claude)
	if err != nil || len(one) != 1 || len(one[0].Log) != 2 || one[0].Log[0].Revision != 1 {
		t.Fatalf("one: %+v %v", one, err)
	}
	absent := domain.SessionUploadKey{Provider: domain.ProviderClaude, SessionID: "absent"}
	if _, err := care.UploadLog(context.Background(), "/work/app", &absent); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a session nothing is kept for: %v", err)
	}
	if _, err := NewSegmentCareService(nil).UploadLog(context.Background(), "/work/app", nil); err == nil {
		t.Fatal("a service without records must say so")
	}
}
