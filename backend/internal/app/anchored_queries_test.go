package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The read side reads a session's conversation in pieces, and only what the
// caller may read: the uploader all of it, anyone else the open parts, with
// reading going on at the next open part.
func TestConversationIsReadInPiecesWithinWhatIsOpen(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, visitor := anchoredActor("member-a"), anchoredActor("member-b"), context.Background()
	reads, err := NewAnchoredReadService(fixture.storage, fixture.storage, fixture.storage)
	if err != nil {
		t.Fatal(err)
	}
	pushed, local := provider.chain(1, ""), anchoredTestSHA("b")
	// The commit the last segment ends at is pushed later in the test.
	later := remoteGitTestSHA(provider.next + 1)
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	texts := []string{"Plan the parser.", "The parser is committed.", "A hidden idea.", "Still hidden.", "Leftovers."}
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 5,
		Content: &domain.AnchoredContent{Doc: anchoredSearchDoc(t, fixture, texts...), Events: 5},
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3, FirstEvent: 2, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m5", Last: "m5", FirstOrdinal: 4, LastOrdinal: 4, FirstEvent: 4, Events: 1},
				Commits: []domain.AnchoredCommit{{SHA: later, Rule: domain.AnchoredLinkPrintedNumber}}},
		},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	fixture.nextPass(t)

	// whole reads every piece from an event on and returns the texts found.
	whole := func(reader context.Context, from, limit, budget int) ([]string, [][2]int) {
		t.Helper()
		var found []string
		var open [][2]int
		index, offset, pending := from, 0, ""
		for step := 0; index >= 0; step++ {
			if step > 200 {
				t.Fatal("the read does not end")
			}
			page, err := reads.ReadAnchoredSessionFragments(reader, repo, domain.ProviderClaude, "session-1", index, offset, limit, budget)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 5 {
				t.Fatalf("total: %+v", page)
			}
			open = page.Open
			for _, fragment := range page.Fragments {
				pending += fragment.JSON
				if fragment.Complete {
					var event domain.CIREvent
					if err := json.Unmarshal([]byte(pending), &event); err != nil || len(event.Blocks) != 1 {
						t.Fatalf("event %d does not read back: %q %v", fragment.Index, pending, err)
					}
					found, pending = append(found, event.Blocks[0].Text), ""
				}
			}
			index, offset = page.NextIndex, page.NextOffset
		}
		if pending != "" {
			t.Fatalf("an event was left unfinished: %q", pending)
		}
		return found, open
	}
	for _, size := range [][2]int{{50, 12 << 10}, {1, 12 << 10}, {2, 40}} {
		if got, open := whole(alice, 0, size[0], size[1]); !reflect.DeepEqual(got, texts) || !reflect.DeepEqual(open, [][2]int{{0, 5}}) {
			t.Fatalf("what the uploader reads with %v: %v %v", size, got, open)
		}
		for name, reader := range map[string]context.Context{"another member": bob, "a reader without an account": visitor} {
			if got, open := whole(reader, 0, size[0], size[1]); !reflect.DeepEqual(got, texts[:2]) || !reflect.DeepEqual(open, [][2]int{{0, 2}}) {
				t.Fatalf("what %s reads with %v: %v %v", name, size, got, open)
			}
		}
	}
	// Asking for an event that is not open, with no open part after it, reads nothing.
	if got, _ := whole(bob, 2, 50, 12<<10); len(got) != 0 {
		t.Fatalf("a part that is not open was read: %v", got)
	}
	if got, _ := whole(alice, 3, 50, 12<<10); !reflect.DeepEqual(got, texts[3:]) {
		t.Fatalf("from the fourth event: %v", got)
	}

	// The commit of the last segment is pushed: that segment opens, and the
	// reading of another member goes on there past the one that stays closed.
	if made := provider.commit(pushed); made != later {
		t.Fatalf("the later commit: %s", made)
	}
	provider.heads["main"] = later
	fixture.nextPass(t)
	got, open := whole(bob, 0, 50, 12<<10)
	if !reflect.DeepEqual(open, [][2]int{{0, 2}, {4, 5}}) || !reflect.DeepEqual(got, []string{texts[0], texts[1], texts[4]}) {
		t.Fatalf("after the later commit was pushed: %v %v", got, open)
	}
	if got, _ := whole(visitor, 2, 1, 40); !reflect.DeepEqual(got, texts[4:]) {
		t.Fatalf("from an event that is not open, reading goes on at the next open part: %v", got)
	}
	if page, err := reads.ReadAnchoredSessionFragments(bob, repo, domain.ProviderClaude, "session-1", 0, 0, 50, 12<<10); err != nil || page.NextIndex != 4 || page.NextOffset != 0 || len(page.Fragments) != 2 {
		t.Fatalf("a page stops where the open part ends: %+v %v", page, err)
	}
	for _, fragment := range []string{"hidden"} {
		for _, reader := range []context.Context{bob, visitor} {
			page, err := reads.ReadAnchoredSessionFragments(reader, repo, domain.ProviderClaude, "session-1", 2, 0, 50, 12<<10)
			raw, _ := json.Marshal(page)
			if err != nil || strings.Contains(string(raw), fragment) || strings.Contains(string(raw), string(body.Content.Doc)) {
				t.Fatalf("what is not open, or the document's hash, was answered: %s %v", raw, err)
			}
		}
	}

	// What cannot be a request, and what the caller may not read at all.
	for name, call := range map[string]func() error{
		"a negative event": func() error {
			_, err := reads.ReadAnchoredSessionFragments(alice, repo, domain.ProviderClaude, "session-1", -1, 0, 5, 100)
			return err
		},
		"no event asked for": func() error {
			_, err := reads.ReadAnchoredSessionFragments(alice, repo, domain.ProviderClaude, "session-1", 0, 0, 0, 100)
			return err
		},
		"too many events": func() error {
			_, err := reads.ReadAnchoredSessionFragments(alice, repo, domain.ProviderClaude, "session-1", 0, 0, 51, 100)
			return err
		},
		"no budget": func() error {
			_, err := reads.ReadAnchoredSessionFragments(alice, repo, domain.ProviderClaude, "session-1", 0, 0, 5, 0)
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := reads.ReadAnchoredSessionFragments(bob, repo, domain.ProviderClaude, "missing", 0, 0, 5, 100); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a session that is not stored: %v", err)
	}

	// The other reads answer as the service does.
	listed, err := reads.ListAnchoredSessions(bob, repo)
	if err != nil || len(listed) != 1 || !listed[0].Shared {
		t.Fatalf("list: %+v %v", listed, err)
	}
	if opened, err := reads.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-1"); err != nil || !opened.Body.Segments[1].Hidden {
		t.Fatalf("open: %+v %v", opened, err)
	}
	if segments, err := reads.AnchoredCommitSegments(visitor, repo, pushed); err != nil || len(segments) != 1 {
		t.Fatalf("commit: %+v %v", segments, err)
	}
	if found, err := reads.SearchAnchored(bob, repo, "parser"); err != nil || len(found.Hits) == 0 {
		t.Fatalf("search: %+v %v", found, err)
	}
	if memory, err := reads.ReadAnchoredMemory(bob, repo, "main", nil, 5); err != nil || memory.Branch != "main" {
		t.Fatalf("memory: %+v %v", memory, err)
	}
	if summaries, err := reads.ListAnchoredSummaries(bob, repo, []string{pushed}); err != nil || len(summaries) != 0 {
		t.Fatalf("summaries: %+v %v", summaries, err)
	}
	if _, err := NewAnchoredReadService(nil, fixture.storage, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("without a metadata store: %v", err)
	}
}

// The page read and the record-part read of the session routes, through the
// service and the store as they are composed in the server: what was stored is
// what is read.
func TestStoredConversationIsPagedThroughTheService(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	texts := []string{"Plan the parser.", "The parser is committed.", "\ud55c\uae00 \ub300\ud654 🌿", "Still here.", "Leftovers."}
	hash := anchoredSearchDoc(t, fixture, texts...)
	ctx := systemTestContext()
	page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, hash, 0, 20, "all")
	if err != nil {
		t.Fatal(err)
	}
	if page.Hash != hash || page.Total != len(texts) || page.Offset != 0 || page.Next != -1 || len(page.Events) != len(texts) {
		t.Fatalf("the page: %+v", page)
	}
	for index, event := range page.Events {
		if event.Index != index || event.Deferred || len(event.Event.Blocks) != 1 || event.Event.Blocks[0].Text != texts[index] || event.Event.Seq != index {
			t.Fatalf("event %d: %+v", index, event)
		}
	}
	// Two at a time, to the end.
	var walked []string
	for offset := 0; offset != -1; {
		page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, hash, offset, 2, "all")
		if err != nil || len(page.Events) == 0 || len(page.Events) > 2 {
			t.Fatalf("page at %d: %+v %v", offset, page, err)
		}
		for _, event := range page.Events {
			walked = append(walked, event.Event.Blocks[0].Text)
		}
		offset = page.Next
	}
	if !reflect.DeepEqual(walked, texts) {
		t.Fatalf("walked %q", walked)
	}
	// One record in parts of a few bytes: the parts add up to the record.
	whole, err := fixture.service.ReadDocViewFragment(ctx, fixture.repo, hash, 2, 0, 64<<10)
	if err != nil || whole.Next != -1 || whole.Index != 2 {
		t.Fatalf("the record whole: %+v %v", whole, err)
	}
	var record domain.CIREvent
	if err := json.Unmarshal([]byte(whole.JSON), &record); err != nil || record.Blocks[0].Text != texts[2] {
		t.Fatalf("the record does not read back: %q %v", whole.JSON, err)
	}
	var parts strings.Builder
	for offset := 0; offset != -1; {
		part, err := fixture.service.ReadDocViewFragment(ctx, fixture.repo, hash, 2, offset, 5)
		if err != nil || part.Total != len(whole.JSON) {
			t.Fatalf("part at %d: %+v %v", offset, part, err)
		}
		parts.WriteString(part.JSON)
		offset = part.Next
	}
	if parts.String() != whole.JSON {
		t.Fatalf("the parts do not add up: %q", parts.String())
	}
	// A document the repository does not hold.
	if _, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, domain.HashContent([]byte("no such document")), 0, 20, "all"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a document that is not there: %v", err)
	}
}
