package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type segmentSummarizerFake struct {
	asked []string
	err   error
	// silent makes every digest say nothing.
	silent bool
}

func (f *segmentSummarizerFake) Summarize(_ context.Context, folder string, provider domain.ProviderKind, session string, conversation domain.CIRDocument, ends []int) ([]domain.MemoryDigest, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s %s/%s %d events %v", folder, provider, session, len(conversation.Events), ends))
	if f.err != nil {
		return nil, f.err
	}
	digests := make([]domain.MemoryDigest, len(ends))
	for index, end := range ends {
		if !f.silent {
			digests[index] = domain.MemoryDigest{Summary: fmt.Sprintf("%s up to %d", session, end), KeyFacts: []string{"a fact", " "}}
		}
	}
	return digests, nil
}

type segmentSummaryRemoteFake struct {
	stored []string
	// reports is how many reports the server held when each summary came.
	reports []int
	held    func() int
	err     error
}

func (f *segmentSummaryRemoteFake) PutAnchoredSummary(_ context.Context, _ string, provider domain.ProviderKind, session, commit string, body domain.AnchoredSummaryBody) error {
	if f.err != nil {
		return f.err
	}
	f.stored = append(f.stored, fmt.Sprintf("%s/%s@%s: %s %v", provider, session, commit[:7], body.Summary, body.KeyFacts))
	if f.held != nil {
		f.reports = append(f.reports, f.held())
	}
	return nil
}

func TestSegmentUploadWritesASummaryAtEachCommitAfterTheReport(t *testing.T) {
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{held: func() int { return len(remote.uploads) }}
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source}).
		WithSummaries(summaries, summarizer)
	result, err := service.PushSegments(context.Background(), "/work/app")
	if err != nil {
		t.Fatal(err)
	}
	// Only the session that made a commit is written down, up to the end of
	// the segment that ends at it, for the folder the repository is in when
	// the tool recorded none.
	if !reflect.DeepEqual(summarizer.asked, []string{"/work/app claude/with-commit 6 events [6]"}) {
		t.Fatalf("asked of the summarizer: %v", summarizer.asked)
	}
	if !reflect.DeepEqual(summaries.stored, []string{"claude/with-commit@aaaaaaa: with-commit up to 6 [a fact]"}) || !reflect.DeepEqual(summaries.reports, []int{1}) {
		t.Fatalf("summaries stored: %v after %v reports", summaries.stored, summaries.reports)
	}
	if result.Summaries != 1 || result.Sessions[0].Summaries != 1 || result.Sessions[0].SummaryReason != "" {
		t.Fatalf("result: %+v", result)
	}
	for _, session := range result.Sessions[1:] {
		if session.Summaries != 0 || session.SummaryReason != "" {
			t.Fatalf("a session without a commit: %+v", session)
		}
	}
}

func TestSegmentUploadKeepsAReportWhoseSummaryCannotBeWritten(t *testing.T) {
	for name, broken := range map[string]func(*segmentSummarizerFake, *segmentSummaryRemoteFake){
		"the conversation cannot be written down": func(summarizer *segmentSummarizerFake, _ *segmentSummaryRemoteFake) {
			summarizer.err = errors.New("no distiller")
		},
		"the server refuses the summary": func(_ *segmentSummarizerFake, summaries *segmentSummaryRemoteFake) {
			summaries.err = segmentUploadStatus(422)
		},
	} {
		remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
		summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{}
		broken(summarizer, summaries)
		service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source}).
			WithSummaries(summaries, summarizer)
		result, err := service.PushSegments(context.Background(), "/work/app")
		if err != nil || result.Stored != 2 || result.Summaries != 0 || result.Sessions[0].SummaryReason == "" || result.Sessions[0].State != domain.SegmentPushStored {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
	}
	// A conversation that gives nothing to write down stores nothing and is no failure.
	remote, source, content := &segmentRemoteFake{}, &segmentContentSourceFake{}, &segmentContentRemoteFake{}
	summarizer, summaries := &segmentSummarizerFake{silent: true}, &segmentSummaryRemoteFake{}
	service := segmentUploadFixture(remote).WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{domain.ProviderClaude: source}).
		WithSummaries(summaries, summarizer)
	if result, err := service.PushSegments(context.Background(), "/work/app"); err != nil || len(summaries.stored) != 0 || result.Sessions[0].SummaryReason != "" {
		t.Fatalf("nothing to write down: %+v %v", result, err)
	}
}

func TestCollectorWritesASummaryOnceForEachCommit(t *testing.T) {
	f := newCollectorFixture(t)
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{}
	f.service.WithSummaries(summaries, summarizer)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	sha := strings.Repeat("a", 40)
	first := f.run(t)
	if first.Summaries != 1 || len(summaries.stored) != 1 || !reflect.DeepEqual(f.state.state.Sessions[domain.CollectorSessionKey(domain.ProviderClaude, "with-commit")].Summarized, []string{sha}) {
		t.Fatalf("first run: %+v stored=%v", first, summaries.stored)
	}
	// Nothing changed: nothing is written down again.
	if second := f.run(t); second.Summaries != 0 || len(summarizer.asked) != 1 || len(summaries.stored) != 1 {
		t.Fatalf("second run: %+v asked=%v", second, summarizer.asked)
	}
	// The session goes on after its commit: its conversation is read again,
	// but the commit already has its summary.
	f.claude.batch.Sessions[0].Items = append(f.claude.batch.Sessions[0].Items, domain.RecordedItem{ID: "m4", Turn: "t2", AtMs: collectorDay(30), Folder: "/work/app", Branch: "main"})
	if third := f.run(t); third.Stored != 1 || third.Summaries != 0 || len(summarizer.asked) != 1 || len(f.source.read) != 1 {
		t.Fatalf("after the session went on: %+v asked=%v reads=%d", third, summarizer.asked, len(f.source.read))
	}
}

func TestCollectorTriesASummaryAgainThatWasNotStored(t *testing.T) {
	f := newCollectorFixture(t)
	summarizer, summaries := &segmentSummarizerFake{}, &segmentSummaryRemoteFake{err: segmentUploadStatus(503)}
	f.service.WithSummaries(summaries, summarizer)
	f.began(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	key := domain.CollectorSessionKey(domain.ProviderClaude, "with-commit")
	first := f.run(t)
	if first.Stored != 2 || first.Summaries != 0 || first.Sessions[0].SummaryReason == "" || len(f.state.state.Sessions[key].Summarized) != 0 {
		t.Fatalf("first run: %+v remembered=%+v", first, f.state.state.Sessions[key])
	}
	// The report is on the server and is not sent again; the conversation is
	// read again for the summary that is still owed.
	summaries.err = nil
	second := f.run(t)
	if second.Stored != 0 || second.Summaries != 1 || len(f.remote.uploads) != 0 || len(summaries.stored) != 1 || len(f.state.state.Sessions[key].Summarized) != 1 {
		t.Fatalf("second run: %+v uploads=%d stored=%v", second, len(f.remote.uploads), summaries.stored)
	}
	if third := f.run(t); third.Summaries != 0 || len(summarizer.asked) != 2 || len(f.source.read) != 0 {
		t.Fatalf("third run: %+v asked=%v", third, summarizer.asked)
	}
}
