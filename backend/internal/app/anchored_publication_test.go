package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// anchoredPublicationFixture is a private repository with an origin, two
// members and a provider held in memory.
func anchoredPublicationFixture(t *testing.T) *remoteGitTestFixture {
	t.Helper()
	fixture := newRemoteGitTestFixture(t, 100)
	current, err := fixture.storage.GetRepo(systemTestContext(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.AddMember(systemTestContext(), domain.Membership{RepositoryID: current.RepositoryID, UserID: "member-b", Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// nextPass moves the clock to the next read of the provider and runs it.
func (fixture *remoteGitTestFixture) nextPass(t *testing.T) {
	t.Helper()
	fixture.clock = fixture.clock.Add(5 * time.Minute)
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
}

// anchoredPublicationDoc stores a content document of that many events.
func anchoredPublicationDoc(t *testing.T, fixture *remoteGitTestFixture, events int) domain.ContentHash {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
	for index := 0; index < events; index++ {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic"}}})
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := fixture.storage.PutDoc(systemTestContext(), fixture.repo, doc); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

func anchoredAt(at time.Time) *time.Time { return &at }

func TestSessionOpensToTheTeamAsWhatItExplainsIsPushed(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	pushed := provider.chain(2, "")
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	start := fixture.clock

	// Two segments and an uncommitted part of two turns. The first segment
	// ends at a commit the remote holds; the second at one made locally; the
	// uncommitted part passed through a branch that is not on the remote yet.
	local, written := anchoredTestSHA("b"), remoteGitTestTime(50)
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 7, Machine: "laptop",
		Content: &domain.AnchoredContent{Doc: anchoredPublicationDoc(t, fixture, 7), Events: 7},
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3, FirstEvent: 2, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber, AuthoredAt: &written, AuthorEmail: "member-a@example.com", Subject: "Rebased work"}}},
		},
		Uncommitted: &domain.AnchoredUncommitted{
			AnchoredRange: domain.AnchoredRange{First: "m5", Last: "m7", FirstOrdinal: 4, LastOrdinal: 6, FirstEvent: 4, Events: 3,
				StartedAt: anchoredAt(start.Add(time.Minute)), EndedAt: anchoredAt(start.Add(8 * time.Minute))},
			Turns: []domain.AnchoredTurnMark{{FirstOrdinal: 4, FirstEvent: 4, StartedAt: anchoredAt(start.Add(time.Minute))},
				{FirstOrdinal: 5, FirstEvent: 6, StartedAt: anchoredAt(start.Add(7 * time.Minute))}},
			Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "topic", Rule: domain.AnchoredPlaceRecorded}},
		},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	read := func(actor context.Context) domain.AnchoredSession {
		t.Helper()
		session, err := service.GetAnchoredSession(actor, repo, domain.ProviderClaude, "session-1")
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	listed := func(actor context.Context) domain.AnchoredSessionSummary {
		t.Helper()
		summaries, err := service.ListAnchoredSessions(actor, repo)
		if err != nil || len(summaries) != 1 {
			t.Fatalf("listed: %+v %v", summaries, err)
		}
		return summaries[0]
	}
	events := func(actor context.Context) [][2]int {
		t.Helper()
		access, err := service.AnchoredSessionContentAccess(actor, repo, domain.ProviderClaude, "session-1")
		if err != nil {
			t.Fatal(err)
		}
		if access.Doc != body.Content.Doc || access.Events != 7 {
			t.Fatalf("content access: %+v", access)
		}
		return access.Open
	}

	// The commit that is on the remote opens its segment at once.
	view := read(bob)
	if view.Uploader != "member-a" || len(view.Body.Segments) != 2 || view.Body.Segments[0].Hidden || view.Body.Segments[0].Last != "m2" ||
		!reflect.DeepEqual(view.Body.Segments[1], domain.AnchoredSegment{Commits: []domain.AnchoredCommit{}, Hidden: true}) ||
		view.Body.Uncommitted != nil || view.Body.Records != 2 || view.Body.Machine != "" || view.Body.Content.Doc != "" {
		t.Fatalf("what another member reads at first: %+v", view.Body)
	}
	if open := events(bob); !reflect.DeepEqual(open, [][2]int{{0, 2}}) {
		t.Fatalf("events open at first: %v", open)
	}
	held, missing := true, false
	summary := listed(bob)
	if !summary.Shared || summary.Uploader != "member-a" || summary.Segments != 1 || summary.Uncommitted != nil ||
		!reflect.DeepEqual(summary.Commits, []domain.AnchoredSummaryCommit{{SHA: pushed, SegmentNo: 1, Rule: domain.AnchoredLinkPrintedNumber, OnRemote: &held}}) {
		t.Fatalf("what another member sees listed at first: %+v", summary)
	}
	if segments, err := service.AnchoredCommitSegments(bob, repo, pushed); err != nil || len(segments) != 1 || segments[0].SegmentNo != 1 {
		t.Fatalf("segments of the pushed commit: %+v %v", segments, err)
	}
	if segments, err := service.AnchoredCommitSegments(bob, repo, local); err != nil || len(segments) != 0 {
		t.Fatalf("segments of the local commit: %+v %v", segments, err)
	}
	// Its uploader reads all of it, and where each commit is.
	own := listed(alice)
	if own.Shared || own.Segments != 2 || own.Uncommitted == nil || len(own.Commits) != 2 || !reflect.DeepEqual(own.Commits[0].OnRemote, &held) ||
		!reflect.DeepEqual(own.Commits[1].OnRemote, &missing) || len(own.Commits[1].Copies) != 0 {
		t.Fatalf("what the uploader sees listed: %+v", own)
	}
	if full := read(alice); full.Body.Machine != "laptop" || full.Body.Uncommitted == nil || full.Body.Content.Doc != body.Content.Doc || full.Body.Segments[1].Hidden {
		t.Fatalf("what the uploader reads: %+v", full.Body)
	}
	if open := events(alice); !reflect.DeepEqual(open, [][2]int{{0, 7}}) {
		t.Fatalf("events open to the uploader: %v", open)
	}

	// The local commit is rebased and pushed as a new branch: a copy of it is
	// now on the remote, and the branch the uncommitted part passed through
	// was pushed five minutes in, when only the first turn had begun.
	provider.next++
	rebased := remoteGitTestSHA(provider.next)
	provider.commits[rebased] = domain.RemoteCommit{SHA: rebased, Parents: []string{pushed}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: written, CommittedAt: remoteGitTestTime(60), Subject: "Rebased work"}
	provider.heads["topic"] = rebased
	fixture.nextPass(t)
	view = read(bob)
	part := view.Body.Uncommitted
	if view.Body.Segments[1].Hidden || view.Body.Segments[1].Commits[0].AuthorEmail != "" || part == nil || !part.Partial || part.FirstOrdinal != 4 || part.LastOrdinal != 4 ||
		part.Events != 2 || part.Last != "" || len(part.Turns) != 0 || !reflect.DeepEqual(part.Places, []domain.AnchoredPlace{{Branch: "topic", Rule: domain.AnchoredPlaceRecorded}}) ||
		view.Body.Records != 5 {
		t.Fatalf("what another member reads after the push: %+v %+v", view.Body, part)
	}
	if open := events(bob); !reflect.DeepEqual(open, [][2]int{{0, 6}}) {
		t.Fatalf("events open after the push: %v", open)
	}
	summary = listed(bob)
	if summary.Segments != 2 || len(summary.Commits) != 2 || !reflect.DeepEqual(summary.Commits[1].OnRemote, &missing) || !reflect.DeepEqual(summary.Commits[1].Copies, []string{rebased}) || summary.Uncommitted == nil {
		t.Fatalf("what another member sees listed after the push: %+v", summary)
	}
	if segments, err := service.AnchoredCommitSegments(bob, repo, local); err != nil || len(segments) != 1 || segments[0].SegmentNo != 2 {
		t.Fatalf("segments of the copied commit: %+v %v", segments, err)
	}

	// The branch is pushed again after the second turn began.
	provider.heads["topic"] = provider.commit(rebased)
	fixture.nextPass(t)
	if part := read(bob).Body.Uncommitted; part == nil || part.Partial || part.LastOrdinal != 6 || part.Last != "m7" || part.Events != 3 {
		t.Fatalf("the uncommitted part after the second push: %+v", part)
	}
	if open := events(bob); !reflect.DeepEqual(open, [][2]int{{0, 7}}) {
		t.Fatalf("events open after the second push: %v", open)
	}

	// What was opened stays open when the session is cut another way: here
	// the second segment's commit is gone and its records are uncommitted,
	// in a place no push ever reached.
	recut := body
	recut.Segments = body.Segments[:1]
	recut.Uncommitted = &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m7", FirstOrdinal: 2, LastOrdinal: 6, FirstEvent: 2, Events: 5},
		Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "elsewhere", Rule: domain.AnchoredPlaceRecorded}}}
	if receipt, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", recut); err != nil || !receipt.Changed {
		t.Fatalf("report cut another way: %+v %v", receipt, err)
	}
	if view := read(bob); len(view.Body.Segments) != 1 || view.Body.Uncommitted == nil || view.Body.Uncommitted.Partial || view.Body.Uncommitted.FirstOrdinal != 2 || view.Body.Uncommitted.LastOrdinal != 6 {
		t.Fatalf("what another member reads of the session cut another way: %+v %+v", view.Body, view.Body.Uncommitted)
	}
	if open := events(bob); !reflect.DeepEqual(open, [][2]int{{0, 7}}) {
		t.Fatalf("events open of the session cut another way: %v", open)
	}
	publication, err := fixture.storage.GetAnchoredPublication(context.Background(), repo, domain.ProviderClaude, "session-1")
	if err != nil || len(publication) != 4 || publication[0].By != (domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByCommit, Commit: pushed}) ||
		publication[1].By != (domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByCommit, Commit: rebased}) ||
		publication[2].By != (domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByBranchPush, Branch: "topic"}) || publication[2].LastOrdinal != 4 || publication[3].LastOrdinal != 6 {
		t.Fatalf("what is recorded as open: %+v %v", publication, err)
	}
}

// The first read of a repository finds every branch new and says nothing
// about when any was pushed, so it opens no uncommitted part. Neither does a
// push that came before the part began.
func TestUncommittedPartStaysPrivateUntilItsBranchIsSeenPushed(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	provider.heads["main"] = provider.chain(1, "")
	start := fixture.clock
	report := func(session string, began time.Time) {
		t.Helper()
		body := domain.AnchoredSessionBody{RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 1,
			Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m1", StartedAt: &began, EndedAt: &began},
				Turns: []domain.AnchoredTurnMark{{StartedAt: &began}}, Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "main", Rule: domain.AnchoredPlaceRecorded}}}}
		if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, session, body); err != nil {
			t.Fatal(err)
		}
	}
	shared := func() []string {
		t.Helper()
		summaries, err := service.ListAnchoredSessions(bob, repo)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, summary := range summaries {
			ids = append(ids, summary.SessionID)
		}
		return ids
	}
	report("session-before", start.Add(-time.Hour))
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	if ids := shared(); len(ids) != 0 {
		t.Fatalf("the first read opened %v", ids)
	}
	// Nothing was known when the session was reported, and its own list says
	// nothing about the remote for a session without commits.
	if own, err := service.ListAnchoredSessions(alice, repo); err != nil || len(own) != 1 || own[0].Shared {
		t.Fatalf("own list: %+v %v", own, err)
	}

	// A push seen later opens the part that had begun by then, and not the
	// one that begins after it.
	report("session-after", start.Add(6*time.Minute))
	provider.heads["main"] = provider.commit(provider.heads["main"])
	fixture.nextPass(t)
	if ids := shared(); !reflect.DeepEqual(ids, []string{"session-before"}) {
		t.Fatalf("open after the push was seen: %v", ids)
	}
	if _, err := service.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-after"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a part begun after the push: %v", err)
	}
	if _, err := service.AnchoredSessionContentAccess(bob, repo, domain.ProviderClaude, "session-before"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("content of a session without content: %v", err)
	}
	// A read that fails opens nothing new.
	report("session-third", start.Add(7*time.Minute))
	provider.heads["main"] = provider.commit(provider.heads["main"])
	provider.failures["pulls:1"] = errors.New("provider unavailable")
	fixture.clock = fixture.clock.Add(5 * time.Minute)
	if _, err := fixture.pass(t); err == nil {
		t.Fatal("the failing read did not fail")
	}
	if ids := shared(); !reflect.DeepEqual(ids, []string{"session-before"}) {
		t.Fatalf("open after a failed read: %v", ids)
	}
}

func TestSpawnedSessionOpensWithThePointThatSpawnedIt(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	provider.heads["main"] = provider.chain(1, "")
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	local := anchoredTestSHA("c")
	parent := domain.AnchoredSessionBody{RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 4,
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1},
			Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber}}}},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3}, Unplaced: true, Places: []domain.AnchoredPlace{}}}
	spawned := func(session, parentID string, at int) {
		t.Helper()
		body := domain.AnchoredSessionBody{RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 2, Sub: true, Parent: parentID,
			Spawn:       &domain.AnchoredSpawn{AtRecord: "m", AtOrdinal: at, Rule: domain.AnchoredSpawnToolCall},
			Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "s1", Last: "s2", FirstOrdinal: 0, LastOrdinal: 1}, Unplaced: true, Places: []domain.AnchoredPlace{}}}
		if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, session, body); err != nil {
			t.Fatal(err)
		}
	}
	shared := func() map[string]domain.AnchoredSessionSummary {
		t.Helper()
		summaries, err := service.ListAnchoredSessions(bob, repo)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]domain.AnchoredSessionSummary{}
		for _, summary := range summaries {
			byID[summary.SessionID] = summary
		}
		return byID
	}
	// The sessions are reported from the innermost outwards, before anything
	// of them is pushed.
	spawned("session-grandchild", "session-child", 1)
	spawned("session-child", "session-parent", 0)
	spawned("session-late-child", "session-parent", 3)
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-parent", parent); err != nil {
		t.Fatal(err)
	}
	if open := shared(); len(open) != 0 {
		t.Fatalf("open before the push: %+v", open)
	}

	// The parent's commit is pushed. Its segment opens, and with it the
	// session spawned there and the one that session spawned; the session
	// spawned in the part that is still private stays private.
	provider.commits[local] = domain.RemoteCommit{SHA: local, Parents: []string{provider.heads["main"]}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: remoteGitTestTime(70), CommittedAt: remoteGitTestTime(70), Subject: "Pushed work"}
	provider.heads["main"] = local
	fixture.nextPass(t)
	open := shared()
	if len(open) != 3 || open["session-parent"].Uncommitted != nil || open["session-child"].SpawnedIn == nil || open["session-child"].SpawnedIn.SegmentNo != 1 ||
		open["session-grandchild"].SpawnedIn == nil || !open["session-grandchild"].SpawnedIn.Uncommitted {
		t.Fatalf("open after the push: %+v", open)
	}
	child, err := service.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-child")
	if err != nil || child.Body.Uncommitted == nil || child.Body.Uncommitted.Partial || child.Body.Uncommitted.LastOrdinal != 1 {
		t.Fatalf("the spawned session as another member reads it: %+v %v", child.Body, err)
	}
	// A session spawned at an open point opens when it is reported.
	spawned("session-new-child", "session-parent", 1)
	if open := shared(); len(open) != 4 || open["session-new-child"].SpawnedIn == nil {
		t.Fatalf("a session reported after its parent opened: %+v", open)
	}
	publication, err := fixture.storage.GetAnchoredPublication(context.Background(), repo, domain.ProviderClaude, "session-grandchild")
	if err != nil || len(publication) != 1 || publication[0].By != (domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedBySpawn, Parent: "session-child"}) || publication[0].LastOrdinal != 1 {
		t.Fatalf("what is recorded as open of the innermost session: %+v %v", publication, err)
	}
}

// Without a read of the provider nothing is known to be on the remote, and
// nothing is said to be missing from it.
func TestNothingOpensBeforeTheRemoteWasRead(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3")); err != nil {
		t.Fatal(err)
	}
	own, err := service.ListAnchoredSessions(alice, repo)
	if err != nil || len(own) != 1 || len(own[0].Commits) != 1 || own[0].Commits[0].OnRemote != nil || own[0].Commits[0].Copies != nil {
		t.Fatalf("own list: %+v %v", own, err)
	}
	if shared, err := service.ListAnchoredSessions(bob, repo); err != nil || len(shared) != 0 {
		t.Fatalf("another member's list: %+v %v", shared, err)
	}
	if _, err := service.AnchoredSessionContentAccess(bob, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member's content access: %v", err)
	}
}

// Whoever may view the repository reads what is open of it. A reader with
// no account reads exactly what another member reads, owns nothing, and
// stores nothing.
func TestAReaderWithoutAnAccountReadsWhatIsOpen(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, visitor := anchoredActor("member-a"), anchoredActor("member-b"), context.Background()
	pushed, local := provider.chain(1, ""), anchoredTestSHA("b")
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 5, Machine: "laptop",
		Content: &domain.AnchoredContent{Doc: anchoredPublicationDoc(t, fixture, 5), Events: 5},
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3, FirstEvent: 2, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber}}},
		},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m5", Last: "m5", FirstOrdinal: 4, LastOrdinal: 4, FirstEvent: 4, Events: 1},
			Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "topic", Rule: domain.AnchoredPlaceRecorded}}},
		AgentCalls: []domain.AnchoredAgentCall{{Tool: "Task", From: "session-1", To: []string{"session-2"}, Status: "completed"}},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	// A session nothing is pushed of.
	private := anchoredTestBody("m3")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-private", private); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReportWorktrees(alice, repo, domain.WorktreeReport{Machine: "laptop", Worktrees: []domain.WorktreeReportEntry{{Name: "app", Branch: "topic"}}}); err != nil {
		t.Fatal(err)
	}
	onRemote, err := service.CreateAnchoredNote(alice, repo, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: pushed}, "about a pushed commit")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []domain.AnchoredNoteTarget{{Kind: domain.AnchoredNoteCommit, Commit: local}, {Kind: domain.AnchoredNoteWorktree, Worktree: "app", Machine: "laptop"}} {
		if _, err := service.CreateAnchoredNote(alice, repo, target, "not open"); err != nil {
			t.Fatal(err)
		}
	}

	// The uploader has a public handle; such a reader may not read the member list.
	if err := fixture.storage.UpsertUser(systemTestContext(), domain.User{ID: "member-a", Username: "alice", Email: "alice@example.test", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}

	theirs, err := service.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	seen, err := service.GetAnchoredSession(visitor, repo, domain.ProviderClaude, "session-1")
	if err != nil || !reflect.DeepEqual(seen, theirs) || seen.Body.Segments[0].Hidden || !seen.Body.Segments[1].Hidden || seen.Body.Uncommitted != nil ||
		seen.Body.Machine != "" || seen.Body.Content.Doc != "" || len(seen.Body.AgentCalls) != 0 {
		t.Fatalf("what a reader without an account reads: %+v %v", seen.Body, err)
	}
	listed, err := service.ListAnchoredSessions(visitor, repo)
	shared, sharedErr := service.ListAnchoredSessions(bob, repo)
	if err != nil || sharedErr != nil || len(listed) != 1 || !listed[0].Shared || listed[0].UploaderName != "alice" || !reflect.DeepEqual(listed, shared) {
		t.Fatalf("listed without an account: %+v %v (another member: %+v %v)", listed, err, shared, sharedErr)
	}
	if own, err := service.ListAnchoredSessions(alice, repo); err != nil || len(own) != 2 || own[0].Shared || own[0].UploaderName != "" {
		t.Fatalf("the uploader's own list: %+v %v", own, err)
	}
	if access, err := service.AnchoredSessionContentAccess(visitor, repo, domain.ProviderClaude, "session-1"); err != nil || !reflect.DeepEqual(access.Open, [][2]int{{0, 2}}) {
		t.Fatalf("events open without an account: %+v %v", access, err)
	}
	if segments, err := service.AnchoredCommitSegments(visitor, repo, pushed); err != nil || len(segments) != 1 || segments[0].SegmentNo != 1 {
		t.Fatalf("segments of the pushed commit: %+v %v", segments, err)
	}
	if segments, err := service.AnchoredCommitSegments(visitor, repo, local); err != nil || len(segments) != 0 {
		t.Fatalf("segments of the local commit: %+v %v", segments, err)
	}
	if notes, err := service.ListAnchoredNotes(visitor, repo); err != nil || len(notes) != 1 || notes[0].ID != onRemote.ID || notes[0].AuthorName != "alice" {
		t.Fatalf("notes without an account: %+v %v", notes, err)
	}
	// What is its member's alone stays so.
	if _, err := service.GetAnchoredSession(visitor, repo, domain.ProviderClaude, "session-private"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a session nothing is pushed of: %v", err)
	}
	if _, err := service.AnchoredSessionContentAccess(visitor, repo, domain.ProviderClaude, "session-private"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the conversation of a session nothing is pushed of: %v", err)
	}
	if worktrees, err := service.ListReportedWorktrees(visitor, repo); err != nil || worktrees == nil || len(worktrees) != 0 {
		t.Fatalf("worktrees without an account: %+v %v", worktrees, err)
	}
	if _, err := service.AnchoredSessionCalls(visitor, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("agent calls without an account: %v", err)
	}
	// Reading is all such a reader does: a write needs an account.
	refused := func(err error) bool {
		return errors.Is(err, domain.ErrUnauthorized) || errors.Is(err, domain.ErrForbidden)
	}
	if _, err := service.PutAnchoredSession(visitor, repo, domain.ProviderClaude, "session-9", private); !refused(err) {
		t.Fatalf("a report without an account: %v", err)
	}
	if _, err := service.CreateAnchoredNote(visitor, repo, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: pushed}, "anonymous"); !refused(err) {
		t.Fatalf("a note without an account: %v", err)
	}
	if _, err := service.ReportWorktrees(visitor, repo, domain.WorktreeReport{Machine: "x", Worktrees: []domain.WorktreeReportEntry{{Name: "app"}}}); !refused(err) {
		t.Fatalf("a worktree list without an account: %v", err)
	}
	if listed, err := service.ListAnchoredSessions(alice, repo); err != nil || len(listed) != 2 {
		t.Fatalf("a refused write changed what is stored: %+v %v", listed, err)
	}
}
