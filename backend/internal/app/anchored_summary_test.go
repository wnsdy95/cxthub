package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A summary is written by the session's uploader at a commit a segment of the
// session ends at, and read under the rule of that segment.
func TestSummaryIsReadWithTheSegmentItBelongsTo(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, visitor := anchoredActor("member-a"), anchoredActor("member-b"), context.Background()
	pushed, local, other := provider.chain(1, ""), anchoredTestSHA("b"), anchoredTestSHA("c")
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.UpsertUser(systemTestContext(), domain.User{ID: "member-a", Username: "alice", Email: "alice@example.test", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 4,
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1}, Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3}, Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber}}},
		},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	first := domain.AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}}
	second := domain.AnchoredSummaryBody{Summary: "Reworked the parser.", OpenTasks: []string{"Document it."}}

	// Only the uploader writes, and only at a commit the session made.
	if _, err := service.PutAnchoredSummary(bob, repo, domain.ProviderClaude, "session-1", pushed, first); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member wrote a summary: %v", err)
	}
	if _, err := service.PutAnchoredSummary(visitor, repo, domain.ProviderClaude, "session-1", pushed, first); err == nil {
		t.Fatal("a reader without an account wrote a summary")
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", other, first); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a summary at a commit the session did not make: %v", err)
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-0", pushed, first); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a summary of a session that is not stored: %v", err)
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, domain.AnchoredSummaryBody{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a summary that says nothing: %v", err)
	}
	receipt, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, first)
	if err != nil || !receipt.Changed || receipt.BodyHash == "" {
		t.Fatalf("stored: %+v %v", receipt, err)
	}
	if again, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, first); err != nil || again.Changed || again.BodyHash != receipt.BodyHash {
		t.Fatalf("the same text again: %+v %v", again, err)
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", local, second); err != nil {
		t.Fatal(err)
	}

	// The uploader reads both; anyone else the one whose commit is pushed.
	own, err := service.ListAnchoredSummaries(alice, repo, []string{pushed, local})
	if err != nil || len(own) != 2 {
		t.Fatalf("what the uploader reads: %+v %v", own, err)
	}
	for _, summary := range own {
		want := 1
		if summary.Commit == local {
			want = 2
		}
		if summary.SegmentNo != want || summary.Uploader != "member-a" || summary.Provider != domain.ProviderClaude || summary.SessionID != "session-1" {
			t.Fatalf("a summary does not say where it belongs: %+v", summary)
		}
	}
	for name, reader := range map[string]context.Context{"another member": bob, "a reader without an account": visitor} {
		shared, err := service.ListAnchoredSummaries(reader, repo, []string{pushed, local})
		if err != nil || len(shared) != 1 || shared[0].Commit != pushed || shared[0].Body.Summary != "Added the parser." || shared[0].SegmentNo != 1 || shared[0].UploaderName != "alice" {
			t.Fatalf("what %s reads: %+v %v", name, shared, err)
		}
	}

	// Once the local commit is pushed, its summary opens with its segment.
	provider.commits[local] = domain.RemoteCommit{SHA: local, Parents: []string{pushed}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: remoteGitTestTime(70), CommittedAt: remoteGitTestTime(70), Subject: "Pushed work"}
	provider.heads["main"] = local
	fixture.nextPass(t)
	if shared, err := service.ListAnchoredSummaries(bob, repo, []string{local}); err != nil || len(shared) != 1 || shared[0].Body.Summary != "Reworked the parser." {
		t.Fatalf("after the push: %+v %v", shared, err)
	}

	if _, err := service.ListAnchoredSummaries(alice, repo, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a read that names no commit: %v", err)
	}
	if _, err := service.ListAnchoredSummaries(alice, repo, []string{"abc1234"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a read by a short commit number: %v", err)
	}
}

// The summaries along a branch are found by walking the branch's commits on
// the remote: the branch's own first, then those of the branch it was made
// from. A rebased commit is found at its copy, and a commit a pull request
// was squashed into stands for the commits of that pull request.
func TestSummariesAreReadAlongABranch(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, visitor := anchoredActor("member-a"), anchoredActor("member-b"), context.Background()

	// Nothing was read from the remote yet: no branch, no summary.
	if empty, err := service.ReadAnchoredMemory(bob, repo, "main", nil, 5); err != nil || empty.Branch != "" || empty.Head != "" || empty.Summaries == nil || len(empty.Summaries) != 0 {
		t.Fatalf("before the remote was read: %+v %v", empty, err)
	}

	base := provider.chain(2, "")
	first := provider.commit(base)
	second := provider.commit(first)
	provider.heads["main"], provider.heads["topic"] = base, second
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	local, written := anchoredTestSHA("b"), remoteGitTestTime(40)
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 6,
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1}, Commits: []domain.AnchoredCommit{{SHA: base, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3}, Commits: []domain.AnchoredCommit{{SHA: first, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m5", Last: "m6", FirstOrdinal: 4, LastOrdinal: 5},
				Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber, AuthoredAt: &written, AuthorEmail: "member-a@example.com", Subject: "Local work"}}},
		},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	for commit, text := range map[string]string{base: "Base work.", first: "Topic work.", local: "Local work."} {
		if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", commit, domain.AnchoredSummaryBody{Summary: text}); err != nil {
			t.Fatal(err)
		}
	}
	texts := func(memory domain.AnchoredMemory) []string {
		listed := []string{}
		for _, summary := range memory.Summaries {
			listed = append(listed, summary.Body.Summary)
		}
		return listed
	}
	read := func(reader context.Context, branch string, limit int) domain.AnchoredMemory {
		t.Helper()
		memory, err := service.ReadAnchoredMemory(reader, repo, branch, nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		return memory
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for index := range got {
			if got[index] != want[index] {
				return false
			}
		}
		return true
	}

	// The branch's own summary comes first, then that of the branch it was
	// made from. A commit that is on no remote branch is not on the walk,
	// also for the member who made it.
	for name, reader := range map[string]context.Context{"the uploader": alice, "another member": bob, "a reader without an account": visitor} {
		topic := read(reader, "topic", 5)
		if topic.Branch != "topic" || topic.Head != second || topic.Walked != 4 || !same(texts(topic), "Topic work.", "Base work.") {
			t.Fatalf("what %s reads along the branch: %+v", name, topic)
		}
		if topic.Summaries[0].Commit != first || topic.Summaries[0].SegmentNo != 2 || topic.Summaries[0].Uploader != "member-a" {
			t.Fatalf("a summary does not say where it belongs: %+v", topic.Summaries[0])
		}
	}
	// A commit of the caller's folder is looked at when the caller names it,
	// before the branch's commits. One that is not pushed is read by its
	// uploader alone.
	named := func(reader context.Context) []string {
		t.Helper()
		memory, err := service.ReadAnchoredMemory(reader, repo, "topic", []string{local, anchoredTestSHA("d")}, 5)
		if err != nil || memory.Walked != 4 {
			t.Fatalf("with a commit of the folder: %+v %v", memory, err)
		}
		return texts(memory)
	}
	if own := named(alice); !same(own, "Local work.", "Topic work.", "Base work.") {
		t.Fatalf("what the uploader reads with its commit that is not pushed: %v", own)
	}
	for name, reader := range map[string]context.Context{"another member": bob, "a reader without an account": visitor} {
		if other := named(reader); !same(other, "Topic work.", "Base work.") {
			t.Fatalf("what %s reads with a commit that is not pushed: %v", name, other)
		}
	}
	if main := read(bob, "main", 5); main.Branch != "main" || main.Head != base || main.Walked != 2 || !same(texts(main), "Base work.") {
		t.Fatalf("along the default branch: %+v", main)
	}
	// A branch the remote does not hold, and no branch at all, are read as
	// the default branch.
	for _, name := range []string{"not-pushed", ""} {
		if other := read(bob, name, 5); other.Branch != "main" || !same(texts(other), "Base work.") {
			t.Fatalf("along %q: %+v", name, other)
		}
	}
	if one := read(bob, "topic", 1); !same(texts(one), "Topic work.") {
		t.Fatalf("the newest only: %+v", one)
	}

	// The local commit is rebased onto the branch and pushed: its summary is
	// found where the copy stands.
	provider.next++
	rebased := remoteGitTestSHA(provider.next)
	provider.commits[rebased] = domain.RemoteCommit{SHA: rebased, Parents: []string{second}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: written, CommittedAt: remoteGitTestTime(90), Subject: "Local work"}
	provider.heads["topic"] = rebased
	fixture.nextPass(t)
	topic := read(bob, "topic", 5)
	if topic.Head != rebased || !same(texts(topic), "Local work.", "Topic work.", "Base work.") || topic.Summaries[0].Commit != local || topic.Summaries[0].SegmentNo != 3 {
		t.Fatalf("after the rebase: %+v", topic)
	}
	if main := read(bob, "main", 5); !same(texts(main), "Base work.") {
		t.Fatalf("the default branch after the rebase: %+v", main)
	}

	// The branch is squashed into the default branch and deleted: the commit
	// it became stands for the commits of its pull request.
	provider.next++
	squashed := remoteGitTestSHA(provider.next)
	provider.commits[squashed] = domain.RemoteCommit{SHA: squashed, Parents: []string{base}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: remoteGitTestTime(120), CommittedAt: remoteGitTestTime(120), Subject: "Topic (#7)"}
	provider.pull(7, 130, rebased, first, second, rebased)
	pull := provider.pulls[7]
	pull.State, pull.MergeSHA = domain.RemotePullMerged, squashed
	provider.pulls[7] = pull
	provider.heads["main"] = squashed
	delete(provider.heads, "topic")
	fixture.nextPass(t)
	for _, name := range []string{"main", "topic"} {
		merged := read(visitor, name, 5)
		if merged.Branch != "main" || merged.Head != squashed || merged.Walked != 3 || !same(texts(merged), "Local work.", "Topic work.", "Base work.") {
			t.Fatalf("along %q after the squash: %+v", name, merged)
		}
	}
	// The same holds for the commits the caller names from its own folder:
	// the pull request's commits come right after the commit it became.
	pulled, err := service.ReadAnchoredMemory(visitor, repo, "main", []string{squashed, base}, 5)
	if err != nil || !same(texts(pulled), "Local work.", "Topic work.", "Base work.") {
		t.Fatalf("naming the squashed commit: %+v %v", pulled, err)
	}

	for name, call := range map[string]func() error{
		"no summary asked for": func() error { _, err := service.ReadAnchoredMemory(bob, repo, "main", nil, 0); return err },
		"too many asked for": func() error {
			_, err := service.ReadAnchoredMemory(bob, repo, "main", nil, domain.AnchoredMemoryLimit+1)
			return err
		},
		"a short commit number": func() error {
			_, err := service.ReadAnchoredMemory(bob, repo, "main", []string{"abc1234"}, 5)
			return err
		},
		"too many commits": func() error {
			_, err := service.ReadAnchoredMemory(bob, repo, "main", make([]string, domain.AnchoredMemoryLocalCommits+1), 5)
			return err
		},
		"a name with a NUL": func() error { _, err := service.ReadAnchoredMemory(bob, repo, "ma\x00in", nil, 5); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A repository whose remote was never read has no branch to walk. A member
// still finds what that member wrote at a commit of that member's folder.
func TestSummaryOfACommitThatIsNotPushedIsFoundWithoutTheRemote(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3")); err != nil {
		t.Fatal(err)
	}
	commit := anchoredTestSHA("a")
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", commit, domain.AnchoredSummaryBody{Summary: "Local work."}); err != nil {
		t.Fatal(err)
	}
	own, err := service.ReadAnchoredMemory(alice, repo, "main", []string{commit}, 5)
	if err != nil || own.Branch != "" || own.Walked != 0 || len(own.Summaries) != 1 || own.Summaries[0].Body.Summary != "Local work." || own.Summaries[0].SegmentNo != 1 {
		t.Fatalf("what the uploader reads: %+v %v", own, err)
	}
	if other, err := service.ReadAnchoredMemory(bob, repo, "main", []string{commit}, 5); err != nil || len(other.Summaries) != 0 {
		t.Fatalf("what another member reads: %+v %v", other, err)
	}
	if none, err := service.ReadAnchoredMemory(alice, repo, "main", nil, 5); err != nil || none.Summaries == nil || len(none.Summaries) != 0 {
		t.Fatalf("without naming the commit: %+v %v", none, err)
	}
}
