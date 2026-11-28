package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func collectorSHA(digit string) string { return strings.Repeat(digit, 40) }

func TestCollectorReportValidation(t *testing.T) {
	valid := CollectorReport{Machine: "laptop", Version: "1.2.3", RunError: "the session records could not be read",
		Pushes: []CollectorPush{{Branch: "main", SHA: collectorSHA("a"), At: 1_700_000_000}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (CollectorReport{}).Validate(); err != nil {
		t.Fatalf("a report that names nothing: %v", err)
	}
	many := make([]CollectorPush, CollectorPushLimit+1)
	for index := range many {
		many[index] = valid.Pushes[0]
	}
	if err := (CollectorReport{Pushes: many[:CollectorPushLimit]}).Validate(); err != nil {
		t.Fatalf("as many pushes as a report may hold: %v", err)
	}
	for name, change := range map[string]func(*CollectorReport){
		"a machine label with a line break": func(r *CollectorReport) { r.Machine = "lap\ntop" },
		"a version that is too long":        func(r *CollectorReport) { r.Version = strings.Repeat("9", collectorVersionLimit+1) },
		"an error that is too long":         func(r *CollectorReport) { r.RunError = strings.Repeat("e", CollectorErrorLimit+1) },
		"an error with a line break":        func(r *CollectorReport) { r.RunError = "first\nsecond" },
		"too many pushes":                   func(r *CollectorReport) { r.Pushes = many },
		"a push without a branch":           func(r *CollectorReport) { r.Pushes[0].Branch = "" },
		"a push with a short commit number": func(r *CollectorReport) { r.Pushes[0].SHA = "abc123" },
		"a push without a time":             func(r *CollectorReport) { r.Pushes[0].At = 0 },
	} {
		report := valid
		report.Pushes = append([]CollectorPush{}, valid.Pushes...)
		change(&report)
		if err := report.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was taken: %v", name, err)
		}
	}
}

func TestStoredCollectorAndClaimValidation(t *testing.T) {
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	collector := ReportedCollector{Account: "member-a", Machine: "laptop", Version: "1.2.3", HeardAt: at}
	if err := collector.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ReportedCollector){
		"no account":         func(c *ReportedCollector) { c.Account = "" },
		"no time":            func(c *ReportedCollector) { c.HeardAt = time.Time{} },
		"a bad machine":      func(c *ReportedCollector) { c.Machine = " laptop" },
		"an error too long":  func(c *ReportedCollector) { c.RunError = strings.Repeat("e", CollectorErrorLimit+1) },
		"a version too long": func(c *ReportedCollector) { c.Version = strings.Repeat("9", collectorVersionLimit+1) },
	} {
		changed := collector
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("a collector with %s was taken: %v", name, err)
		}
	}
	claim := ClaimedPush{Branch: "main", SHA: collectorSHA("a"), Account: "member-a", Machine: "laptop", PushedAt: at, ClaimedAt: at}
	if err := claim.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ClaimedPush){
		"no branch":       func(p *ClaimedPush) { p.Branch = "" },
		"a bad commit":    func(p *ClaimedPush) { p.SHA = "abc" },
		"no account":      func(p *ClaimedPush) { p.Account = "" },
		"a bad machine":   func(p *ClaimedPush) { p.Machine = "lap\ttop" },
		"no push time":    func(p *ClaimedPush) { p.PushedAt = time.Time{} },
		"no time claimed": func(p *ClaimedPush) { p.ClaimedAt = time.Time{} },
	} {
		changed := claim
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("a claimed push with %s was taken: %v", name, err)
		}
	}
}

// What a report becomes when it is heard: the collector as it is now, and one
// claim for each push, clean only when the report says nothing went wrong.
func TestCollectorReportHeard(t *testing.T) {
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.FixedZone("elsewhere", 9*3600))
	report := CollectorReport{Machine: "laptop", Version: "1.2.3", Pushes: []CollectorPush{
		{Branch: "main", SHA: collectorSHA("a"), At: 1_700_000_100}, {Branch: "topic", SHA: collectorSHA("b"), At: 1_700_000_000}}}
	collector, claims := report.Heard("member-a", at)
	if want := (ReportedCollector{Account: "member-a", Machine: "laptop", Version: "1.2.3", HeardAt: at.UTC()}); !reflect.DeepEqual(collector, want) {
		t.Fatalf("collector %#v, want %#v", collector, want)
	}
	want := []ClaimedPush{
		{Branch: "main", SHA: collectorSHA("a"), Account: "member-a", Machine: "laptop", PushedAt: time.Unix(1_700_000_100, 0).UTC(), ClaimedAt: at.UTC(), Clean: true},
		{Branch: "topic", SHA: collectorSHA("b"), Account: "member-a", Machine: "laptop", PushedAt: time.Unix(1_700_000_000, 0).UTC(), ClaimedAt: at.UTC(), Clean: true},
	}
	if !reflect.DeepEqual(claims, want) {
		t.Fatalf("claims\n got %#v\nwant %#v", claims, want)
	}
	report.RunError = "the server refused the upload"
	collector, claims = report.Heard("member-a", at)
	if collector.RunError != report.RunError || claims[0].Clean || claims[1].Clean {
		t.Fatalf("a report of a failed run: %#v %#v", collector, claims)
	}
	// The same commit pushed to the same branch three times is claimed once,
	// at the earliest time, wherever that time stands in the report.
	again := CollectorReport{Pushes: []CollectorPush{
		{Branch: "main", SHA: collectorSHA("a"), At: 1_700_000_200}, {Branch: "main", SHA: collectorSHA("a"), At: 1_700_000_050},
		{Branch: "topic", SHA: collectorSHA("a"), At: 1_700_000_300}, {Branch: "main", SHA: collectorSHA("a"), At: 1_700_000_100}}}
	_, claims = again.Heard("member-a", at)
	if len(claims) != 2 || claims[0].Branch != "main" || !claims[0].PushedAt.Equal(time.Unix(1_700_000_050, 0)) ||
		claims[1].Branch != "topic" || !claims[1].PushedAt.Equal(time.Unix(1_700_000_300, 0)) {
		t.Fatalf("a commit pushed to a branch more than once: %#v", claims)
	}
	if _, none := (CollectorReport{}).Heard("member-a", at); none == nil || len(none) != 0 {
		t.Fatalf("a report without pushes: %#v", none)
	}
}

// A push that is claimed again keeps who claimed it first and becomes clean
// once; it never stops being clean.
func TestMergeClaimedPush(t *testing.T) {
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	first := ClaimedPush{Branch: "main", SHA: collectorSHA("a"), Account: "member-a", Machine: "laptop", PushedAt: at, ClaimedAt: at.Add(time.Minute)}
	later := ClaimedPush{Branch: "main", SHA: collectorSHA("a"), Account: "member-b", Machine: "desk", PushedAt: at.Add(time.Hour), ClaimedAt: at.Add(2 * time.Hour), Clean: true}
	merged := MergeClaimedPush(first, later)
	want := first
	want.Clean = true
	if merged != want {
		t.Fatalf("a failed claim, then a clean one: %#v", merged)
	}
	later.Clean = false
	if again := MergeClaimedPush(merged, later); again != want {
		t.Fatalf("a clean claim, then a failed one: %#v", again)
	}
	if still := MergeClaimedPush(first, later); still != first {
		t.Fatalf("two failed claims: %#v", still)
	}
}

func TestPushOrigins(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	hour := func(hours int) time.Time { return now.Add(time.Duration(hours) * time.Hour) }
	index := func(value int) *int { return &value }
	moment := func(at time.Time) *time.Time { return &at }
	collectors := []ReportedCollector{
		{Account: "member-a", Machine: "laptop", HeardAt: hour(-1)},
		{Account: "member-a", Machine: "desk", HeardAt: hour(-30), RunError: "the session records could not be read"},
		{Account: "member-b", HeardAt: now.Add(-CollectorKnownFor - time.Minute)},
	}
	seen := []RemoteBranchPush{
		// Claimed below: the server's sighting adds nothing.
		{Branch: "main", SHA: collectorSHA("a"), SeenAt: hour(-40)},
		// After every collector that is still waited for was heard from.
		{Branch: "main", SHA: collectorSHA("c"), SeenAt: hour(-31)},
		// The desk has said nothing since; the laptop has.
		{Branch: "topic", SHA: collectorSHA("d"), SeenAt: hour(-2)},
		// Exactly when the laptop was heard from: not after it.
		{Branch: "topic", SHA: collectorSHA("e"), SeenAt: hour(-1)},
		// Seen twice: said once.
		{Branch: "topic", SHA: collectorSHA("d"), SeenAt: hour(-3)},
	}
	claims := []ClaimedPush{
		{Branch: "main", SHA: collectorSHA("a"), Account: "member-a", Machine: "laptop", PushedAt: hour(-41), ClaimedAt: hour(-40), Clean: true},
		{Branch: "main", SHA: collectorSHA("b"), Account: "member-a", Machine: "desk", PushedAt: hour(-35), ClaimedAt: hour(-30)},
		// Claimed by a collector that is no longer stored.
		{Branch: "old", SHA: collectorSHA("f"), Account: "member-c", Machine: "gone", PushedAt: hour(-50), ClaimedAt: hour(-50), Clean: true},
	}
	want := []PushOrigin{
		{Branch: "topic", SHA: collectorSHA("e"), At: hour(-1), State: PushOriginNotKnown, Waiting: []int{0, 1}},
		{Branch: "topic", SHA: collectorSHA("d"), At: hour(-2), State: PushOriginNotKnown, Waiting: []int{1}},
		{Branch: "main", SHA: collectorSHA("c"), At: hour(-31), State: PushWithoutCollector},
		{Branch: "main", SHA: collectorSHA("b"), At: hour(-35), State: PushByFailingCollector, Collector: index(1), ClaimedAt: moment(hour(-30))},
		{Branch: "main", SHA: collectorSHA("a"), At: hour(-41), State: PushByCollector, Collector: index(0), ClaimedAt: moment(hour(-40))},
		{Branch: "old", SHA: collectorSHA("f"), At: hour(-50), State: PushByCollector, ClaimedAt: moment(hour(-50))},
	}
	if got := PushOrigins(seen, claims, collectors, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("origins\n got %#v\nwant %#v", got, want)
	}
	// A collector is waited for until CollectorKnownFor has passed, and not after.
	collectors[2].HeardAt = now.Add(-CollectorKnownFor)
	if got := PushOrigins(seen[1:2], nil, collectors, now); len(got) != 1 || got[0].State != PushOriginNotKnown || !reflect.DeepEqual(got[0].Waiting, []int{2}) {
		t.Fatalf("a collector heard from exactly as long ago as is waited: %#v", got)
	}
	// Without any collector every push the server saw is one without cxt.
	if got := PushOrigins(seen[2:3], nil, nil, now); len(got) != 1 || got[0].State != PushWithoutCollector || got[0].Waiting != nil {
		t.Fatalf("no collector at all: %#v", got)
	}
	// The newest are said, and no more than the limit.
	many := make([]RemoteBranchPush, PushOriginLimit+10)
	for position := range many {
		many[position] = RemoteBranchPush{Branch: "main", SHA: fmt.Sprintf("%040x", position), SeenAt: now.Add(time.Duration(position) * time.Second)}
	}
	got := PushOrigins(many, nil, nil, now)
	if len(got) != PushOriginLimit || !got[0].At.Equal(many[len(many)-1].SeenAt) || !got[PushOriginLimit-1].At.Equal(many[10].SeenAt) {
		t.Fatalf("%d origins of %d pushes, first %v, last %v", len(got), len(many), got[0].At, got[len(got)-1].At)
	}
	if none := PushOrigins(nil, nil, nil, now); none == nil || len(none) != 0 {
		t.Fatalf("no pushes: %#v", none)
	}
}

// Who is told what of a collector: its owner everything, another member not
// what went wrong, and a reader who is not signed in only when it was heard
// from and whether it fails.
func TestPushCollectors(t *testing.T) {
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.FixedZone("elsewhere", 9*3600))
	stored := []ReportedCollector{
		{Account: "member-a", Machine: "laptop", Version: "1.2.3", RunError: "the upload was refused", HeardAt: at},
		{Account: "member-b", Machine: "desk", Version: "1.2.2", HeardAt: at.Add(time.Hour)},
	}
	names := map[string]string{"member-a": "alice"}
	want := []PushCollector{
		{By: "alice", Machine: "laptop", Version: "1.2.3", HeardAt: at.UTC(), Failing: true, RunError: "the upload was refused", Own: true},
		{Machine: "desk", Version: "1.2.2", HeardAt: at.Add(time.Hour).UTC()},
	}
	if got := PushCollectors(stored, "member-a", names); !reflect.DeepEqual(got, want) {
		t.Fatalf("to its owner\n got %#v\nwant %#v", got, want)
	}
	want[0].RunError, want[0].Own = "", false
	if got := PushCollectors(stored, "member-c", names); !reflect.DeepEqual(got, want) {
		t.Fatalf("to another member\n got %#v\nwant %#v", got, want)
	}
	want = []PushCollector{{HeardAt: at.UTC(), Failing: true}, {HeardAt: at.Add(time.Hour).UTC()}}
	if got := PushCollectors(stored, "", names); !reflect.DeepEqual(got, want) {
		t.Fatalf("to a reader who is not signed in\n got %#v\nwant %#v", got, want)
	}
	if none := PushCollectors(nil, "member-a", names); none == nil || len(none) != 0 {
		t.Fatalf("no collectors: %#v", none)
	}
}
