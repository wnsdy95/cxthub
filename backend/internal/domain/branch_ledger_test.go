package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func branchLedgerTestID(letter string) string { return strings.Repeat(letter, 32) }

func branchLedgerTestTime(minute int) time.Time {
	return time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC)
}

func TestBranchLedgerOneRuleForEveryRegistration(t *testing.T) {
	repo := HashContent([]byte("repo"))
	alice := BranchEvidence{Kind: BranchEvidenceRegistered, Account: "alice"}
	bob := BranchEvidence{Kind: BranchEvidenceRegistered, Account: "bob"}
	push := BranchEvidence{Kind: BranchEvidenceHostingPush, Source: "observation-1"}
	deleted := BranchEvidence{Kind: BranchEvidenceHostingPush, Source: "observation-2"}
	server := func() string { return branchLedgerTestID("f") }
	never := func() string { t.Fatal("no ID is made here"); return "" }
	a, b := branchLedgerTestID("a"), branchLedgerTestID("b")

	// No lifetime is alive under the name: the registering side's ID opens one.
	opened, outcome, store := RegisterBranchID(repo, "topic", a, nil, nil, alice, branchLedgerTestTime(1), never)
	if outcome != BranchLedgerOpened || !store || opened.LedgerID != a || opened.Name != "topic" || !opened.Alive() || opened.OpenedBy != alice || opened.Validate() != nil {
		t.Fatalf("opened: %+v %s %v", opened, outcome, opened.Validate())
	}
	// The same registration again changes nothing.
	if _, outcome, store := RegisterBranchID(repo, "topic", a, &opened, &opened, alice, branchLedgerTestTime(2), never); outcome != BranchLedgerUnchanged || store {
		t.Fatalf("again: %s %v", outcome, store)
	}
	// A name alive on the remote has one lifetime: another ID joins it.
	joined, outcome, store := RegisterBranchID(repo, "topic", b, &opened, nil, bob, branchLedgerTestTime(3), never)
	if outcome != BranchLedgerJoined || !store || joined.LedgerID != a || !joined.Holds(b) || !reflect.DeepEqual(joined.IDs(), []string{a, b}) || joined.Linked[0].By != bob || joined.Validate() != nil {
		t.Fatalf("joined: %+v %s %v", joined, outcome, joined.Validate())
	}
	if len(opened.Linked) != 0 {
		t.Fatal("joining changed the lifetime it was computed from")
	}

	// A deletion closes the lifetime and keeps it.
	closed, outcome, store := ObserveBranchDeleted(&joined, deleted, branchLedgerTestTime(4))
	if outcome != BranchLedgerClosed || !store || closed.Alive() || !closed.ClosedAt.Equal(branchLedgerTestTime(4)) || *closed.ClosedBy != deleted || closed.Validate() != nil {
		t.Fatalf("closed: %+v %s %v", closed, outcome, closed.Validate())
	}
	if _, outcome, store := ObserveBranchDeleted(nil, deleted, branchLedgerTestTime(4)); outcome != BranchLedgerUnchanged || store {
		t.Fatalf("deleting a name without a lifetime: %s %v", outcome, store)
	}

	// The same name later, with an ID the ledger never saw, is another lifetime.
	c := branchLedgerTestID("c")
	later, outcome, _ := RegisterBranchID(repo, "topic", c, nil, nil, bob, branchLedgerTestTime(5), never)
	if outcome != BranchLedgerOpened || later.LedgerID != c {
		t.Fatalf("a reused name: %+v %s", later, outcome)
	}
	// Registering an ID of the closed lifetime again continues that lifetime,
	// whether it was its own ID or one joined to it.
	for _, id := range []string{a, b} {
		reopened, outcome, store := RegisterBranchID(repo, "topic", id, nil, &closed, alice, branchLedgerTestTime(6), never)
		if outcome != BranchLedgerReopened || !store || reopened.LedgerID != a || !reopened.Alive() || reopened.ClosedBy != nil || len(reopened.Reopenings) != 1 ||
			!reopened.Reopenings[0].ClosedAt.Equal(branchLedgerTestTime(4)) || reopened.Reopenings[0].ReopenedBy != alice || reopened.Reopenings[0].Name != "" || reopened.Validate() != nil {
			t.Fatalf("reopened by %s: %+v %s %v", id[:1], reopened, outcome, reopened.Validate())
		}
	}
	// Under another name it is still that lifetime; the record keeps the name it had.
	renamed, outcome, _ := RegisterBranchID(repo, "topic-renamed", a, nil, &closed, alice, branchLedgerTestTime(6), never)
	if outcome != BranchLedgerReopened || renamed.Name != "topic-renamed" || renamed.Reopenings[0].Name != "topic" || renamed.Validate() != nil {
		t.Fatalf("reopened under another name: %+v %s", renamed, outcome)
	}

	// An ID that is the identity of a branch alive under another name cannot
	// be the identity of this one too: the name gets an ID of its own.
	separate, outcome, store := RegisterBranchID(repo, "second-name", a, nil, &opened, alice, branchLedgerTestTime(7), server)
	if outcome != BranchLedgerSeparate || !store || separate.LedgerID != branchLedgerTestID("f") || separate.Name != "second-name" || separate.Holds(a) || separate.Validate() != nil {
		t.Fatalf("one ID under two names: %+v %s", separate, outcome)
	}
	// And when that name already has its lifetime, nothing changes.
	if unchanged, outcome, store := RegisterBranchID(repo, "second-name", a, &separate, &opened, alice, branchLedgerTestTime(8), never); outcome != BranchLedgerSeparate || store || unchanged.LedgerID != separate.LedgerID {
		t.Fatalf("one ID under two living names: %+v %s %v", unchanged, outcome, store)
	}

	// The hosting provider showing a name nobody registered: the server makes the ID.
	seen, outcome, store := ObserveBranchOnRemote(repo, "from-the-web", nil, push, branchLedgerTestTime(9), server)
	if outcome != BranchLedgerOpened || !store || seen.LedgerID != branchLedgerTestID("f") || seen.OpenedBy != push || seen.Validate() != nil {
		t.Fatalf("seen: %+v %s", seen, outcome)
	}
	if _, outcome, store := ObserveBranchOnRemote(repo, "from-the-web", &seen, push, branchLedgerTestTime(10), never); outcome != BranchLedgerUnchanged || store {
		t.Fatalf("seen again: %s %v", outcome, store)
	}
	// A registration after the server saw the name joins the server's lifetime.
	if joined, outcome, _ := RegisterBranchID(repo, "from-the-web", a, &seen, nil, alice, branchLedgerTestTime(11), never); outcome != BranchLedgerJoined || joined.LedgerID != seen.LedgerID || !joined.Holds(a) {
		t.Fatalf("registering after the server: %+v %s", joined, outcome)
	}
	// A delivery processed twice is recognised by its observation.
	if !closed.SawObservation("observation-2") || closed.SawObservation("observation-1") || !seen.SawObservation("observation-1") || seen.SawObservation("") {
		t.Fatal("observations a lifetime saw")
	}
}

func TestBranchLifetimeValidation(t *testing.T) {
	valid := func() BranchLifetime {
		closedAt := branchLedgerTestTime(5)
		return BranchLifetime{
			RepoID: HashContent([]byte("repo")), LedgerID: branchLedgerTestID("a"), Name: "feature/one", OpenedAt: branchLedgerTestTime(1),
			OpenedBy: BranchEvidence{Kind: BranchEvidenceRegistered, Account: "alice"},
			ClosedAt: &closedAt, ClosedBy: &BranchEvidence{Kind: BranchEvidenceHostingPush, Source: "observation-2"},
			Linked: []BranchLinkedID{{ID: branchLedgerTestID("b"), LinkedAt: branchLedgerTestTime(2), By: BranchEvidence{Kind: BranchEvidenceRegistered, Account: "bob"}}},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a well-formed lifetime was rejected: %v", err)
	}
	for name, change := range map[string]func(*BranchLifetime){
		"a repository that is not a hash":   func(l *BranchLifetime) { l.RepoID = "repo" },
		"an ID in another form":             func(l *BranchLifetime) { l.LedgerID = "legacy-" + branchLedgerTestID("a")[:25] },
		"an upper-case ID":                  func(l *BranchLifetime) { l.LedgerID = strings.Repeat("A", 32) },
		"a name that is not a branch name":  func(l *BranchLifetime) { l.Name = "feature..one" },
		"a name with a space":               func(l *BranchLifetime) { l.Name = "feature one" },
		"a name longer than the limit":      func(l *BranchLifetime) { l.Name = strings.Repeat("n", 256) },
		"no opening time":                   func(l *BranchLifetime) { l.OpenedAt = time.Time{} },
		"evidence of an unknown kind":       func(l *BranchLifetime) { l.OpenedBy.Kind = "guessed" },
		"a registration without an account": func(l *BranchLifetime) { l.OpenedBy.Account = " " },
		"a registration with a source":      func(l *BranchLifetime) { l.OpenedBy.Source = "observation-1" },
		"a hosting event without a source":  func(l *BranchLifetime) { l.ClosedBy.Source = "" },
		"a closing time without evidence":   func(l *BranchLifetime) { l.ClosedBy = nil },
		"evidence without a closing time":   func(l *BranchLifetime) { l.ClosedAt = nil },
		"its own ID joined to it":           func(l *BranchLifetime) { l.Linked[0].ID = l.LedgerID },
		"a joined ID without a time":        func(l *BranchLifetime) { l.Linked[0].LinkedAt = time.Time{} },
		"a joined ID in another form":       func(l *BranchLifetime) { l.Linked[0].ID = "b" },
		"a reopening without its times": func(l *BranchLifetime) {
			l.Reopenings = []BranchReopening{{ClosedBy: *l.ClosedBy, ReopenedBy: l.OpenedBy}}
		},
	} {
		lifetime := valid()
		change(&lifetime)
		if err := lifetime.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	if ValidBranchLedgerID("") || !ValidBranchLedgerID(branchLedgerTestID("0")) || ValidateBranchLedgerName("main") != nil || ValidateBranchLedgerName("") == nil {
		t.Fatal("ID and name forms")
	}
}
