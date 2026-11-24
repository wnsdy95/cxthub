package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// checkCollectorPersistence is the contract both stores keep. other is a
// second repository that exists and is never written to.
func checkCollectorPersistence(t *testing.T, ctx context.Context, storage outbound.CollectorStore, repo, other domain.ContentHash) {
	t.Helper()
	sha := func(digit string) string { return strings.Repeat(digit, 40) }
	at := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	if listed, err := storage.ListReportedCollectors(ctx, repo); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no collector yet: %#v %v", listed, err)
	}
	if listed, err := storage.ListClaimedPushes(ctx, repo, 10); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no claimed push yet: %#v %v", listed, err)
	}

	laptop := domain.ReportedCollector{Account: "member-a", Machine: "laptop", Version: "1.0.0", RunError: "the upload was refused", HeardAt: at}
	mainA := domain.ClaimedPush{Branch: "main", SHA: sha("a"), Account: "member-a", Machine: "laptop", PushedAt: at.Add(-10 * time.Minute), ClaimedAt: at}
	topicB := domain.ClaimedPush{Branch: "topic", SHA: sha("b"), Account: "member-a", Machine: "laptop", PushedAt: at.Add(-5 * time.Minute), ClaimedAt: at}
	if err := storage.PutCollectorReport(ctx, repo, laptop, []domain.ClaimedPush{mainA, topicB}); err != nil {
		t.Fatal(err)
	}
	// Another computer of the same member claims a push that is claimed
	// already, and two of its own that were made at the same moment.
	desk := domain.ReportedCollector{Account: "member-a", Machine: "desk", HeardAt: at.Add(time.Minute)}
	again := domain.ClaimedPush{Branch: "main", SHA: sha("a"), Account: "member-a", Machine: "desk", PushedAt: at.Add(-time.Minute), ClaimedAt: at.Add(time.Minute), Clean: true}
	zebraC := domain.ClaimedPush{Branch: "zebra", SHA: sha("c"), Account: "member-a", Machine: "desk", PushedAt: at.Add(-20 * time.Minute), ClaimedAt: at.Add(time.Minute), Clean: true}
	alphaD := domain.ClaimedPush{Branch: "alpha", SHA: sha("d"), Account: "member-a", Machine: "desk", PushedAt: at.Add(-20 * time.Minute), ClaimedAt: at.Add(time.Minute), Clean: true}
	alphaC := domain.ClaimedPush{Branch: "alpha", SHA: sha("c"), Account: "member-a", Machine: "desk", PushedAt: at.Add(-20 * time.Minute), ClaimedAt: at.Add(time.Minute), Clean: true}
	// One of them is handed over with its times in another zone, and is listed in UTC like the rest.
	elsewhere := alphaD
	elsewhere.PushedAt, elsewhere.ClaimedAt = alphaD.PushedAt.In(time.FixedZone("elsewhere", 9*3600)), alphaD.ClaimedAt.In(time.FixedZone("elsewhere", 9*3600))
	if err := storage.PutCollectorReport(ctx, repo, desk, []domain.ClaimedPush{zebraC, again, elsewhere, alphaC}); err != nil {
		t.Fatal(err)
	}
	// A collector without a machine label, in another time zone, claiming nothing.
	bare := domain.ReportedCollector{Account: "member-b", HeardAt: at.Add(2 * time.Minute).In(time.FixedZone("elsewhere", 9*3600))}
	if err := storage.PutCollectorReport(ctx, repo, bare, nil); err != nil {
		t.Fatal(err)
	}
	bare.HeardAt = bare.HeardAt.UTC()
	if listed, err := storage.ListReportedCollectors(ctx, repo); err != nil || !reflect.DeepEqual(listed, []domain.ReportedCollector{desk, laptop, bare}) {
		t.Fatalf("collectors by account, then machine:\n got %#v\nwant %#v\n%v", listed, []domain.ReportedCollector{desk, laptop, bare}, err)
	}
	// The first claim of a push stays, and the push is clean once a claim was.
	mainA.Clean = true
	want := []domain.ClaimedPush{topicB, mainA, alphaC, alphaD, zebraC}
	if listed, err := storage.ListClaimedPushes(ctx, repo, 10); err != nil || !reflect.DeepEqual(listed, want) {
		t.Fatalf("claimed pushes, the one pushed last first, then by branch and commit:\n got %#v\nwant %#v\n%v", listed, want, err)
	}
	if listed, err := storage.ListClaimedPushes(ctx, repo, 2); err != nil || !reflect.DeepEqual(listed, want[:2]) {
		t.Fatalf("the two newest claimed pushes: %#v %v", listed, err)
	}
	if listed, err := storage.ListClaimedPushes(ctx, repo, 0); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("no claimed pushes asked for: %#v %v", listed, err)
	}

	// The same collector again replaces what it said; a clean claim makes a
	// push clean, and a later claim that is not clean changes nothing.
	laptop.Version, laptop.RunError, laptop.HeardAt = "1.0.1", "", at.Add(time.Hour)
	clean := topicB
	clean.Clean, clean.ClaimedAt = true, at.Add(time.Hour)
	if err := storage.PutCollectorReport(ctx, repo, laptop, []domain.ClaimedPush{clean}); err != nil {
		t.Fatal(err)
	}
	failed := topicB
	failed.ClaimedAt = at.Add(2 * time.Hour)
	laptop.RunError, laptop.HeardAt = "the session records could not be read", at.Add(2*time.Hour)
	if err := storage.PutCollectorReport(ctx, repo, laptop, []domain.ClaimedPush{failed}); err != nil {
		t.Fatal(err)
	}
	if listed, err := storage.ListReportedCollectors(ctx, repo); err != nil || !reflect.DeepEqual(listed, []domain.ReportedCollector{desk, laptop, bare}) {
		t.Fatalf("a collector that reported again:\n got %#v\n%v", listed, err)
	}
	topicB.Clean = true
	if listed, err := storage.ListClaimedPushes(ctx, repo, 1); err != nil || !reflect.DeepEqual(listed, []domain.ClaimedPush{topicB}) {
		t.Fatalf("a push claimed clean, then not clean:\n got %#v\nwant %#v\n%v", listed, topicB, err)
	}

	// What is not well formed is refused, and nothing of it is stored.
	refusedCollector := laptop
	refusedCollector.Version, refusedCollector.Account = "9.9.9", ""
	newClaim := domain.ClaimedPush{Branch: "refused", SHA: sha("e"), Account: "member-a", Machine: "laptop", PushedAt: at, ClaimedAt: at}
	badClaim := newClaim
	badClaim.SHA = "not-a-commit"
	laterVersion := laptop
	laterVersion.Version = "9.9.9"
	for name, write := range map[string]func() error{
		"a collector without an account": func() error {
			return storage.PutCollectorReport(ctx, repo, refusedCollector, []domain.ClaimedPush{newClaim})
		},
		"a claim without a commit number": func() error {
			return storage.PutCollectorReport(ctx, repo, laterVersion, []domain.ClaimedPush{newClaim, badClaim})
		},
		"a push claimed twice at once": func() error {
			return storage.PutCollectorReport(ctx, repo, laterVersion, []domain.ClaimedPush{newClaim, newClaim})
		},
	} {
		if err := write(); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s was stored: %v", name, err)
		}
	}
	if listed, err := storage.ListReportedCollectors(ctx, repo); err != nil || !reflect.DeepEqual(listed, []domain.ReportedCollector{desk, laptop, bare}) {
		t.Fatalf("collectors after refused writes:\n got %#v\n%v", listed, err)
	}
	if listed, err := storage.ListClaimedPushes(ctx, repo, 100); err != nil || len(listed) != 5 {
		t.Fatalf("claimed pushes after refused writes: %#v %v", listed, err)
	}
	// Another repository holds none of it.
	if listed, err := storage.ListReportedCollectors(ctx, other); err != nil || len(listed) != 0 {
		t.Fatalf("collectors of another repository: %#v %v", listed, err)
	}
	if listed, err := storage.ListClaimedPushes(ctx, other, 100); err != nil || len(listed) != 0 {
		t.Fatalf("claimed pushes of another repository: %#v %v", listed, err)
	}
}

// checkRecentRemotePushes is the contract both stores keep for the newest
// pushes the server saw.
func checkRecentRemotePushes(t *testing.T, ctx context.Context, storage remotePushTestStore, repo domain.ContentHash) {
	t.Helper()
	a, b := remoteGitStoreSHA("a"), remoteGitStoreSHA("b")
	if _, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(0), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if pushes, err := storage.ListRecentRemotePushes(ctx, repo, 10); err != nil || pushes == nil || len(pushes) != 0 {
		t.Fatalf("no push yet: %#v %v", pushes, err)
	}
	recorded := []domain.RemoteBranchPush{
		{Branch: "main", SHA: a, SeenAt: remoteGitStoreTime(10)},
		{Branch: "topic", SHA: a, SeenAt: remoteGitStoreTime(10)},
		{Branch: "gone", SHA: b, SeenAt: remoteGitStoreTime(20)},
		{Branch: "main", SHA: b, SeenAt: remoteGitStoreTime(30)},
	}
	if err := storage.RecordRemotePushes(ctx, repo, recorded); err != nil {
		t.Fatal(err)
	}
	want := []domain.RemoteBranchPush{recorded[3], recorded[2], recorded[1], recorded[0]}
	if pushes, err := storage.ListRecentRemotePushes(ctx, repo, 10); err != nil || !reflect.DeepEqual(pushes, want) {
		t.Fatalf("pushes of every branch, the one seen last first:\n got %+v\nwant %+v\n%v", pushes, want, err)
	}
	if pushes, err := storage.ListRecentRemotePushes(ctx, repo, 3); err != nil || !reflect.DeepEqual(pushes, want[:3]) {
		t.Fatalf("the three newest pushes: %+v %v", pushes, err)
	}
	if pushes, err := storage.ListRecentRemotePushes(ctx, repo, 0); err != nil || pushes == nil || len(pushes) != 0 {
		t.Fatalf("no pushes asked for: %#v %v", pushes, err)
	}
}

func TestFSCollectorPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	checkCollectorPersistence(t, context.Background(), storage, repo, domain.HashContent([]byte(t.Name()+" other")))
	// What is on disk is checked when it is read.
	for _, file := range []struct {
		path string
		read func() error
	}{
		{storage.reportedCollectorPath(repo), func() error { _, err := storage.ListReportedCollectors(context.Background(), repo); return err }},
		{storage.claimedPushPath(repo), func() error { _, err := storage.ListClaimedPushes(context.Background(), repo, 10); return err }},
	} {
		raw, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatal(err)
		}
		tampered := strings.Replace(string(raw), `"account":"member-a"`, `"account":""`, 1)
		if tampered == string(raw) {
			t.Fatalf("%s does not hold the account to change", file.path)
		}
		if err := os.WriteFile(file.path, []byte(tampered), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := file.read(); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("a malformed entry in %s: %v", file.path, err)
		}
	}
}

func TestFSRecentRemotePushes(t *testing.T) {
	checkRecentRemotePushes(t, context.Background(), NewFSStore(t.TempDir()), domain.HashContent([]byte(t.Name())))
}
