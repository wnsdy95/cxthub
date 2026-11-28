package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// collectorRead is what the content reader was asked for one session in the
// last run: the records, and how many events came before them.
func (f *collectorFixture) collectorRead(session string) (asked [][]string, from []domain.SessionPartResume) {
	for index, records := range f.source.read {
		if records.ID == session {
			asked, from = append(asked, f.source.asked[index]), append(from, f.source.from[index])
		}
	}
	return asked, from
}

// collectorGrow adds records to the session that made no commit.
func (f *collectorFixture) collectorGrow(ids ...string) {
	for _, id := range ids {
		f.now = f.now.Add(time.Minute)
		f.claude.batch.Sessions[1].Items = append(f.claude.batch.Sessions[1].Items, domain.RecordedItem{ID: id, Turn: "t-" + id, AtMs: f.now.UnixMilli(), Folder: "/work/app", Branch: "main"})
	}
	f.now = f.now.Add(time.Minute)
}

func (f *collectorFixture) collectorUpload(t *testing.T, session string) domain.AnchoredSessionBody {
	t.Helper()
	for _, upload := range f.remote.uploads {
		if upload.SessionID == session {
			return upload.Body
		}
	}
	t.Fatalf("no report of %s was sent", session)
	return domain.AnchoredSessionBody{}
}

// A session that goes on is read from where its sealed parts end. What was
// sealed is neither read nor sent again; the report names the sealed
// documents and what follows them, and every range keeps its place.
func TestCollectorGoesOnFromTheSealedPartsOfASession(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	// Two records seal a part; every record gives two events.
	f.source.sealedAt = 2
	key := domain.CollectorSessionKey(domain.ProviderClaude, "without-commit")
	f.collectorGrow("w2", "w3", "w4", "w5")
	if result := f.run(t); result.Stored != 2 {
		t.Fatalf("first run: %+v", result)
	}
	// Everything is read from the start, and three documents are sent for
	// the five records: two sealed and the open one.
	asked, from := f.collectorRead("without-commit")
	if !reflect.DeepEqual(asked, [][]string{{"w1", "w2", "w3", "w4", "w5"}}) || !reflect.DeepEqual(from, []domain.SessionPartResume{{}}) {
		t.Fatalf("first read: %v from %+v", asked, from)
	}
	first := f.collectorUpload(t, "without-commit")
	if first.Content == nil || first.Content.Events != 10 || len(first.Content.Parts) != 3 || first.Content.Parts[0].Events != 4 || first.Content.Parts[2].Events != 2 ||
		first.Uncommitted.FirstEvent != 0 || first.Uncommitted.Events != 10 {
		t.Fatalf("first report: %+v %+v", first.Content, first.Uncommitted)
	}
	// The documents name no folder of this computer; what is remembered to go on from does.
	for _, doc := range f.content.docs {
		if doc.Envelope.Cwd != "" {
			t.Fatalf("a document names a folder: %+v", doc.Envelope)
		}
	}
	sealed := f.state.state.Sessions[key].Parts
	if f.state.state.Sessions[key].PartsEpoch != "epoch-1" || len(sealed) != 2 || sealed[0].Records() != 2 || !reflect.DeepEqual(sealed[1].Counts, []int{2, 2}) ||
		sealed[0].Chain != domain.SessionRecordChain("", []string{"w1", "w2"}) || sealed[1].Chain != domain.SessionRecordChain(sealed[0].Chain, []string{"w3", "w4"}) ||
		sealed[0].Part != first.Content.Parts[0] || sealed[1].Part != first.Content.Parts[1] ||
		sealed[1].Resume.Events != 8 || sealed[1].Resume.Token != "after w4" || sealed[1].Resume.Envelope.Cwd != "/work/app" {
		t.Fatalf("remembered parts: %+v", sealed)
	}

	// The same records again: nothing is read and nothing is sent.
	if result := f.run(t); result.Unchanged != 2 || len(f.source.read) != 0 || len(f.content.docs) != 0 {
		t.Fatalf("an unchanged run: %+v reads=%d", result, len(f.source.read))
	}

	// Two more records. Only what follows the sealed parts is read, from
	// where they end, and only the documents after them are sent.
	f.collectorGrow("w6", "w7")
	if result := f.run(t); result.Stored != 1 {
		t.Fatalf("the run after the session went on: %+v", result)
	}
	asked, from = f.collectorRead("without-commit")
	if !reflect.DeepEqual(asked, [][]string{{"w5", "w6", "w7"}}) || len(from) != 1 || from[0].Events != 8 || from[0].Token != "after w4" || from[0].Envelope.SessionOriginID != "without-commit" {
		t.Fatalf("the read after the session went on: %v from %+v", asked, from)
	}
	if len(f.content.docs) != 2 || len(f.content.docs[0].Events) != 4 || f.content.docs[0].Events[0].Seq != 8 || len(f.content.docs[1].Events) != 2 {
		t.Fatalf("documents sent: %d", len(f.content.docs))
	}
	second := f.collectorUpload(t, "without-commit")
	if second.Content.Events != 14 || len(second.Content.Parts) != 4 || !reflect.DeepEqual(second.Content.Parts[:2], first.Content.Parts[:2]) || second.Content.Parts[2] == first.Content.Parts[2] ||
		second.Uncommitted.FirstEvent != 0 || second.Uncommitted.Events != 14 {
		t.Fatalf("second report: %+v %+v", second.Content, second.Uncommitted)
	}
	// Every turn of the uncommitted part is placed in the whole conversation.
	for index, turn := range second.Uncommitted.Turns {
		if turn.FirstEvent != 2*index {
			t.Fatalf("turn %d begins at event %d", index, turn.FirstEvent)
		}
	}
	if sealed := f.state.state.Sessions[key].Parts; len(sealed) != 3 || sealed[2].Resume.Events != 12 || sealed[2].Part != second.Content.Parts[2] ||
		sealed[2].Chain != domain.SessionRecordChain(sealed[1].Chain, []string{"w5", "w6"}) {
		t.Fatalf("remembered after the session went on: %+v", sealed)
	}
	// The part sealed by the run that went on is kept by the next run like
	// the two before it.
	f.collectorGrow("w8")
	if result := f.run(t); result.Stored != 1 {
		t.Fatalf("the second run after the session went on: %+v", result)
	}
	asked, from = f.collectorRead("without-commit")
	if !reflect.DeepEqual(asked, [][]string{{"w7", "w8"}}) || from[0].Events != 12 || from[0].Token != "after w6" {
		t.Fatalf("the second read after the session went on: %v from %+v", asked, from)
	}

	// A record of a sealed part is another one: that part and what follows
	// it are built again, and the part before it is kept.
	f.claude.batch.Sessions[1].Items[2].ID = "x3"
	if result := f.run(t); result.Stored != 1 {
		t.Fatalf("the run after a record changed: %+v", result)
	}
	asked, from = f.collectorRead("without-commit")
	if !reflect.DeepEqual(asked, [][]string{{"x3", "w4", "w5", "w6", "w7", "w8"}}) || from[0].Events != 4 || from[0].Token != "after w2" {
		t.Fatalf("the read after a record changed: %v from %+v", asked, from)
	}
	third := f.collectorUpload(t, "without-commit")
	if len(third.Content.Parts) != 4 || third.Content.Parts[0] != first.Content.Parts[0] || third.Content.Parts[1] == first.Content.Parts[1] {
		t.Fatalf("report after a record changed: %+v", third.Content)
	}

	// Another epoch masks otherwise: nothing that was sealed holds.
	f.epoch = "epoch-2"
	f.run(t)
	asked, from = f.collectorRead("without-commit")
	if len(asked) != 1 || len(asked[0]) != 8 || !reflect.DeepEqual(from, []domain.SessionPartResume{{}}) || f.state.state.Sessions[key].PartsEpoch != "epoch-2" {
		t.Fatalf("the read in another epoch: %v from %+v", asked, from)
	}

	// The run a person asks for remembers nothing and builds everything.
	f.source.read, f.source.asked, f.source.from, f.content.docs = nil, nil, nil, nil
	if _, err := f.service.PushSegments(t.Context(), "/work/app"); err != nil {
		t.Fatal(err)
	}
	if asked, from = f.collectorRead("without-commit"); len(asked) != 1 || len(asked[0]) != 8 || !reflect.DeepEqual(from, []domain.SessionPartResume{{}}) {
		t.Fatalf("the read of a requested run: %v from %+v", asked, from)
	}
}

// A summary is written from the whole conversation. A run that went on from
// sealed parts holds only what follows them, so the conversation is read from
// its start then, and only then.
func TestConversationIsReadFromItsStartOnlyWhenASummaryIsWritten(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	f.source.sealedAt = 2
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{}
	f.service.WithSummaries(summaries, summarizer)
	key := domain.CollectorSessionKey(domain.ProviderClaude, "with-commit")
	// The first run reads everything once and writes the summary from it.
	if result := f.run(t); result.Summaries != 1 {
		t.Fatalf("first run: %+v", result)
	}
	if asked, _ := f.collectorRead("with-commit"); !reflect.DeepEqual(asked, [][]string{{"m1", "m2", "m3"}}) || !reflect.DeepEqual(summarizer.asked, []string{"/work/app claude/with-commit 6 events [6]"}) {
		t.Fatalf("first run: reads %v, summarizer %v", asked, summarizer.asked)
	}
	// The summary is to be written again, as after a failure. The run goes on
	// from the sealed part, and reads the conversation from its start for
	// the summary.
	again := func() {
		state := f.state.state.Sessions[key]
		state.ContentKey, state.Summarized = "", nil
		f.state.state.Sessions[key] = state
		summarizer.asked = nil
	}
	again()
	if result := f.run(t); result.Summaries != 1 {
		t.Fatalf("the run that writes the summary again: %+v", result)
	}
	asked, from := f.collectorRead("with-commit")
	if !reflect.DeepEqual(asked, [][]string{{"m3"}, {"m1", "m2", "m3"}}) || from[0].Events != 4 || !reflect.DeepEqual(from[1], domain.SessionPartResume{}) ||
		!reflect.DeepEqual(summarizer.asked, []string{"/work/app claude/with-commit 6 events [6]"}) {
		t.Fatalf("reads %v from %+v, summarizer %v", asked, from, summarizer.asked)
	}
	// The conversation cannot be read from its start: the report is stored,
	// the summary is not, and the reason is said.
	for name, answer := range map[string]func(ids []string) (domain.SessionPartsRead, error){
		"the reader fails": func([]string) (domain.SessionPartsRead, error) {
			return domain.SessionPartsRead{}, errors.New("synthetic read failure")
		},
		"a record is gone": func([]string) (domain.SessionPartsRead, error) {
			return domain.SessionPartsRead{Missing: []string{"m1"}}, nil
		},
	} {
		again()
		plain := *f.source
		plain.answer = nil
		f.source.answer = func(records domain.SessionRecordRanges, ids []string, from domain.SessionPartResume) (domain.SessionPartsRead, error) {
			if records.ID == "with-commit" && len(ids) == 3 {
				return answer(ids)
			}
			return plain.ReadSessionParts(t.Context(), "/work/app", records, ids, from)
		}
		result := f.run(t)
		f.source.answer = nil
		var session domain.SegmentPushSession
		for _, each := range result.Sessions {
			if each.ID == "with-commit" {
				session = each
			}
		}
		if result.Summaries != 0 || session.Content != domain.SegmentContentStored || session.SummaryReason == "" || len(summarizer.asked) != 0 ||
			(name == "the reader fails") != strings.Contains(session.SummaryReason, "synthetic read failure") || (name == "a record is gone") != strings.Contains(session.SummaryReason, "1 records changed") {
			t.Fatalf("%s: %+v summarizer %v", name, session, summarizer.asked)
		}
	}
}

// A copy that was not kept whole is built again. An automatic run builds only
// what follows the sealed parts, so they are forgotten: the next run that
// reads the conversation reads all of it and gives all of it to the copy.
func TestCollectorBuildsASessionFromItsStartAfterItsCopyWasNotKept(t *testing.T) {
	f := newCollectorFixture(t)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	f.source.sealedAt = 2
	copies := &uploadRecordsFake{}
	f.service.WithUploadRecords(copies, func() time.Time { return f.now })
	key := domain.CollectorSessionKey(domain.ProviderClaude, "without-commit")
	f.collectorGrow("w2", "w3", "w4", "w5")
	if result := f.run(t); result.Stored != 2 || len(f.state.state.Sessions[key].Parts) != 2 || result.LocalRecords == nil || result.LocalRecords.NotKept != "" {
		t.Fatalf("first run: %+v", result)
	}
	// The session goes on, and its copy cannot be kept.
	copies.err = errors.New("synthetic: the copy lacks a document")
	f.collectorGrow("w6")
	result := f.run(t)
	if asked, _ := f.collectorRead("without-commit"); !reflect.DeepEqual(asked, [][]string{{"w5", "w6"}}) {
		t.Fatalf("the run whose copy is not kept reads %v", asked)
	}
	state := f.state.state.Sessions[key]
	if result.Stored != 1 || result.LocalRecords == nil || !strings.Contains(result.LocalRecords.NotKept, "lacks a document") || len(state.Parts) != 0 || state.Sent == "" {
		t.Fatalf("the run whose copy is not kept: %+v remembered %+v", result, state)
	}
	// The same records again: the report is known, and nothing is read.
	if result := f.run(t); len(f.source.read) != 0 || result.Unchanged != 2 {
		t.Fatalf("an unchanged run after it: %+v reads=%d", result, len(f.source.read))
	}
	// It goes on again, and the copy can be kept: everything is read and
	// every document is given to the copy.
	copies.err, copies.kept = nil, nil
	f.collectorGrow("w7")
	f.run(t)
	asked, from := f.collectorRead("without-commit")
	if !reflect.DeepEqual(asked, [][]string{{"w1", "w2", "w3", "w4", "w5", "w6", "w7"}}) || !reflect.DeepEqual(from, []domain.SessionPartResume{{}}) {
		t.Fatalf("the read after the copy was not kept: %v from %+v", asked, from)
	}
	var copied uploadRecordKept
	for _, kept := range copies.kept {
		if kept.session == key {
			copied = kept
		}
	}
	if len(copied.built) != 4 || copied.events != 14 || len(f.state.state.Sessions[key].Parts) != 3 {
		t.Fatalf("the copy after it: %+v remembered %d parts", copied, len(f.state.state.Sessions[key].Parts))
	}
}
