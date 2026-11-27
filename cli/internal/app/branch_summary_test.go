package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// branchSummaryWorld is a folder on a branch, a server that holds summaries
// along branches, and what cxt remembers having given.
type branchSummaryWorld struct {
	branch   string
	newest   []string
	placeErr error
	memory   map[string]domain.AnchoredMemory
	readErr  error
	reads    []string
	given    map[string]string
	emitted  []string
	emitErr  error
}

func (w *branchSummaryWorld) BranchPlace(_ context.Context, _ string, limit int) (string, []string, error) {
	if limit != domain.AnchoredMemoryLocalCommits {
		return "", nil, errors.New("unexpected limit")
	}
	return w.branch, w.newest, w.placeErr
}

func (w *branchSummaryWorld) ReadAnchoredMemory(_ context.Context, repoID, branch string, local []string, limit int) (domain.AnchoredMemory, error) {
	w.reads = append(w.reads, repoID+" "+branch+" "+strings.Join(local, ",")+" "+string(rune('0'+limit)))
	return w.memory[branch], w.readErr
}

func (w *branchSummaryWorld) GivenBranch(_ context.Context, _ string, provider domain.ProviderKind, sessionID string) (string, error) {
	return w.given[string(provider)+"/"+sessionID], nil
}

func (w *branchSummaryWorld) SetGivenBranch(_ context.Context, _ string, provider domain.ProviderKind, sessionID, branch string) error {
	w.given[string(provider)+"/"+sessionID] = branch
	return nil
}

func (w *branchSummaryWorld) emit(text string) error {
	if w.emitErr != nil {
		return w.emitErr
	}
	w.emitted = append(w.emitted, text)
	return nil
}

func branchSummaryFixture() (*branchSummaryWorld, *BranchSummaryService, inbound.BranchSummaryInput) {
	world := &branchSummaryWorld{branch: "topic", given: map[string]string{}, memory: map[string]domain.AnchoredMemory{}}
	repo := func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: "repo-id"}, nil }
	return world, NewBranchSummaryService(repo, world, world, world), inbound.BranchSummaryInput{Cwd: "/work/app", Provider: domain.ProviderClaude, SessionID: "session-1"}
}

// A session is given the newest summary along its branch once for each
// branch it comes to, whatever woke cxt.
func TestSessionIsGivenTheSummaryOfItsBranchOnce(t *testing.T) {
	world, service, in := branchSummaryFixture()
	ctx := context.Background()
	local := strings.Repeat("c", 40)
	world.newest = []string{local}
	world.memory["topic"] = domain.AnchoredMemory{Branch: "topic", Summaries: []domain.AnchoredSummary{
		// What the session wrote itself is stepped over.
		{Provider: domain.ProviderClaude, SessionID: "session-1", Commit: local, Body: domain.AnchoredSummaryBody{Summary: "My own latest work."}},
		{Provider: domain.ProviderClaude, SessionID: "session-0", Commit: strings.Repeat("a", 40), UploaderName: "alice",
			Body: domain.AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}, OpenTasks: []string{"Document the parser."}, TasksAuthoritative: true}},
		{Provider: domain.ProviderCodex, SessionID: "session-9", Commit: strings.Repeat("b", 40), Body: domain.AnchoredSummaryBody{Summary: "Older work."}},
	}}
	// Open tasks that cxt picked out of a conversation are not the tool's own
	// list: they are not given to another session as what is left to do.
	world.memory["main"] = domain.AnchoredMemory{Branch: "main", Summaries: []domain.AnchoredSummary{
		{Provider: domain.ProviderClaude, SessionID: "session-0", Commit: strings.Repeat("d", 40),
			Body: domain.AnchoredSummaryBody{Summary: "Released the first version.", OpenTasks: []string{"Rewrite everything."}}},
	}}
	if err := service.DeliverBranchSummary(ctx, in, world.emit); err != nil {
		t.Fatal(err)
	}
	if want := []string{"repo-id topic " + local + " 5"}; !reflect.DeepEqual(world.reads, want) {
		t.Fatalf("what the server was asked: %v", world.reads)
	}
	if len(world.emitted) != 1 {
		t.Fatalf("emitted: %v", world.emitted)
	}
	text := world.emitted[0]
	for _, part := range []string{branchSummaryPrefix, `"topic"`, `"alice"`, "aaaaaaaaaaaa.", "Added the parser.", "It reads TOML.", "Document the parser.", "not a new instruction"} {
		if !strings.Contains(text, part) {
			t.Fatalf("the text lacks %q:\n%s", part, text)
		}
	}
	if strings.Contains(text, "My own latest work.") || strings.Contains(text, "Older work.") || strings.Contains(text, strings.Repeat("a", 13)) || len(text) > domain.BranchSummaryMaxBytes {
		t.Fatalf("the text says more than the newest summary of another session:\n%s", text)
	}
	if world.given["claude/session-1"] != "topic" {
		t.Fatalf("what is remembered: %v", world.given)
	}

	// The same branch again: nothing is asked, nothing is given.
	if err := service.DeliverBranchSummary(ctx, in, world.emit); err != nil || len(world.reads) != 1 || len(world.emitted) != 1 {
		t.Fatalf("given twice: %v %v %v", err, world.reads, world.emitted)
	}
	// Another session in the same folder is given it for itself.
	other := in
	other.SessionID = "session-2"
	if err := service.DeliverBranchSummary(ctx, other, world.emit); err != nil || len(world.emitted) != 2 || !strings.Contains(world.emitted[1], "My own latest work.") {
		t.Fatalf("another session: %v %v", err, world.emitted)
	}

	// The folder moves to another branch: the session is given that branch's.
	world.branch, world.newest = "main", nil
	if err := service.DeliverBranchSummary(ctx, in, world.emit); err != nil || len(world.emitted) != 3 || !strings.Contains(world.emitted[2], "Released the first version.") || !strings.Contains(world.emitted[2], `"main"`) {
		t.Fatalf("after the branch changed: %v %v", err, world.emitted)
	}
	if strings.Contains(world.emitted[2], "a session of") || strings.Contains(world.emitted[2], "Rewrite everything.") {
		t.Fatalf("a writer without a name is named:\n%s", world.emitted[2])
	}
	// And back: the first branch is given again, because it is where the
	// session now stands and not where it was last given a summary.
	world.branch = "topic"
	if err := service.DeliverBranchSummary(ctx, in, world.emit); err != nil || len(world.emitted) != 4 || world.given["claude/session-1"] != "topic" {
		t.Fatalf("back on the first branch: %v %v", err, world.given)
	}
}

// A branch without a summary is remembered as given, so the server is not
// asked at every prompt.
func TestBranchWithoutASummaryIsAskedForOnce(t *testing.T) {
	world, service, in := branchSummaryFixture()
	ctx := context.Background()
	for _, memory := range []domain.AnchoredMemory{
		{},
		// Only what the session wrote itself.
		{Summaries: []domain.AnchoredSummary{{Provider: domain.ProviderClaude, SessionID: "session-1", Commit: strings.Repeat("a", 40), Body: domain.AnchoredSummaryBody{Summary: "Mine."}}}},
		// A summary that says nothing.
		{Summaries: []domain.AnchoredSummary{{Provider: domain.ProviderCodex, SessionID: "session-3", Commit: strings.Repeat("a", 40), Body: domain.AnchoredSummaryBody{Summary: "  "}}}},
	} {
		world.memory["topic"], world.given, world.reads = memory, map[string]string{}, nil
		for range [2]struct{}{} {
			if err := service.DeliverBranchSummary(ctx, in, world.emit); err != nil {
				t.Fatal(err)
			}
		}
		if len(world.reads) != 1 || len(world.emitted) != 0 || world.given["claude/session-1"] != "topic" {
			t.Fatalf("for %+v: reads %v, emitted %v, given %v", memory, world.reads, world.emitted, world.given)
		}
	}
}

// What failed is tried again the next time: the branch is remembered only
// once the server answered and the text reached the tool.
func TestSummaryThatWasNotGivenIsTriedAgain(t *testing.T) {
	world, service, in := branchSummaryFixture()
	ctx := context.Background()
	world.memory["topic"] = domain.AnchoredMemory{Summaries: []domain.AnchoredSummary{{Provider: domain.ProviderCodex, SessionID: "session-3", Commit: strings.Repeat("a", 40), Body: domain.AnchoredSummaryBody{Summary: "Earlier work."}}}}
	offline, closed := errors.New("server unreachable"), errors.New("output closed")

	world.readErr = offline
	if err := service.DeliverBranchSummary(ctx, in, world.emit); !errors.Is(err, offline) || len(world.given) != 0 {
		t.Fatalf("an unreachable server: %v %v", err, world.given)
	}
	world.readErr, world.emitErr = nil, closed
	if err := service.DeliverBranchSummary(ctx, in, world.emit); !errors.Is(err, closed) || len(world.given) != 0 {
		t.Fatalf("a text that did not reach the tool: %v %v", err, world.given)
	}
	world.emitErr = nil
	if err := service.DeliverBranchSummary(ctx, in, world.emit); err != nil || len(world.emitted) != 1 || len(world.reads) != 3 || world.given["claude/session-1"] != "topic" {
		t.Fatalf("the next time: %v %v %v", err, world.emitted, world.given)
	}
}

// Nothing is asked where there is nothing to ask for.
func TestNoSummaryIsAskedForWithoutABranchOrAServer(t *testing.T) {
	failing := errors.New("git failed")
	for name, arrange := range map[string]func(*branchSummaryWorld, *BranchSummaryService, *inbound.BranchSummaryInput) error{
		"a folder on no branch": func(world *branchSummaryWorld, _ *BranchSummaryService, _ *inbound.BranchSummaryInput) error {
			world.branch = ""
			return nil
		},
		"a repository without a server": func(_ *branchSummaryWorld, service *BranchSummaryService, _ *inbound.BranchSummaryInput) error {
			service.WithConnection(func(context.Context, string) bool { return false })
			return nil
		},
		"a session whose ID cannot name a file": func(_ *branchSummaryWorld, _ *BranchSummaryService, in *inbound.BranchSummaryInput) error {
			in.SessionID = "../session"
			return nil
		},
		"a hook without a session": func(_ *branchSummaryWorld, _ *BranchSummaryService, in *inbound.BranchSummaryInput) error {
			in.SessionID = ""
			return nil
		},
		"Git that cannot be read": func(world *branchSummaryWorld, _ *BranchSummaryService, _ *inbound.BranchSummaryInput) error {
			world.placeErr = failing
			return failing
		},
	} {
		world, service, in := branchSummaryFixture()
		world.memory["topic"] = domain.AnchoredMemory{Summaries: []domain.AnchoredSummary{{Provider: domain.ProviderCodex, SessionID: "session-3", Commit: strings.Repeat("a", 40), Body: domain.AnchoredSummaryBody{Summary: "Earlier work."}}}}
		want := arrange(world, service, &in)
		if err := service.DeliverBranchSummary(context.Background(), in, world.emit); !errors.Is(err, want) || len(world.reads) != 0 || len(world.emitted) != 0 || len(world.given) != 0 {
			t.Fatalf("%s: %v %v %v %v", name, err, world.reads, world.emitted, world.given)
		}
	}
	var none *BranchSummaryService
	if err := none.DeliverBranchSummary(context.Background(), inbound.BranchSummaryInput{SessionID: "session-1"}, func(string) error { return errors.New("emitted") }); err != nil {
		t.Fatal(err)
	}
}

// A long summary is cut to what a tool is given at once.
func TestBranchSummaryTextIsBounded(t *testing.T) {
	summary := domain.AnchoredSummary{Provider: domain.ProviderCodex, SessionID: "session-3", Commit: strings.Repeat("a", 40),
		Body: domain.AnchoredSummaryBody{Summary: strings.Repeat("A long account of the work. ", 4000), KeyFacts: []string{"The parser reads TOML."}}}
	text := renderBranchSummary("topic", summary)
	if len(text) > domain.BranchSummaryMaxBytes || !strings.HasPrefix(text, branchSummaryPrefix) || !strings.Contains(text, "The parser reads TOML.") {
		t.Fatalf("%d bytes:\n%.400s", len(text), text)
	}
}
