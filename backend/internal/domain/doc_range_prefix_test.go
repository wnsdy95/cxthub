package domain

import (
	"context"
	"fmt"
	"testing"
)

type docRangePrefixFixture struct {
	total       int
	offset      int
	edit        func(int, []DocRangeEntry) []DocRangeEntry
	hashPages   []int
	readPages   []int
	readEntries int
}

func (fixture *docRangePrefixFixture) page(page int) []DocRangeEntry {
	start := page * DocRangePageEvents
	entries := make([]DocRangeEntry, min(DocRangePageEvents, fixture.total-start))
	for ordinal := range entries {
		position := start + ordinal
		body := []byte(fmt.Sprintf(`{"kind":"message","role":"user","seq":%d}`, position))
		entries[ordinal] = DocRangeEntry{
			DocEventIndex: DocEventIndex{Index: position, Offset: fixture.offset + position*128, Length: len(body), Hash: HashContent(body), Seq: position, Role: "user"},
			Kind:          EventMessage,
		}
	}
	if fixture.edit != nil {
		entries = fixture.edit(page, entries)
	}
	return entries
}

func docRangePrefixAssertPages(test testing.TB, actual []int, start, count int) {
	test.Helper()
	if len(actual) != count {
		test.Fatalf("visited pages %v; want %d pages starting at %d", actual, count, start)
	}
	for ordinal, page := range actual {
		if page != start+ordinal {
			test.Fatalf("visited pages %v; want consecutive pages starting at %d", actual, start)
		}
	}
}

func TestCompareDocRangePrefixRejectsInvalidInputsBeforeCallbacks(test *testing.T) {
	test.Parallel()
	total := 2*DocRangePageEvents + 7
	for _, scenario := range []struct {
		name        string
		leftTotal   int
		rightTotal  int
		start       int
		budget      int
		nilCallback string
	}{
		{"negative-left-total", -1, total, 0, 1, ""},
		{"negative-right-total", total, -1, 0, 1, ""},
		{"oversized-left-total", MaxConversationDocumentBytes + 1, total, 0, 1, ""},
		{"oversized-right-total", total, MaxConversationDocumentBytes + 1, 0, 1, ""},
		{"negative-start", total, total, -1, 1, ""},
		{"past-left-total", 7, total, DocRangePageEvents, 1, ""},
		{"past-right-total", total, 7, DocRangePageEvents, 1, ""},
		{"unaligned-first-page", total, total, 1, 1, ""},
		{"unaligned-later-page", total, total, DocRangePageEvents + 1, 1, ""},
		{"past-empty", 0, 0, 1, 1, ""},
		{"zero-budget", total, total, 0, 0, ""},
		{"negative-budget", total, total, 0, -1, ""},
		{"empty-zero-budget", 0, 0, 0, 0, ""},
		{"completed-zero-budget", total, total, total, 0, ""},
		{"nil-left-hash", total, total, 0, 1, "left-hash"},
		{"nil-right-hash", total, total, 0, 1, "right-hash"},
		{"nil-left-read", total, total, 0, 1, "left-read"},
		{"nil-right-read", total, total, 0, 1, "right-read"},
		{"empty-nil-left-hash", 0, 0, 0, 1, "left-hash"},
		{"empty-nil-right-hash", 0, 0, 0, 1, "right-hash"},
		{"empty-nil-left-read", 0, 0, 0, 1, "left-read"},
		{"empty-nil-right-read", 0, 0, 0, 1, "right-read"},
		{"zero-sources", 0, 0, 0, 1, "all"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			left := &docRangePrefixFixture{total: scenario.leftTotal}
			right := &docRangePrefixFixture{total: scenario.rightTotal}
			switch scenario.nilCallback {
			case "left-hash":
			case "right-hash":
			case "left-read":
			case "right-read":
			case "all":
			}
			for _, fixture := range []*docRangePrefixFixture{left, right} {
				docRangePrefixAssertPages(test, fixture.hashPages, 0, 0)
				docRangePrefixAssertPages(test, fixture.readPages, 0, 0)
			}
		})
	}
}

type docRangePrefixContext struct {
	context.Context
	checks   int
	cancelAt int
	cancel   context.CancelFunc
}

func (ctx *docRangePrefixContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
