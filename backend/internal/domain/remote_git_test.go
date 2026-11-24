package domain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func remoteGitTestSHA(letter string) string { return strings.Repeat(letter, 40) }

func remoteGitTestTime(minute, second int) time.Time {
	return time.Date(2031, 1, 2, 3, minute, second, 0, time.UTC)
}

func remoteGitTestCommit(letter string, parents ...string) RemoteCommit {
	return RemoteCommit{SHA: remoteGitTestSHA(letter), Parents: parents, AuthorName: "Author", AuthorEmail: "author@example.com",
		AuthoredAt: remoteGitTestTime(1, 0), CommittedAt: remoteGitTestTime(2, 0), Subject: "subject " + letter}
}

func TestRemoteCommitSubjectIsWhatGitPrints(t *testing.T) {
	long := strings.Repeat("a", RemoteCommitSubjectLimit-1) + "étail"
	for _, test := range []struct {
		name, message, want string
	}{
		{"single line", "fix the reader\n", "fix the reader"},
		{"no final newline", "fix the reader", "fix the reader"},
		{"title and body", "fix the reader\n\nIt read one page too many.\n", "fix the reader"},
		{"title on several lines", "fix the reader\nwhen a page is short\n\nbody\n", "fix the reader when a page is short"},
		{"carriage returns", "fix the reader\r\nwhen a page is short\r\n\r\nbody\r\n", "fix the reader when a page is short"},
		{"leading blank lines", "\n\n \t\nfix the reader\n\nbody\n", "fix the reader"},
		{"a line of spaces ends the title", "fix the reader\n   \nbody\n", "fix the reader"},
		{"trailing whitespace of a line", "fix the reader \t\nwhen short  \n", "fix the reader when short"},
		{"leading whitespace of a line is kept", "  fix the reader\n  when short\n", "  fix the reader   when short"},
		{"other control characters are not whitespace", "fix\vthe\freader\n", "fix\vthe\freader"},
		{"empty", "", ""},
		{"only blank lines", "\n \n\t\n", ""},
		{"long", long + "\n\nbody", long[:RemoteCommitSubjectLimit-1]},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := RemoteCommitSubject(test.message)
			if got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
			if len(got) > RemoteCommitSubjectLimit || !utf8.ValidString(got) {
				t.Fatalf("a subject of %d bytes, valid=%v", len(got), utf8.ValidString(got))
			}
		})
	}
	// A subject of exactly the limit is kept whole.
	exact := strings.Repeat("b", RemoteCommitSubjectLimit)
	if got := RemoteCommitSubject(exact + "\n"); got != exact {
		t.Fatalf("a subject at the limit was cut to %d bytes", len(got))
	}
}

func TestRemoteGitValidation(t *testing.T) {
	a, b := remoteGitTestSHA("a"), remoteGitTestSHA("b")
	commit := remoteGitTestCommit("a", b)
	if err := commit.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RemoteCommit{SHA: strings.Repeat("c", 64), AuthoredAt: remoteGitTestTime(1, 0), CommittedAt: remoteGitTestTime(1, 0)}).Validate(); err != nil {
		t.Fatalf("a root commit with a 64-character number and no author text: %v", err)
	}
	for name, change := range map[string]func(*RemoteCommit){
		"a short number":                func(c *RemoteCommit) { c.SHA = a[:39] },
		"an upper-case number":          func(c *RemoteCommit) { c.SHA = strings.ToUpper(a) },
		"a parent without a number":     func(c *RemoteCommit) { c.Parents = []string{"main"} },
		"no author time":                func(c *RemoteCommit) { c.AuthoredAt = time.Time{} },
		"no commit time":                func(c *RemoteCommit) { c.CommittedAt = time.Time{} },
		"a time with a fraction":        func(c *RemoteCommit) { c.AuthoredAt = c.AuthoredAt.Add(time.Millisecond) },
		"a time outside UTC":            func(c *RemoteCommit) { c.CommittedAt = c.CommittedAt.In(time.FixedZone("east", 3600)) },
		"a subject over the limit":      func(c *RemoteCommit) { c.Subject = strings.Repeat("s", RemoteCommitSubjectLimit+1) },
		"an author name over the limit": func(c *RemoteCommit) { c.AuthorName = strings.Repeat("n", 1025) },
		"a zero byte in the email":      func(c *RemoteCommit) { c.AuthorEmail = "a\x00b@example.com" },
		"too many parents":              func(c *RemoteCommit) { c.Parents = make([]string, 257) },
	} {
		changed := commit
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if key := commit.Key(); key != (RemoteCommitKey{AuthoredAt: commit.AuthoredAt, AuthorEmail: commit.AuthorEmail, Subject: commit.Subject}) || key.Validate() != nil {
		t.Fatalf("what a copy keeps: %+v %v", key, key.Validate())
	}
	if err := (RemoteCommitKey{AuthorEmail: "author@example.com"}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("a key without an author time: %v", err)
	}

	if err := (RemoteBranch{Name: "feature/one", SHA: a}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []RemoteBranch{{Name: "", SHA: a}, {Name: "main", SHA: "main"}, {Name: strings.Repeat("n", 1025), SHA: a}} {
		if err := branch.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("branch %q %q: %v", branch.Name, branch.SHA, err)
		}
	}

	pull := RemotePull{Number: 7, Title: "Read pages", State: RemotePullMerged, HeadRef: "topic", HeadSHA: a, BaseRef: "main", MergeSHA: b,
		Commits: []string{a}, UpdatedAt: remoteGitTestTime(3, 0)}
	if err := pull.Validate(); err != nil {
		t.Fatal(err)
	}
	full := make([]string, RemotePullCommitLimit)
	for index := range full {
		full[index] = fmt.Sprintf("%040x", index+1)
	}
	for name, change := range map[string]func(*RemotePull){
		"no number":                         func(p *RemotePull) { p.Number = 0 },
		"an unknown state":                  func(p *RemotePull) { p.State = "draft" },
		"no head name":                      func(p *RemotePull) { p.HeadRef = "" },
		"no base name":                      func(p *RemotePull) { p.BaseRef = "" },
		"a head without a number":           func(p *RemotePull) { p.HeadSHA = "" },
		"a merge commit while open":         func(p *RemotePull) { p.State = RemotePullOpen },
		"a merge commit while closed":       func(p *RemotePull) { p.State = RemotePullClosed },
		"a merge commit without a number":   func(p *RemotePull) { p.MergeSHA = "merged" },
		"a listed commit without a number":  func(p *RemotePull) { p.Commits = []string{"first"} },
		"more commits than the list holds":  func(p *RemotePull) { p.Commits = append(append([]string(nil), full...), a) },
		"cut short without being full":      func(p *RemotePull) { p.Truncated = true },
		"no update time":                    func(p *RemotePull) { p.UpdatedAt = time.Time{} },
		"an update time with a fraction":    func(p *RemotePull) { p.UpdatedAt = p.UpdatedAt.Add(time.Microsecond) },
		"a title over the limit":            func(p *RemotePull) { p.Title = strings.Repeat("t", 1025) },
		"a title that is not readable text": func(p *RemotePull) { p.Title = "\xff" },
	} {
		changed := pull
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A merged pull request may come without its merge commit, an open one
	// has none, and a full list is the one that may be cut short.
	for _, valid := range []RemotePull{
		{Number: 1, State: RemotePullMerged, HeadRef: "topic", HeadSHA: a, BaseRef: "main", UpdatedAt: remoteGitTestTime(3, 0)},
		{Number: 2, State: RemotePullOpen, HeadRef: "topic", HeadSHA: a, BaseRef: "main", Fork: true, UpdatedAt: remoteGitTestTime(3, 0)},
		{Number: 3, State: RemotePullClosed, HeadRef: "topic", HeadSHA: a, BaseRef: "main", Commits: full, Truncated: true, UpdatedAt: remoteGitTestTime(3, 0)},
	} {
		if err := valid.Validate(); err != nil {
			t.Errorf("pull request %d: %v", valid.Number, err)
		}
	}

	repo := HashContent([]byte("repo"))
	state := NewRemoteGitState(repo, "https://github.com/example/project", remoteGitTestTime(0, 0))
	if err := state.Validate(); err != nil || state.PullPage != 1 || state.PullsComplete || !state.NextAt.Equal(remoteGitTestTime(0, 0)) {
		t.Fatalf("a new state: %+v %v", state, err)
	}
	var zero time.Time
	for name, change := range map[string]func(*RemoteGitState){
		"no repository":           func(s *RemoteGitState) { s.RepoID = "" },
		"no origin":               func(s *RemoteGitState) { s.GitOrigin = "" },
		"no time of a next pass":  func(s *RemoteGitState) { s.NextAt = zero },
		"a negative page":         func(s *RemoteGitState) { s.PullPage = -1 },
		"an unset time of a read": func(s *RemoteGitState) { s.SyncedAt = &zero },
		"a refused commit without a number": func(s *RemoteGitState) {
			s.Unreadable = []string{"gone"}
		},
		"too many refused commits": func(s *RemoteGitState) { s.Unreadable = make([]string, RemoteGitUnreadableLimit+1) },
	} {
		changed := state
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRemoteCommitCursorIsOpaqueAndExact(t *testing.T) {
	commit := remoteGitTestCommit("a")
	cursor := RemoteCommitCursorAt(commit)
	raw := cursor.String()
	if raw == "" || strings.Contains(raw, commit.SHA) {
		t.Fatalf("the cursor shows what it holds: %q", raw)
	}
	parsed, err := ParseRemoteCommitCursor(raw)
	if err != nil || parsed != cursor || parsed.IsZero() {
		t.Fatalf("round trip: %+v %v", parsed, err)
	}
	if top, err := ParseRemoteCommitCursor(""); err != nil || !top.IsZero() || top.String() != "" {
		t.Fatalf("the top of the list: %+v %v", top, err)
	}
	encode := func(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }
	for _, invalid := range []string{"not a cursor", "!!!!", raw + "A", raw[:len(raw)-2], strings.Repeat("A", 300),
		encode("123456"), encode("1." + commit.SHA[:39]), encode("soon." + commit.SHA), encode("01." + commit.SHA),
		encode("99999999999999999999." + commit.SHA), encode("253402300800." + commit.SHA), encode("1." + commit.SHA + ".more")} {
		if _, err := ParseRemoteCommitCursor(invalid); !errors.Is(err, ErrValidation) {
			t.Errorf("%q: %v", invalid, err)
		}
	}
}

// One rule decides when a repository is read: a pass is due when the time of
// the next pass has come and no claim is still held.
func TestRemoteGitSchedulingRule(t *testing.T) {
	repo := HashContent([]byte("repo"))
	const origin = "https://github.com/example/project"
	at := func(seconds int) time.Time { return remoteGitTestTime(10, 0).Add(time.Duration(seconds) * time.Second) }
	var none time.Time

	t.Run("due", func(t *testing.T) {
		state := NewRemoteGitState(repo, origin, at(0))
		state.NextAt = at(60)
		for _, test := range []struct {
			name         string
			now          time.Time
			claimedUntil time.Time
			want         bool
		}{
			{"before the next pass", at(59), none, false},
			{"at the next pass", at(60), none, true},
			{"after the next pass", at(61), none, true},
			{"while a claim is held", at(61), at(120), false},
			{"when the claim ends", at(120), at(120), true},
			{"after the claim ended", at(121), at(120), true},
			{"an ended claim before the next pass", at(30), at(20), false},
		} {
			if got := state.Due(test.now, test.claimedUntil); got != test.want {
				t.Errorf("%s: due=%v", test.name, got)
			}
		}
	})

	t.Run("claim", func(t *testing.T) {
		// A repository never read is read at once.
		state, startOver, ok := ClaimRemoteGitPass(nil, none, repo, origin, at(0))
		if !ok || startOver || state.RepoID != repo || state.GitOrigin != origin || !state.AttemptedAt.Equal(at(0)) || state.PullPage != 1 || state.Validate() != nil {
			t.Fatalf("first claim: %+v %v %v", state, startOver, ok)
		}
		stored := state.Ended(at(0), false, false)
		for _, test := range []struct {
			name          string
			origin        string
			now           time.Time
			claimedUntil  time.Time
			ok, startOver bool
		}{
			// Nobody asked: the next pass is six hours (21,600 seconds) after the last.
			{"before the next pass", origin, at(21599), none, false, false},
			{"at the next pass", origin, at(21600), none, true, false},
			{"while another worker holds the claim", origin, at(21700), at(21701), false, false},
			{"after that claim ended", origin, at(21701), at(21701), true, false},
			{"another origin starts over at once", origin + "-moved", at(1), none, true, true},
			{"another origin waits for a held claim", origin + "-moved", at(1), at(2), false, false},
		} {
			got, startOver, ok := ClaimRemoteGitPass(&stored, test.claimedUntil, repo, test.origin, test.now)
			if ok != test.ok || startOver != test.startOver {
				t.Errorf("%s: ok=%v startOver=%v", test.name, ok, startOver)
				continue
			}
			if !ok {
				continue
			}
			if got.GitOrigin != test.origin || !got.AttemptedAt.Equal(test.now) || got.Validate() != nil {
				t.Errorf("%s: %+v", test.name, got)
			}
			// Starting over keeps nothing of what was read from the other origin.
			if startOver != (got.SyncedAt == nil) {
				t.Errorf("%s: kept or lost the last read: %+v", test.name, got)
			}
		}
		if stored.AttemptedAt == nil || !stored.AttemptedAt.Equal(at(0)) {
			t.Fatal("claiming changed the state it was computed from")
		}
	})

	t.Run("end of a pass", func(t *testing.T) {
		started := NewRemoteGitState(repo, origin, at(0)).Started(at(0))
		done := started.Ended(at(0), false, false)
		if !done.NextAt.Equal(at(21600)) || done.Remaining || !done.SyncedAt.Equal(at(0)) {
			t.Fatalf("nothing left: %+v", done)
		}
		more := started.Ended(at(0), false, true)
		if !more.NextAt.Equal(at(60)) || !more.Remaining || !more.SyncedAt.Equal(at(0)) {
			t.Fatalf("out of budget: %+v", more)
		}
		// A failure keeps the last read and what the pass before it left.
		later := more.Started(at(60))
		failed := later.Ended(at(60), true, false)
		if !failed.NextAt.Equal(at(180)) || !failed.SyncedAt.Equal(at(0)) || !failed.Remaining || !failed.AttemptedAt.Equal(at(60)) {
			t.Fatalf("failed: %+v", failed)
		}
		if started.SyncedAt != nil || !started.NextAt.Equal(at(0)) {
			t.Fatal("ending a pass changed the state it was computed from")
		}
		// A read asked for while the pass ran is not lost when the pass ends.
		for _, test := range []struct {
			name      string
			requested int
			failed    bool
			next      int
		}{
			{"asked early in the pass", 5, false, 30},
			{"asked late in the pass", 45, false, 45},
			{"asked while a pass failed", 5, true, 30},
		} {
			asked := started.Request(at(test.requested))
			if got := asked.Ended(at(0), test.failed, false); !got.NextAt.Equal(at(test.next)) || !got.RequestedAt.Equal(at(test.requested)) {
				t.Errorf("%s: next pass at %s", test.name, got.NextAt)
			}
		}
		// What was asked for before the pass started was read by the pass.
		before := NewRemoteGitState(repo, origin, at(0)).Request(at(-10)).Started(at(0))
		if got := before.Ended(at(0), false, false); !got.NextAt.Equal(at(21600)) {
			t.Fatalf("a request the pass served: next pass at %s", got.NextAt)
		}
	})

	t.Run("request", func(t *testing.T) {
		idle := NewRemoteGitState(repo, origin, at(0)).Started(at(0)).Ended(at(0), false, false)
		for _, test := range []struct {
			name string
			now  int
			next int
		}{
			{"right after a pass started", 10, 30},
			{"at the spacing", 30, 30},
			{"well after the last pass", 100, 100},
			{"hours after the last pass", 20000, 20000},
			{"after the next pass is due anyway", 21700, 21600},
		} {
			got := idle.Request(at(test.now))
			if !got.NextAt.Equal(at(test.next)) || !got.RequestedAt.Equal(at(test.now)) {
				t.Errorf("%s: next pass at %s, requested at %s", test.name, got.NextAt, got.RequestedAt)
			}
		}
		// Asking never pushes the next pass further away.
		soon := idle
		soon.NextAt = at(20)
		if got := soon.Request(at(10)); !got.NextAt.Equal(at(20)) {
			t.Fatalf("a request moved the next pass from %s to %s", soon.NextAt, got.NextAt)
		}
		// A repository whose first pass never started is read at the request.
		if got := NewRemoteGitState(repo, origin, at(50)).Request(at(10)); !got.NextAt.Equal(at(10)) {
			t.Fatalf("before any pass: next pass at %s", got.NextAt)
		}
		if idle.RequestedAt != nil {
			t.Fatal("asking changed the state it was computed from")
		}
	})

	t.Run("same pass", func(t *testing.T) {
		first := NewRemoteGitState(repo, origin, at(0)).Started(at(0))
		if !first.SamePass(first.Request(at(5))) {
			t.Fatal("a request during a pass made it another pass")
		}
		moved := first
		moved.GitOrigin = origin + "-moved"
		if first.SamePass(first.Started(at(1))) || first.SamePass(moved) || NewRemoteGitState(repo, origin, at(0)).SamePass(first) {
			t.Fatal("another start, another origin or no start counted as the same pass")
		}
	})
}

func TestRemoteGitUnreadableIsBounded(t *testing.T) {
	state := NewRemoteGitState(HashContent([]byte("repo")), "https://github.com/example/project", remoteGitTestTime(0, 0))
	first := fmt.Sprintf("%040x", 1)
	for index := 1; index <= RemoteGitUnreadableLimit; index++ {
		state = state.WithUnreadable(fmt.Sprintf("%040x", index))
	}
	if len(state.Unreadable) != RemoteGitUnreadableLimit || state.Unreadable[0] != first || state.Validate() != nil {
		t.Fatalf("a full list: %d %v", len(state.Unreadable), state.Validate())
	}
	// The same commit again is remembered once, as the newest.
	again := state.WithUnreadable(first)
	if len(again.Unreadable) != RemoteGitUnreadableLimit || again.Unreadable[len(again.Unreadable)-1] != first {
		t.Fatalf("a commit refused twice: %d", len(again.Unreadable))
	}
	// One more drops the oldest.
	over := state.WithUnreadable(remoteGitTestSHA("f"))
	if len(over.Unreadable) != RemoteGitUnreadableLimit || over.Unreadable[0] == first || over.Unreadable[len(over.Unreadable)-1] != remoteGitTestSHA("f") || over.Validate() != nil {
		t.Fatalf("over the bound: %d", len(over.Unreadable))
	}
	if len(state.Unreadable) != RemoteGitUnreadableLimit || state.Unreadable[0] != first {
		t.Fatal("remembering changed the state it was computed from")
	}
}
