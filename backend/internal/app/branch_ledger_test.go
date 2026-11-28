package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func branchLedgerTestID(letter string) string { return strings.Repeat(letter, 32) }

func TestBranchLedgerRegistrationFollowsTheOneRule(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	a, b := branchLedgerTestID("a"), branchLedgerTestID("b")

	// The first to register a name opens its lifetime with the ID it keeps.
	results, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "topic", LocalID: a}, {Name: "main", LocalID: b}})
	if err != nil || len(results) != 2 || results[0] != (domain.BranchRegistrationResult{Name: "topic", LocalID: a, LedgerID: a, Outcome: domain.BranchLedgerOpened}) ||
		results[1].Outcome != domain.BranchLedgerOpened || results[1].LedgerID != b {
		t.Fatalf("first registration: %+v %v", results, err)
	}
	// A teammate's own ID for the same name joins that lifetime.
	c := branchLedgerTestID("c")
	results, err = service.RegisterBranchIDs(bob, repo, []domain.BranchRegistration{{Name: "topic", LocalID: c}})
	if err != nil || results[0] != (domain.BranchRegistrationResult{Name: "topic", LocalID: c, LedgerID: a, Outcome: domain.BranchLedgerJoined}) {
		t.Fatalf("a teammate's ID: %+v %v", results, err)
	}
	// The same report again changes nothing.
	results, err = service.RegisterBranchIDs(bob, repo, []domain.BranchRegistration{{Name: "topic", LocalID: c}})
	if err != nil || results[0].Outcome != domain.BranchLedgerUnchanged || results[0].LedgerID != a {
		t.Fatalf("the same report again: %+v %v", results, err)
	}
	lifetimes, err := service.ListBranchLifetimes(alice, repo, "topic")
	if err != nil || len(lifetimes) != 1 || lifetimes[0].LedgerID != a || !lifetimes[0].Holds(c) || !lifetimes[0].Alive() ||
		lifetimes[0].OpenedBy != (domain.BranchEvidence{Kind: domain.BranchEvidenceRegistered, Account: "member-a"}) ||
		lifetimes[0].Linked[0].By.Account != "member-b" {
		t.Fatalf("the lifetime of the name: %+v %v", lifetimes, err)
	}
	if all, err := service.ListBranchLifetimes(bob, repo, ""); err != nil || len(all) != 2 {
		t.Fatalf("every lifetime: %+v %v", all, err)
	}
	// One ID that is another living branch's identity: the second name gets its own.
	results, err = service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "second-name", LocalID: a}})
	if err != nil || results[0].Outcome != domain.BranchLedgerSeparate || results[0].LedgerID == a || !domain.ValidBranchLedgerID(results[0].LedgerID) {
		t.Fatalf("one ID under two names: %+v %v", results, err)
	}
}

func TestBranchLedgerRefusesWhatIsNotAMembersReport(t *testing.T) {
	service, _, repo := anchoredSessionFixture(t)
	alice := anchoredActor("member-a")
	a := branchLedgerTestID("a")
	valid := []domain.BranchRegistration{{Name: "topic", LocalID: a}}
	if _, err := service.RegisterBranchIDs(anchoredActor("viewer"), repo, valid); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer: %v", err)
	}
	if _, err := service.RegisterBranchIDs(anchoredActor("stranger"), repo, valid); !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an account outside the repository: %v", err)
	}
	if _, err := service.RegisterBranchIDs(systemTestContext(), repo, valid); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the server itself: %v", err)
	}
	for name, branches := range map[string][]domain.BranchRegistration{
		"a name that is not a branch name": {{Name: "topic..one", LocalID: a}},
		"an ID in another form":            {{Name: "topic", LocalID: "legacy-" + a[:25]}},
		"no ID":                            {{Name: "topic"}},
		"too many branches":                make([]domain.BranchRegistration, domain.BranchRegistrationLimit+1),
	} {
		if _, err := service.RegisterBranchIDs(alice, repo, branches); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := service.ListBranchLifetimes(alice, repo, "not a name"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an invalid name filter: %v", err)
	}
	// A refused report stored nothing.
	if lifetimes, err := service.ListBranchLifetimes(alice, repo, ""); err != nil || len(lifetimes) != 0 {
		t.Fatalf("after refused reports: %+v %v", lifetimes, err)
	}
	// Two local branches that track one name are two IDs for it: the first
	// opens the lifetime, the second joins it.
	results, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "shared", LocalID: a}, {Name: "shared", LocalID: branchLedgerTestID("b")}})
	if err != nil || len(results) != 2 || results[0].Outcome != domain.BranchLedgerOpened || results[1].Outcome != domain.BranchLedgerJoined || results[1].LedgerID != a {
		t.Fatalf("two IDs under one name in one report: %+v %v", results, err)
	}
	// An empty report is a report.
	if results, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{}); err != nil || len(results) != 0 {
		t.Fatalf("an empty report: %+v %v", results, err)
	}
}

// A push the hosting provider delivers shows a branch name alive or gone. The
// ledger learns it in the transaction that stores the observation.
func TestBranchLedgerFollowsPushObservations(t *testing.T) {
	service, storage, repo := anchoredSessionFixture(t)
	ctx := systemTestContext()
	const origin = "https://github.com/example/ledger"
	stored, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	stored.GitRemoteURL = origin
	if _, err := storage.PutRepo(ctx, stored); err != nil {
		t.Fatal(err)
	}
	alice := anchoredActor("member-a")
	// The webhook receiver passes a ref's absence as an empty commit number.
	one, two, gone := strings.Repeat("1", 40), strings.Repeat("2", 40), ""
	lifetimes := func(name string) []domain.BranchLifetime {
		t.Helper()
		listed, err := service.ListBranchLifetimes(alice, repo, name)
		if err != nil {
			t.Fatal(err)
		}
		return listed
	}

	// A name nobody registered: the server makes the ID when it first sees it.
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/from-the-web", "", one, false, "delivery-1"); err != nil {
		t.Fatal(err)
	}
	seen := lifetimes("from-the-web")
	if len(seen) != 1 || !seen[0].Alive() || !domain.ValidBranchLedgerID(seen[0].LedgerID) || seen[0].OpenedBy.Kind != domain.BranchEvidenceHostingPush || seen[0].OpenedBy.Source == "" {
		t.Fatalf("first sight: %+v", seen)
	}
	// Later pushes to the name, forced or not, keep its ID.
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/from-the-web", one, two, true, "delivery-2"); err != nil {
		t.Fatal(err)
	}
	if again := lifetimes("from-the-web"); len(again) != 1 || again[0].LedgerID != seen[0].LedgerID {
		t.Fatalf("after a forced push: %+v", again)
	}
	// A member's CLI registering afterwards joins the server's lifetime.
	a := branchLedgerTestID("a")
	if results, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "from-the-web", LocalID: a}}); err != nil || results[0].Outcome != domain.BranchLedgerJoined || results[0].LedgerID != seen[0].LedgerID {
		t.Fatalf("registering after the server: %+v %v", results, err)
	}
	// The push that leaves the name without a commit closes the lifetime.
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/from-the-web", two, gone, false, "delivery-3"); err != nil {
		t.Fatal(err)
	}
	closed := lifetimes("from-the-web")
	if len(closed) != 1 || closed[0].Alive() || closed[0].ClosedBy.Kind != domain.BranchEvidenceHostingPush || !closed[0].Holds(a) {
		t.Fatalf("after the deletion: %+v", closed)
	}
	// The same name again is another lifetime.
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/from-the-web", "", one, false, "delivery-4"); err != nil {
		t.Fatal(err)
	}
	reused := lifetimes("from-the-web")
	if len(reused) != 2 || reused[0].Alive() || !reused[1].Alive() || reused[0].LedgerID == reused[1].LedgerID {
		t.Fatalf("a reused name: %+v", reused)
	}
	// The deletion delivered again does not close the new lifetime.
	if _, err := service.ObserveRemotePush(ctx, origin, "refs/heads/from-the-web", two, gone, false, "delivery-3"); err != nil {
		t.Fatal(err)
	}
	if after := lifetimes("from-the-web"); len(after) != 2 || !after[1].Alive() {
		t.Fatalf("a deletion processed twice: %+v", after)
	}
	// An observation of something that is not a branch, and of a name the
	// ledger cannot hold, leave the ledger as it was.
	for _, ref := range []string{"refs/tags/v1", "refs/heads/" + strings.Repeat("n", 300)} {
		observation := domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "github-push", Ref: ref, After: one, Delivery: "delivery-5"}.WithID()
		if err := recordBranchLedgerObservation(ctx, storage, observation, time.Now().UTC()); err != nil {
			t.Fatalf("%s: %v", ref[:16], err)
		}
	}
	if all := lifetimes(""); len(all) != 2 {
		t.Fatalf("after observations the ledger does not hold: %+v", all)
	}
	// Registering the closed lifetime's ID under a free name continues that lifetime.
	results, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "came-back", LocalID: a}})
	if err != nil || results[0].Outcome != domain.BranchLedgerReopened || results[0].LedgerID != closed[0].LedgerID {
		t.Fatalf("the closed lifetime's ID again: %+v %v", results, err)
	}
	back := lifetimes("came-back")
	if len(back) != 1 || !back[0].Alive() || len(back[0].Reopenings) != 1 || back[0].Reopenings[0].Name != "from-the-web" {
		t.Fatalf("the continued lifetime: %+v", back)
	}
}

func TestBranchLedgerObservationWithoutALedgerStoreIsSkipped(t *testing.T) {
	observation := domain.GitRefObservation{RepoID: hh(t.Name()), Ref: "refs/heads/main", After: strings.Repeat("1", 40)}.WithID()
	if err := recordBranchLedgerObservation(context.Background(), struct{}{}, observation, time.Now()); err != nil {
		t.Fatalf("a store without a ledger: %v", err)
	}
}

// The GitHub App's push deliveries feed the ledger the same way, with the
// stored observation as the evidence.
func TestBranchLedgerFollowsQualifiedPushDeliveries(t *testing.T) {
	worker, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	zero, head := strings.Repeat("0", 40), strings.Repeat("c", 40)
	push := func(delivery, before, after string) {
		t.Helper()
		body := []byte(`{"installation":{"id":789},"repository":{"id":91},"ref":"refs/heads/topic","before":"` + before + `","after":"` + after + `"}`)
		if err := worker.Receive(ctx, delivery, "push", body); err != nil {
			t.Fatal(err)
		}
		if err := githubWorkerOf(worker).deliver(ctx, githubHash(delivery)); err != nil {
			t.Fatal(err)
		}
		// A delivery processed again changes nothing more.
		if err := githubWorkerOf(worker).applyDelivery(ctx, domain.GitHubDelivery{ID: githubHash(delivery), Kind: "push", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	push("ledger-created", zero, head)
	lifetimes, err := storage.ListBranchLifetimes(ctx, repo.ID, "topic")
	if err != nil || len(lifetimes) != 1 || !lifetimes[0].Alive() || lifetimes[0].OpenedBy.Kind != domain.BranchEvidenceHostingPush {
		t.Fatalf("after the creating push: %+v %v", lifetimes, err)
	}
	// The evidence names the push that showed the name.
	if lifetimes[0].OpenedBy.Source == "" {
		t.Fatalf("the push behind the lifetime: %+v", lifetimes[0].OpenedBy)
	}
	push("ledger-deleted", head, zero)
	closed, err := storage.ListBranchLifetimes(ctx, repo.ID, "topic")
	if err != nil || len(closed) != 1 || closed[0].Alive() || closed[0].LedgerID != lifetimes[0].LedgerID || closed[0].ClosedBy.Source == lifetimes[0].OpenedBy.Source {
		t.Fatalf("after the deleting push: %+v %v", closed, err)
	}
	push("ledger-created-again", zero, head)
	if again, err := storage.ListBranchLifetimes(ctx, repo.ID, "topic"); err != nil || len(again) != 2 || !again[1].Alive() || again[1].LedgerID == closed[0].LedgerID {
		t.Fatalf("after the name came back: %+v %v", again, err)
	}
}
