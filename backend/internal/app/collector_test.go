package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestCollectorReportsSayWhereAPushCameFrom(t *testing.T) {
	service, storage, repo := anchoredSessionFixture(t)
	system := systemTestContext()
	alice, bob, viewer := anchoredActor("member-a"), anchoredActor("member-b"), anchoredActor("viewer")
	if err := storage.UpsertUser(system, domain.User{ID: "member-a", Username: "alice", Email: "alice@example.test", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	sha := func(digit string) string { return strings.Repeat(digit, 40) }
	read := func(who string) domain.PushOriginView {
		t.Helper()
		view, err := service.PushOrigins(anchoredActor(who), repo)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}

	// Nothing yet: empty lists, not missing ones.
	if view := read("viewer"); view.Collectors == nil || view.Pushes == nil || len(view.Collectors)+len(view.Pushes) != 0 {
		t.Fatalf("before anything was reported: %#v", view)
	}
	// The server saw two pushes on the Git host.
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := storage.ClaimRemoteGitSync(system, repo, "https://git.example.test/team/app", start.Add(-3*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	seen := []domain.RemoteBranchPush{{Branch: "main", SHA: sha("a"), SeenAt: start.Add(-2 * time.Hour)}, {Branch: "topic", SHA: sha("b"), SeenAt: start.Add(-time.Hour)}}
	if err := storage.RecordRemotePushes(system, repo, seen); err != nil {
		t.Fatal(err)
	}
	// No collector is known: both came from computers without cxt.
	if view := read("viewer"); len(view.Pushes) != 2 || view.Pushes[0].SHA != sha("b") || view.Pushes[0].State != domain.PushWithoutCollector || view.Pushes[1].State != domain.PushWithoutCollector {
		t.Fatalf("pushes while no collector is known: %#v", view.Pushes)
	}

	// Alice's laptop says that it made the push to main, and that its run failed.
	revision := func() domain.RepositoryRevision {
		t.Helper()
		current, err := storage.RepositoryRevision(system, repo)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	before := revision()
	pushedAt := start.Add(-2*time.Hour - 5*time.Second)
	report := domain.CollectorReport{Machine: "laptop", Version: "1.2.3", RunError: "the upload was refused",
		Pushes: []domain.CollectorPush{{Branch: "main", SHA: sha("a"), At: pushedAt.Unix()}}}
	if err := service.ReportCollector(alice, repo, report); err != nil {
		t.Fatal(err)
	}
	if after := revision(); after == before {
		t.Fatalf("a collector report left the repository revision at %+v", after)
	}
	heard := time.Now().UTC()
	own := read("member-a")
	if len(own.Collectors) != 1 || len(own.Pushes) != 2 {
		t.Fatalf("after the first report: %#v", own)
	}
	collector := own.Collectors[0]
	if collector.HeardAt.Before(start) || collector.HeardAt.After(heard) {
		t.Fatalf("the collector was heard at %v, outside %v..%v", collector.HeardAt, start, heard)
	}
	if want := (domain.PushCollector{By: "alice", Machine: "laptop", Version: "1.2.3", HeardAt: collector.HeardAt, Failing: true, RunError: "the upload was refused", Own: true}); collector != want {
		t.Fatalf("the collector as its owner reads it: %#v", collector)
	}
	zero := 0
	wantPushes := []domain.PushOrigin{
		// The laptop was heard from after this push and did not claim it.
		{Branch: "topic", SHA: sha("b"), At: seen[1].SeenAt, State: domain.PushWithoutCollector},
		// Claimed by a collector whose run failed; dated by the clone.
		{Branch: "main", SHA: sha("a"), At: pushedAt, State: domain.PushByFailingCollector, Collector: &zero, ClaimedAt: &collector.HeardAt},
	}
	if !reflect.DeepEqual(own.Pushes, wantPushes) {
		t.Fatalf("pushes after the first report:\n got %#v\nwant %#v", own.Pushes, wantPushes)
	}
	// Another member is told whose collector it is, and not what went wrong.
	if theirs := read("member-b"); len(theirs.Collectors) != 1 ||
		theirs.Collectors[0] != (domain.PushCollector{By: "alice", Machine: "laptop", Version: "1.2.3", HeardAt: collector.HeardAt, Failing: true}) || !reflect.DeepEqual(theirs.Pushes, wantPushes) {
		t.Fatalf("as another member reads it: %#v", theirs)
	}

	// Bob's collector, on a machine without a label, claims nothing. Then the
	// laptop's next run ends well: the push it claimed is clean from now on.
	if err := service.ReportCollector(bob, repo, domain.CollectorReport{Pushes: []domain.CollectorPush{}}); err != nil {
		t.Fatal(err)
	}
	report.RunError = ""
	if err := service.ReportCollector(alice, repo, report); err != nil {
		t.Fatal(err)
	}
	view := read("viewer")
	if len(view.Collectors) != 2 || view.Collectors[0].By != "alice" || view.Collectors[0].Failing || view.Collectors[1].By != "" || view.Collectors[1].Machine != "" || view.Collectors[1].Own {
		t.Fatalf("collectors by account, then machine: %#v", view.Collectors)
	}
	if len(view.Pushes) != 2 || view.Pushes[1].State != domain.PushByCollector || view.Pushes[1].Collector == nil || *view.Pushes[1].Collector != 0 ||
		!view.Pushes[1].ClaimedAt.Equal(collector.HeardAt) {
		t.Fatalf("the claimed push after a run that ended well: %#v", view.Pushes)
	}
	// A push the server sees after both collectors were last heard from is
	// not known yet: either of them may still claim it.
	// A collector that said nothing for longer than is waited for is taken to be gone.
	retired := domain.ReportedCollector{Account: "member-b", Machine: "retired", HeardAt: time.Now().UTC().Add(-domain.CollectorKnownFor - time.Hour)}
	if err := storage.PutCollectorReport(system, repo, retired, nil); err != nil {
		t.Fatal(err)
	}
	later := domain.RemoteBranchPush{Branch: "main", SHA: sha("c"), SeenAt: time.Now().UTC().Add(time.Minute).Truncate(time.Second)}
	if err := storage.RecordRemotePushes(system, repo, []domain.RemoteBranchPush{later}); err != nil {
		t.Fatal(err)
	}
	if view = read("viewer"); len(view.Pushes) != 3 || view.Pushes[0].SHA != sha("c") || view.Pushes[0].State != domain.PushOriginNotKnown || !reflect.DeepEqual(view.Pushes[0].Waiting, []int{0, 1}) {
		t.Fatalf("a push after every collector was last heard from: %#v", view.Pushes)
	}

	// Who may report: a member, for itself. A report that is not well formed
	// is refused and changes nothing.
	if err := service.ReportCollector(viewer, repo, report); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer's report: %v", err)
	}
	if err := service.ReportCollector(system, repo, report); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the server's own report: %v", err)
	}
	if err := service.ReportCollector(anchoredActor("stranger"), repo, report); !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a report of an account outside the repository: %v", err)
	}
	broken := report
	broken.Machine = "lap\ntop"
	if err := service.ReportCollector(alice, repo, broken); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a report with a machine label of two lines: %v", err)
	}
	// What is no repository identifier is refused where the repository is looked up.
	if err := service.ReportCollector(alice, "not-a-repository", report); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a report for what is no repository identifier: %v", err)
	}
	if _, err := service.PushOrigins(alice, "not-a-repository"); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("origins of what is no repository identifier: %v", err)
	}
	tooMany := report
	tooMany.Pushes = make([]domain.CollectorPush, domain.CollectorPushLimit+1)
	for index := range tooMany.Pushes {
		tooMany.Pushes[index] = domain.CollectorPush{Branch: "main", SHA: fmt.Sprintf("%040x", index+1), At: pushedAt.Unix()}
	}
	if err := service.ReportCollector(alice, repo, tooMany); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a report of more pushes than one may hold: %v", err)
	}
	if view = read("member-a"); len(view.Collectors) != 3 || len(view.Pushes) != 3 {
		t.Fatalf("after refused reports: %#v", view)
	}
}
