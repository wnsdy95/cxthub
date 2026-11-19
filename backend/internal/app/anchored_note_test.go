package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestAnchoredNoteBelongsToItsAuthorAndIsMasked(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	before, err := service.GetRepositoryView(systemTestContext(), repo)
	if err != nil {
		t.Fatal(err)
	}
	commit := domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: anchoredTestSHA("a")}
	token := "ghp_" + strings.Repeat("a1B2", 9)
	note, err := service.CreateAnchoredNote(alice, repo, commit, "  Hand-applied hotfix.\r\nToken used: "+token+"  ")
	if err != nil || !domain.ValidAnchoredNoteID(note.ID) || note.Author != "member-a" || note.Target != commit ||
		note.Body != "Hand-applied hotfix.\nToken used: «redacted:github-token»" || note.CreatedAt.IsZero() || !note.UpdatedAt.Equal(note.CreatedAt) {
		t.Fatalf("created note: %+v %v", note, err)
	}
	worktree, err := service.CreateAnchoredNote(alice, repo, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteWorktree, Worktree: "app-fix", Machine: "laptop"}, "scratch folder")
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := service.ListAnchoredNotes(alice, repo); err != nil || len(listed) != 2 || listed[0].ID != note.ID || listed[1].ID != worktree.ID {
		t.Fatalf("own notes, oldest first: %+v %v", listed, err)
	}
	// To another member the note does not exist: not listed, not changed, not removed.
	if listed, err := service.ListAnchoredNotes(bob, repo); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("another member's list: %#v %v", listed, err)
	}
	if _, err := service.UpdateAnchoredNote(bob, repo, note.ID, "taken over"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member rewrote the note: %v", err)
	}
	if err := service.DeleteAnchoredNote(bob, repo, note.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member removed the note: %v", err)
	}

	// Rewriting keeps the author, the target and when it was written.
	rewritten, err := service.UpdateAnchoredNote(alice, repo, note.ID, "Hand-applied hotfix, reverted later.")
	if err != nil || rewritten.Body != "Hand-applied hotfix, reverted later." || rewritten.Author != "member-a" || rewritten.Target != commit ||
		!rewritten.CreatedAt.Equal(note.CreatedAt) || rewritten.UpdatedAt.Before(note.UpdatedAt) {
		t.Fatalf("rewritten note: %+v %v", rewritten, err)
	}
	// The same text again changes nothing.
	again, err := service.UpdateAnchoredNote(alice, repo, note.ID, " Hand-applied hotfix, reverted later. ")
	if err != nil || !reflect.DeepEqual(again, rewritten) {
		t.Fatalf("the same text again: %+v %v", again, err)
	}
	if err := service.DeleteAnchoredNote(alice, repo, worktree.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteAnchoredNote(alice, repo, worktree.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removing a note twice: %v", err)
	}
	if listed, err := service.ListAnchoredNotes(alice, repo); err != nil || len(listed) != 1 || listed[0].Body != rewritten.Body {
		t.Fatalf("after removing: %+v %v", listed, err)
	}
	// Notes are read only by their author, so nothing an open page shows changed.
	after, err := service.GetRepositoryView(systemTestContext(), repo)
	if err != nil || !reflect.DeepEqual(after.Revision, before.Revision) {
		t.Fatalf("a note moved the repository revision: %+v -> %+v %v", before.Revision, after.Revision, err)
	}
}

func TestAnchoredNoteNamesABranchLifetimeTheLedgerHolds(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice := anchoredActor("member-a")
	held, joined := branchLedgerTestID("a"), branchLedgerTestID("b")
	if _, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "topic", LocalID: held}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterBranchIDs(anchoredActor("member-b"), repo, []domain.BranchRegistration{{Name: "topic", LocalID: joined}}); err != nil {
		t.Fatal(err)
	}
	branch := func(name, ledger string) domain.AnchoredNoteTarget {
		return domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteBranch, Branch: name, LedgerID: ledger}
	}
	if note, err := service.CreateAnchoredNote(alice, repo, branch("topic", held), "why the branch exists"); err != nil || note.Target.LedgerID != held {
		t.Fatalf("a lifetime the ledger holds: %+v %v", note, err)
	}
	// A name the remote never held has no lifetime to name.
	if _, err := service.CreateAnchoredNote(alice, repo, branch("local-only", ""), "never pushed"); err != nil {
		t.Fatalf("a branch without a lifetime: %v", err)
	}
	for name, target := range map[string]domain.AnchoredNoteTarget{
		"an ID the ledger does not hold":      branch("topic", branchLedgerTestID("c")),
		"a lifetime under another name":       branch("other", held),
		"an ID joined to a lifetime, not its": branch("topic", joined),
	} {
		if _, err := service.CreateAnchoredNote(alice, repo, target, "text"); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

func TestAnchoredNoteRefusesWhatIsNotAMembersNote(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	commit := domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: anchoredTestSHA("a")}
	if _, err := service.CreateAnchoredNote(anchoredActor("viewer"), repo, commit, "text"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer: %v", err)
	}
	if _, err := service.CreateAnchoredNote(anchoredActor("stranger"), repo, commit, "text"); !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an account outside the repository: %v", err)
	}
	if _, err := service.CreateAnchoredNote(systemTestContext(), repo, commit, "text"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the server itself: %v", err)
	}
	alice := anchoredActor("member-a")
	for name, call := range map[string]func() error{
		"a target of no kind": func() error {
			_, err := service.CreateAnchoredNote(alice, repo, domain.AnchoredNoteTarget{}, "text")
			return err
		},
		"no text": func() error { _, err := service.CreateAnchoredNote(alice, repo, commit, " \n "); return err },
		"text over the limit": func() error {
			_, err := service.CreateAnchoredNote(alice, repo, commit, strings.Repeat("x", domain.AnchoredNoteBodyLimit+1))
			return err
		},
		"an identifier in another form": func() error { _, err := service.UpdateAnchoredNote(alice, repo, "first", "text"); return err },
		"removing by another form":      func() error { return service.DeleteAnchoredNote(alice, repo, "first") },
	} {
		if err := call(); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	if _, err := service.UpdateAnchoredNote(alice, repo, domain.NewAnchoredNoteID(), "text"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("rewriting a note that is not there: %v", err)
	}
	if listed, err := service.ListAnchoredNotes(alice, repo); err != nil || len(listed) != 0 {
		t.Fatalf("a refused note was stored: %+v %v", listed, err)
	}
}

func TestReportedWorktreesAreKeptPerMemberAndMachine(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	laptop := domain.WorktreeReport{Machine: "laptop", Worktrees: []domain.WorktreeReportEntry{{Name: "app", Branch: "main"}, {Name: "app-fix", Branch: "fix/typo"}}}
	first, err := service.ReportWorktrees(alice, repo, laptop)
	if err != nil || len(first) != 2 || first[0].Name != "app" || first[1].Name != "app-fix" || first[1].Removed {
		t.Fatalf("first list: %+v %v", first, err)
	}
	if _, err := service.ReportWorktrees(alice, repo, domain.WorktreeReport{Machine: "desktop", Worktrees: []domain.WorktreeReportEntry{{Name: "app"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReportWorktrees(bob, repo, domain.WorktreeReport{Machine: "laptop", Worktrees: []domain.WorktreeReportEntry{{Name: "theirs"}}}); err != nil {
		t.Fatal(err)
	}
	// The laptop's next list no longer holds one folder: its name stays, marked removed.
	laptop.Worktrees = laptop.Worktrees[:1]
	if _, err := service.ReportWorktrees(alice, repo, laptop); err != nil {
		t.Fatal(err)
	}
	listed, err := service.ListReportedWorktrees(alice, repo)
	if err != nil || len(listed) != 3 {
		t.Fatalf("own worktrees: %+v %v", listed, err)
	}
	got := map[string]bool{}
	for _, worktree := range listed {
		got[worktree.Machine+"/"+worktree.Name] = worktree.Removed
	}
	if !reflect.DeepEqual(got, map[string]bool{"desktop/app": false, "laptop/app": false, "laptop/app-fix": true}) {
		t.Fatalf("own worktrees: %+v", got)
	}
	if theirs, err := service.ListReportedWorktrees(bob, repo); err != nil || len(theirs) != 1 || theirs[0].Name != "theirs" {
		t.Fatalf("another member's worktrees: %+v %v", theirs, err)
	}
	if _, err := service.ReportWorktrees(alice, repo, domain.WorktreeReport{Worktrees: []domain.WorktreeReportEntry{{Name: "work/app"}}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a path was accepted: %v", err)
	}
	if _, err := service.ReportWorktrees(anchoredActor("viewer"), repo, laptop); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer's list: %v", err)
	}
	if _, err := service.ReportWorktrees(systemTestContext(), repo, laptop); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the server's own list: %v", err)
	}
}

func TestAnchoredSessionCallsGatherWhatASessionSentAndReceived(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	at := func(second int) *time.Time {
		value := time.Date(2031, 1, 2, 3, 0, second, 0, time.UTC)
		return &value
	}
	lead := anchoredTestBody("m3")
	lead.AgentCalls = []domain.AnchoredAgentCall{
		{Tool: "send_input", From: "lead", To: []string{"worker"}, At: at(30)},
		{Tool: "spawn_agent", From: "lead", To: []string{"worker", "other"}, At: at(10)},
		{Tool: "wait", From: "lead", To: []string{"other"}},
	}
	worker := anchoredTestBody("m3")
	worker.AgentCalls = []domain.AnchoredAgentCall{{Tool: "send_input", From: "worker", To: []string{"lead"}, At: at(20)}}
	for id, body := range map[string]domain.AnchoredSessionBody{"lead": lead, "worker": worker} {
		if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderCodex, id, body); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// The same identifiers under another tool, and another member's report, are not mixed in.
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "lead", lead); err != nil {
		t.Fatal(err)
	}
	theirs := anchoredTestBody("m3")
	theirs.AgentCalls = []domain.AnchoredAgentCall{{Tool: "send_input", From: "theirs", To: []string{"worker"}, At: at(5)}}
	if _, err := service.PutAnchoredSession(bob, repo, domain.ProviderCodex, "theirs", theirs); err != nil {
		t.Fatal(err)
	}
	tools := func(calls []domain.AnchoredAgentCall) []string {
		out := []string{}
		for _, call := range calls {
			out = append(out, call.From+">"+call.Tool)
		}
		return out
	}
	calls, err := service.AnchoredSessionCalls(alice, repo, domain.ProviderCodex, "worker")
	if err != nil || !reflect.DeepEqual(tools(calls), []string{"lead>spawn_agent", "worker>send_input", "lead>send_input"}) {
		t.Fatalf("the worker's calls, oldest first: %v %v", tools(calls), err)
	}
	calls, err = service.AnchoredSessionCalls(alice, repo, domain.ProviderCodex, "lead")
	if err != nil || !reflect.DeepEqual(tools(calls), []string{"lead>spawn_agent", "worker>send_input", "lead>send_input", "lead>wait"}) {
		t.Fatalf("the lead's calls, a call without a time last: %v %v", tools(calls), err)
	}
	if _, err := service.AnchoredSessionCalls(alice, repo, domain.ProviderCodex, "other"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a session that is not among the caller's reports: %v", err)
	}
	if _, err := service.AnchoredSessionCalls(bob, repo, domain.ProviderCodex, "worker"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member's session: %v", err)
	}
	if _, err := service.AnchoredSessionCalls(alice, repo, domain.ProviderCodex, "a/b"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a malformed identifier: %v", err)
	}
}
