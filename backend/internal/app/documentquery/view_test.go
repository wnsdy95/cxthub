package documentquery_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// viewResources counts what a paged read admits and gives back.
type viewResources struct {
	interactive, bulk int
	released          int
	refuse            error
}

func (resources *viewResources) acquire(ctx context.Context, count *int) (context.Context, func(), error) {
	if resources.refuse != nil {
		return ctx, nil, resources.refuse
	}
	*count++
	return ctx, func() { resources.released++ }, nil
}

func (resources *viewResources) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	return resources.acquire(ctx, &resources.interactive)
}

func (resources *viewResources) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return resources.acquire(ctx, &resources.bulk)
}

// viewQueries is the reader the session routes use: the document is read
// through the legacy store, and a paged read is admitted by resources.
func (fixture *eventPrefixFixture) viewQueries(resources documentquery.Resources) *documentquery.Queries {
	fixture.test.Helper()
	queries, err := documentquery.New(documentquery.Config{Profile: documentquery.LegacyCompatibility,
		Scope: fixture.scope, Legacy: fixture.legacy, Resources: resources, AtomicRead: true})
	if err != nil {
		fixture.test.Fatal(err)
	}
	return queries
}

// viewMessage is the canonical JSON of a message event with a text of n bytes.
func viewMessage(role string, seq, n int) string {
	return fmt.Sprintf(`{"blocks":[{"text":"%s","type":"text"}],"kind":"message","role":"%s","seq":%d}`, strings.Repeat("a", n), role, seq)
}

// shape is what a page says about one event: its position, whether only its
// description was sent, and its kind.
func viewShape(page domain.DocViewPage) string {
	parts := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		mark := "full"
		if event.Deferred {
			mark = "later"
		}
		parts = append(parts, fmt.Sprintf("%d:%s:%s", event.Index, event.Event.Kind, mark))
	}
	return strings.Join(parts, " ")
}

func TestViewEventsShowsEachViewWhatItIsFor(test *testing.T) {
	fixture := eventPrefix(test)
	hash := fixture.document.document.Hash
	for _, scenario := range []struct {
		view  string
		shape string
	}{
		// Everything, undeferred: the reader of one record in full.
		{"raw", "0:message:full 1:message:full 2:tool_call:full 3:reasoning:full 4:tool_result:full 5:message:full 6:turn:full"},
		// Everything is listed; tool traffic and reasoning are described, not sent.
		{"all", "0:message:full 1:message:full 2:tool_call:later 3:reasoning:later 4:tool_result:later 5:message:full 6:turn:full"},
		// The messages of both sides.
		{"chat", "0:message:full 1:message:full 2:tool_call:later 3:reasoning:later 4:tool_result:later 5:message:full 6:turn:later"},
		// What the person wrote.
		{"prompts", "0:message:full 1:message:later 2:tool_call:later 3:reasoning:later 4:tool_result:later 5:message:later 6:turn:later"},
	} {
		test.Run(scenario.view, func(test *testing.T) {
			resources := &viewResources{}
			page, err := fixture.viewQueries(resources).ReadDocViewEvents(context.Background(), fixture.repo, hash, 0, 20, scenario.view)
			if err != nil {
				test.Fatal(err)
			}
			if got := viewShape(page); got != scenario.shape {
				test.Fatalf("view %s\n got: %s\nwant: %s", scenario.view, got, scenario.shape)
			}
			if page.Hash != hash || page.Total != 7 || page.Offset != 0 || page.Next != -1 || !reflect.DeepEqual(page.Envelope, fixture.document.document.CIR.Envelope) {
				test.Fatalf("the page does not describe its document: %+v", page)
			}
			for _, event := range page.Events {
				original := fixture.document.events[event.Index]
				if event.Bytes != len(fixture.document.json[event.Index]) {
					test.Fatalf("event %d: %d bytes, the record has %d", event.Index, event.Bytes, len(fixture.document.json[event.Index]))
				}
				if !event.Deferred {
					if !reflect.DeepEqual(event.Event, original) {
						test.Fatalf("event %d was not sent as it is stored:\n got: %+v\nwant: %+v", event.Index, event.Event, original)
					}
					continue
				}
				// A described event carries what a list needs and none of its content.
				described := domain.CIREvent{Kind: original.Kind, Role: original.Role, Seq: original.Seq, ToolName: original.ToolName, CompactSummary: original.CompactSummary}
				if !reflect.DeepEqual(event.Event, described) {
					test.Fatalf("event %d was described with more or less than its kind, role, sequence and tool: %+v", event.Index, event.Event)
				}
			}
			if resources.interactive != 1 || resources.bulk != 1 || resources.released != 2 {
				test.Fatalf("a page was not admitted once and given back: %+v", resources)
			}
		})
	}
}

func TestViewEventsPagesByWhatIsShown(test *testing.T) {
	fixture := eventPrefix(test)
	hash := fixture.document.document.Hash
	read := func(offset, limit int, view string) domain.DocViewPage {
		test.Helper()
		page, err := fixture.viewQueries(&viewResources{}).ReadDocViewEvents(context.Background(), fixture.repo, hash, offset, limit, view)
		if err != nil {
			test.Fatal(err)
		}
		return page
	}
	// The limit counts the events a view shows, not the ones it only describes.
	first := read(0, 2, "chat")
	if got := viewShape(first); got != "0:message:full 1:message:full" || first.Next != 2 || first.Offset != 0 {
		test.Fatalf("first page of two messages: %s next=%d", got, first.Next)
	}
	second := read(first.Next, 2, "chat")
	if got := viewShape(second); got != "2:tool_call:later 3:reasoning:later 4:tool_result:later 5:message:full 6:turn:later" || second.Next != -1 || second.Offset != 2 {
		test.Fatalf("second page: %s next=%d", got, second.Next)
	}
	// Minus one means the beginning, and no limit means twenty.
	if page := read(-1, 0, "all"); len(page.Events) != 7 || page.Offset != 0 || page.Next != -1 {
		test.Fatalf("from the beginning without a limit: %s next=%d", viewShape(page), page.Next)
	}
	// One event at a time reaches every event exactly once.
	seen := []int{}
	for offset := 0; offset != -1; {
		page := read(offset, 1, "all")
		if len(page.Events) != 1 || page.Events[0].Index != offset {
			test.Fatalf("page at %d: %s", offset, viewShape(page))
		}
		seen = append(seen, page.Events[0].Index)
		offset = page.Next
	}
	if !reflect.DeepEqual(seen, []int{0, 1, 2, 3, 4, 5, 6}) {
		test.Fatalf("walking one by one saw %v", seen)
	}
	// The end of the document is a place; beyond it is not.
	if page := read(7, 5, "all"); page.Events == nil || len(page.Events) != 0 || page.Offset != 7 || page.Next != -1 || page.Total != 7 {
		test.Fatalf("at the end: %+v", page)
	}
	if _, err := fixture.viewQueries(&viewResources{}).ReadDocViewEvents(context.Background(), fixture.repo, hash, 8, 5, "all"); !errors.Is(err, domain.ErrValidation) {
		test.Fatalf("beyond the end: %v", err)
	}
}

func TestViewEventsRefusesWhatItCannotMeanBeforeAnyWork(test *testing.T) {
	fixture := eventPrefix(test)
	hash := fixture.document.document.Hash
	for name, call := range map[string]struct {
		offset, limit int
		view          string
	}{
		"an offset below minus one":  {-2, 5, "all"},
		"a negative limit":           {0, -1, "all"},
		"a limit above a hundred":    {0, 101, "all"},
		"a view that does not exist": {0, 5, "summary"},
		"no view":                    {0, 5, ""},
	} {
		resources := &viewResources{}
		fixture.scope.calls = nil
		_, err := fixture.viewQueries(resources).ReadDocViewEvents(context.Background(), fixture.repo, hash, call.offset, call.limit, call.view)
		if !errors.Is(err, domain.ErrValidation) || resources.interactive != 0 || resources.bulk != 0 || len(fixture.scope.calls) != 0 {
			test.Errorf("%s: err=%v admitted=%+v scope=%v", name, err, resources, fixture.scope.stages())
		}
	}
}

func TestViewEventsGivesBackWhatItWasAdmittedWithWhenItFails(test *testing.T) {
	fixture := eventPrefix(test)
	hash := fixture.document.document.Hash
	// A document that is not there.
	resources := &viewResources{}
	missing := domain.HashContent([]byte("no such document"))
	if _, err := fixture.viewQueries(resources).ReadDocViewEvents(context.Background(), fixture.repo, missing, 0, 5, "all"); !errors.Is(err, domain.ErrNotFound) || resources.released != resources.interactive+resources.bulk || resources.interactive != 1 {
		test.Fatalf("missing document: err=%v %+v", err, resources)
	}
	// A caller that is gone: nothing is read.
	resources = &viewResources{}
	fixture.scope.calls = nil
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.viewQueries(resources).ReadDocViewEvents(cancelled, fixture.repo, hash, 0, 5, "all"); !errors.Is(err, context.Canceled) || resources.released != resources.interactive+resources.bulk {
		test.Fatalf("cancelled caller: err=%v %+v", err, resources)
	}
	for _, stage := range fixture.scope.stages() {
		if stage == "doc" || stage == "chunk" {
			test.Fatalf("a cancelled caller still read the document: %v", fixture.scope.stages())
		}
	}
	// Admission refused: the refusal is the answer.
	busy := errors.New("synthetic: too many readers")
	if _, err := fixture.viewQueries(&viewResources{refuse: busy}).ReadDocViewEvents(context.Background(), fixture.repo, hash, 0, 5, "all"); !errors.Is(err, busy) {
		test.Fatalf("refused admission: %v", err)
	}
	// A reader that was given nothing to admit with does not page at all.
	if _, err := fixture.viewQueries(nil).ReadDocViewEvents(context.Background(), fixture.repo, hash, 0, 5, "all"); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		test.Fatalf("no admission configured: %v", err)
	}
}

func TestViewEventsKeepsAPageInsideItsSize(test *testing.T) {
	// Four messages of 50 KiB, one of 200 KiB, one small one. A page carries
	// at most 128 KiB of events, and never an event larger than that.
	big, huge := 50<<10, 200<<10
	fixture := eventPrefixWithJSON(test, []string{
		viewMessage("user", 10, big), viewMessage("assistant", 20, big), viewMessage("user", 30, big), viewMessage("assistant", 40, big),
		viewMessage("user", 50, huge), viewMessage("assistant", 60, 12),
	})
	hash := fixture.document.document.Hash
	for _, chunked := range []bool{false, true} {
		fixture.legacy.chunked = chunked
		read := func(offset int, view string) domain.DocViewPage {
			test.Helper()
			page, err := fixture.viewQueries(&viewResources{}).ReadDocViewEvents(context.Background(), fixture.repo, hash, offset, 20, view)
			if err != nil {
				test.Fatal(err)
			}
			return page
		}
		for _, view := range []string{"raw", "chat"} {
			first := read(0, view)
			if got := viewShape(first); got != "0:message:full 1:message:full" || first.Next != 2 {
				test.Fatalf("chunked=%t %s: the first page holds %s, next=%d", chunked, view, got, first.Next)
			}
			second := read(first.Next, view)
			// The 200 KiB message is described even to the reader of raw records;
			// its parts are read through the fragment read.
			if got := viewShape(second); got != "2:message:full 3:message:full 4:message:later 5:message:full" || second.Next != -1 {
				test.Fatalf("chunked=%t %s: the second page holds %s, next=%d", chunked, view, got, second.Next)
			}
			if second.Events[2].Bytes != len(fixture.document.json[4]) || len(second.Events[2].Event.Blocks) != 0 {
				test.Fatalf("the large message was sent with content: %d bytes, %d blocks", second.Events[2].Bytes, len(second.Events[2].Event.Blocks))
			}
		}
	}
}

func TestViewEventsLooksAtNoMoreThan512EventsForOnePage(test *testing.T) {
	// 600 answers and then a prompt: the view of prompts shows only the last.
	events := make([]string, 0, 601)
	for position := 0; position < 600; position++ {
		events = append(events, viewMessage("assistant", position+1, 8))
	}
	events = append(events, viewMessage("user", 700, 8))
	fixture := eventPrefixWithJSON(test, events)
	hash := fixture.document.document.Hash
	first, err := fixture.viewQueries(&viewResources{}).ReadDocViewEvents(context.Background(), fixture.repo, hash, 0, 5, "prompts")
	if err != nil || len(first.Events) != 512 || first.Next != 512 || first.Total != 601 {
		test.Fatalf("first page: events=%d next=%d total=%d err=%v", len(first.Events), first.Next, first.Total, err)
	}
	for _, event := range first.Events {
		if !event.Deferred {
			test.Fatalf("an answer was sent in the view of prompts: %+v", event)
		}
	}
	second, err := fixture.viewQueries(&viewResources{}).ReadDocViewEvents(context.Background(), fixture.repo, hash, first.Next, 5, "prompts")
	if err != nil || len(second.Events) != 89 || second.Next != -1 || second.Events[88].Deferred || second.Events[88].Event.Role != domain.RoleUser {
		test.Fatalf("second page: events=%d next=%d err=%v", len(second.Events), second.Next, err)
	}
}

func TestViewFragmentReadsARecordInParts(test *testing.T) {
	fixture := eventPrefix(test)
	hash := fixture.document.document.Hash
	queries := func() *documentquery.Queries { return fixture.viewQueries(&viewResources{}) }
	// Any budget, read to the end, gives the record byte for byte, and no
	// part ends inside a character.
	for index, want := range fixture.document.json {
		for _, budget := range []int{4, 5, 7, 16, 64, 64 << 10} {
			var got strings.Builder
			for offset := 0; offset != -1; {
				part, err := queries().ReadDocViewFragment(context.Background(), fixture.repo, hash, index, offset, budget)
				if err != nil {
					test.Fatalf("event %d budget %d offset %d: %v", index, budget, offset, err)
				}
				if part.Index != index || part.Offset != offset || part.Total != len(want) || len(part.JSON) == 0 || len(part.JSON) > budget || !strings.HasPrefix(want[offset:], part.JSON) {
					test.Fatalf("event %d budget %d offset %d: %+v", index, budget, offset, part)
				}
				if part.Next != -1 && part.Next != offset+len(part.JSON) {
					test.Fatalf("event %d budget %d: the next part does not begin where this one ended: %+v", index, budget, part)
				}
				got.WriteString(part.JSON)
				offset = part.Next
			}
			if got.String() != want {
				test.Fatalf("event %d budget %d: the parts do not add up to the record", index, budget)
			}
		}
	}
	// A budget that ends inside a character gives the characters before it.
	emoji := strings.Index(fixture.document.json[1], "🌿")
	part, err := queries().ReadDocViewFragment(context.Background(), fixture.repo, hash, 1, emoji-1, 4)
	if err != nil || part.JSON != " " || part.Next != emoji {
		test.Fatalf("a budget ending inside the emoji: %+v %v", part, err)
	}
	// A part cannot begin inside a character.
	if _, err := queries().ReadDocViewFragment(context.Background(), fixture.repo, hash, 1, emoji+1, 16); !errors.Is(err, domain.ErrValidation) {
		test.Fatalf("a part beginning inside the emoji: %v", err)
	}
	for name, call := range map[string][3]int{
		"a negative event":            {-1, 0, 16},
		"an event beyond the last":    {7, 0, 16},
		"a negative offset":           {0, -1, 16},
		"an offset at the record end": {0, len(fixture.document.json[0]), 16},
		"a budget below a character":  {0, 0, 3},
		"a budget above 64 KiB":       {0, 0, 64<<10 + 1},
	} {
		if _, err := queries().ReadDocViewFragment(context.Background(), fixture.repo, hash, call[0], call[1], call[2]); !errors.Is(err, domain.ErrValidation) {
			test.Errorf("%s: %v", name, err)
		}
	}
	// A record that is not the one its index names is refused, not shown.
	resources := &viewResources{}
	for key, body := range fixture.legacy.documents {
		body.CIR.Events[len(body.CIR.Events)-1].Blocks[0].Text = "another prompt"
		fixture.legacy.documents[key] = body
	}
	if _, err := fixture.viewQueries(resources).ReadDocViewFragment(context.Background(), fixture.repo, hash, 0, 0, 64<<10); err == nil {
		test.Fatal("a document that no longer matches its hash was read")
	}
	if resources.released != resources.interactive+resources.bulk {
		test.Fatalf("a failed read kept what it was admitted with: %+v", resources)
	}
}

func TestViewReadsAskAgainWhetherTheCallerMayRead(test *testing.T) {
	fixture := eventPrefix(test)
	hash := fixture.document.document.Hash
	// Each paged read asks once, before it opens the document.
	resources := &viewResources{}
	if _, err := fixture.viewQueries(resources).ReadDocViewEvents(context.Background(), fixture.repo, hash, 0, 5, "all"); err != nil || fixture.scope.authorized != 1 {
		test.Fatalf("a page asked %d times: %v", fixture.scope.authorized, err)
	}
	if _, err := fixture.viewQueries(resources).ReadDocViewFragment(context.Background(), fixture.repo, hash, 0, 0, 64); err != nil || fixture.scope.authorized != 2 {
		test.Fatalf("a part of a record asked %d times in all: %v", fixture.scope.authorized, err)
	}
	// A caller who may no longer read gets the refusal and nothing else.
	refusal := errors.New("synthetic: the caller left the repository")
	fixture.scope.refusal, fixture.scope.calls = refusal, nil
	resources = &viewResources{}
	if page, err := fixture.viewQueries(resources).ReadDocViewEvents(context.Background(), fixture.repo, hash, 0, 5, "all"); !errors.Is(err, refusal) || len(page.Events) != 0 {
		test.Fatalf("a refused page: %+v %v", page, err)
	}
	if part, err := fixture.viewQueries(resources).ReadDocViewFragment(context.Background(), fixture.repo, hash, 0, 0, 64); !errors.Is(err, refusal) || part.JSON != "" {
		test.Fatalf("a refused part: %+v %v", part, err)
	}
	if len(fixture.scope.calls) != 0 || resources.released != resources.interactive+resources.bulk || resources.interactive != 2 {
		test.Fatalf("a refused caller reached the document or kept an admission: stages=%v %+v", fixture.scope.stages(), resources)
	}
}

func TestViewFragmentRefusesBytesThatAreNotTheRecord(test *testing.T) {
	// Two documents of the same shape whose first record differs. The reader
	// knows the first by its index; the chunks it is then given are the
	// second's, each of them intact in itself.
	build := func(first string) *eventPrefixFixture {
		events := []string{fmt.Sprintf(`{"blocks":[{"text":"%s","type":"text"}],"kind":"message","role":"user","seq":1}`, strings.Repeat(first, 1000))}
		for position := 1; position < 700; position++ {
			events = append(events, viewMessage("assistant", position+1, 1000))
		}
		return eventPrefixWithJSON(test, events)
	}
	fixture, other := build("a"), build("b")
	hash := fixture.document.document.Hash
	if len(fixture.document.plan.Bodies) < 2 || len(fixture.document.json[0]) != len(other.document.json[0]) {
		test.Fatal("the fixture is not stored in chunks, or its two documents differ in shape")
	}
	fixture.legacy.chunked = true
	whole, err := fixture.viewQueries(&viewResources{}).ReadDocViewFragment(context.Background(), fixture.repo, hash, 0, 0, 64<<10)
	if err != nil || whole.JSON != fixture.document.json[0] || whole.Next != -1 {
		test.Fatalf("the record from its own chunks: %v", err)
	}
	fixture.legacy.manifests[hash] = other.document.plan.Manifest
	for key, body := range other.document.plan.Bodies {
		fixture.legacy.chunks[key] = body
	}
	resources := &viewResources{}
	part, err := fixture.viewQueries(resources).ReadDocViewFragment(context.Background(), fixture.repo, hash, 0, 0, 64<<10)
	if !errors.Is(err, domain.ErrIntegrity) || part.JSON != "" {
		test.Fatalf("bytes of another record were shown as this one: %d bytes, %v", len(part.JSON), err)
	}
	if resources.released != resources.interactive+resources.bulk {
		test.Fatalf("a refused read kept what it was admitted with: %+v", resources)
	}
}
