package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// capturePushStatusFailure is a server response carrying only its status, as
// the backend client's HTTP error does.
type capturePushStatusFailure int

func (failure capturePushStatusFailure) Error() string {
	return fmt.Sprintf("synthetic server status %d", int(failure))
}
func (failure capturePushStatusFailure) StatusCode() int { return int(failure) }

func queuedCapturePush(t *testing.T, text string) (frozenDriver, *commitCapturePass, *capturePushSyncSpy) {
	t.Helper()
	f := newCapturePushDriver(t)
	p := f.attempt(t, text, domain.DocumentIdentityLegacy, true)
	ctx := context.Background()
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(p)
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	spy := capturePushSpy(t, f, p)
	f.c.Sync = spy
	return f, p, spy
}

func makeCapturePushDue(t *testing.T, f frozenDriver) {
	t.Helper()
	rel, request, _ := readCapturePushRequest(t, f.cwd)
	request.Next = time.Time{}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfs.WriteRepoFileDurable(f.cwd, rel, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// An overloaded server says nothing about the publication. However long the
// outage lasts the request keeps its bounded retry budget, waits on the slow
// schedule, and is delivered by the first attempt after the server recovers.
func TestCapturePushServerOutageKeepsBudgetAndRecovers(t *testing.T) {
	f, _, spy := queuedCapturePush(t, "server-outage")
	ctx := context.Background()
	spy.failure = fmt.Errorf("history 0123 remains pending: %w", capturePushStatusFailure(503))
	started := time.Now()
	next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID)
	if err != nil || next.IsZero() {
		t.Fatal("outage left the request unscheduled", err)
	}
	_, first, _ := readCapturePushRequest(t, f.cwd)
	if first.Done || first.Tries != 0 || first.Deferred != 1 || first.Error == "" {
		t.Fatalf("outage consumed the retry budget: %+v", first)
	}
	if wait := first.Next.Sub(started); wait < 55*time.Second || wait > 2*time.Minute {
		t.Fatalf("first deferral waits %v, want about a minute", wait)
	}
	// Far more failed attempts than the bounded budget allows.
	for attempt := 2; attempt <= 3*capturePushMaxTries; attempt++ {
		makeCapturePushDue(t, f)
		before := time.Now()
		if _, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil {
			t.Fatal(err)
		}
		_, current, _ := readCapturePushRequest(t, f.cwd)
		if current.Done || current.Tries != 0 || current.exhausted() || current.Deferred != min(attempt, capturePushMaxDeferred) {
			t.Fatalf("attempt %d: outage exhausted or reset the request: %+v", attempt, current)
		}
		if wait := current.Next.Sub(before); wait < capturePushDeferral(current.Deferred)-5*time.Second || wait > 31*time.Minute {
			t.Fatalf("attempt %d waits %v", attempt, wait)
		}
	}
	if len(spy.inputs) != 3*capturePushMaxTries {
		t.Fatalf("transport attempts = %d", len(spy.inputs))
	}
	listed, err := listDeferredPushes(f.cwd)
	if err != nil || len(listed) != 1 || listed[0].Exhausted || listed[0].Deferred == 0 || listed[0].Tries != 0 {
		t.Fatalf("waiting request shown as exhausted: %+v %v", listed, err)
	}
	spy.failure = nil
	makeCapturePushDue(t, f)
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() || spy.accepted != 1 {
		t.Fatal("recovered server did not receive the publication", err)
	}
	if _, delivered, _ := readCapturePushRequest(t, f.cwd); !delivered.Done {
		t.Fatal("delivered request is not complete")
	}
}

// A verdict on the publication still uses the bounded budget, and a verdict
// after an outage starts the fast schedule again.
func TestCapturePushVerdictAfterOutageUsesBoundedBudget(t *testing.T) {
	f, _, spy := queuedCapturePush(t, "verdict-after-outage")
	ctx := context.Background()
	spy.failure = capturePushStatusFailure(502)
	if _, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	spy.failure = capturePushStatusFailure(409)
	makeCapturePushDue(t, f)
	started := time.Now()
	if _, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	_, refused, _ := readCapturePushRequest(t, f.cwd)
	if refused.Tries != 1 || refused.Deferred != 0 || refused.Next.Sub(started) > 10*time.Second {
		t.Fatalf("refusal did not use the bounded budget: %+v", refused)
	}
}

func TestCapturePushFailureClasses(t *testing.T) {
	timeout := &net.DNSError{Err: "no such host", Name: "api.invalid", IsNotFound: true}
	for _, transient := range []error{
		capturePushStatusFailure(429), capturePushStatusFailure(502), capturePushStatusFailure(503), capturePushStatusFailure(504),
		fmt.Errorf("document finalization remains durably queued as 0123 (no ref updates sent): %w", context.DeadlineExceeded),
		fmt.Errorf("worker stopped: %w", context.Canceled),
		fmt.Errorf("destination repository connection verification failed: %w", timeout),
	} {
		if !capturePushTransient(transient) {
			t.Errorf("%v must not use the retry budget", transient)
		}
	}
	for _, verdict := range []error{
		nil, domain.ErrSyncConflict, domain.ErrHashMismatch, errors.New("completed capture publication has not been recorded"),
		capturePushStatusFailure(400), capturePushStatusFailure(401), capturePushStatusFailure(404), capturePushStatusFailure(409), capturePushStatusFailure(500),
	} {
		if capturePushTransient(verdict) {
			t.Errorf("%v must use the retry budget", verdict)
		}
	}
	for status, busy := range map[int]bool{429: true, 502: true, 503: true, 504: true, 500: false, 409: false} {
		if capturePushServerBusy(capturePushStatusFailure(status)) != busy {
			t.Errorf("status %d busy = %v", status, !busy)
		}
	}
	if capturePushServerBusy(context.DeadlineExceeded) {
		t.Error("a local deadline is not a server response")
	}
	want := []time.Duration{time.Minute, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for n, wait := range want {
		if got := capturePushDeferral(n); got != wait {
			t.Errorf("deferral %d = %v, want %v", n, got, wait)
		}
	}
	if capturePushDeferral(capturePushMaxDeferred) != 30*time.Minute {
		t.Error("deferral ceiling moved")
	}
}

// Delivery is bound to the pushed commit. Once the branch points elsewhere the
// request can never be delivered, so it is retired without a transport attempt
// or a spent budget, and status no longer reports it as exhausted work.
func TestCapturePushMovedBranchRetiresRequest(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhausted=%v", exhausted), func(t *testing.T) {
			f, p, spy := queuedCapturePush(t, fmt.Sprintf("moved-branch-%v", exhausted))
			ctx := context.Background()
			rel, request, _ := readCapturePushRequest(t, f.cwd)
			if exhausted {
				request.Tries, request.Error = capturePushMaxTries, `Git branch "refs/heads/x" changed after push selection; context publication remains pending`
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				if err := providerfs.WriteRepoFileDurable(f.cwd, rel, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if capturePushBranchMoved(ctx, f.cwd, request.Intent.Update) {
				t.Fatal("branch at the pushed commit reported as moved")
			}
			runLifecycleGit(t, f.cwd, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "--allow-empty", "-m", "later work")
			if !capturePushBranchMoved(ctx, f.cwd, request.Intent.Update) {
				t.Fatal("moved branch not detected")
			}
			if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() {
				t.Fatal("retired request stayed scheduled", err)
			}
			_, retired, _ := readCapturePushRequest(t, f.cwd)
			if retired.Done || retired.Retired != capturePushSuperseded || retired.Error != "" || len(spy.inputs) != 0 {
				t.Fatalf("moved branch was not retired quietly: %+v", retired)
			}
			if !exhausted && retired.Tries != 0 {
				t.Fatal("retirement spent the retry budget")
			}
			listed, err := listDeferredPushes(f.cwd)
			if err != nil || len(listed) != 0 {
				t.Fatalf("retired request still listed: %+v %v", listed, err)
			}
			if count, err := countRetiredCapturePushes(f.cwd); err != nil || count != 1 {
				t.Fatalf("retired count = %d %v", count, err)
			}
			if n, err := rearmCapturePushes(ctx, f.cwd, p.Proof.Branch); err != nil || n != 0 {
				t.Fatalf("manual push re-armed a retired request: %d %v", n, err)
			}
			if _, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || len(spy.inputs) != 0 {
				t.Fatal("retired request reached transport", err)
			}
		})
	}
}

// Only certain knowledge retires a request: a ref that is not a branch, or a
// repository Git cannot read, proves nothing.
func TestCapturePushBranchMovedNeedsEvidence(t *testing.T) {
	f, _, _ := queuedCapturePush(t, "moved-evidence")
	ctx := context.Background()
	_, request, _ := readCapturePushRequest(t, f.cwd)
	update := request.Intent.Update
	head := update
	head.LocalRef = "HEAD"
	if capturePushBranchMoved(ctx, f.cwd, head) {
		t.Fatal("a non-branch local ref was judged")
	}
	if capturePushBranchMoved(ctx, t.TempDir(), update) {
		t.Fatal("an unreadable repository retired a request")
	}
	gone := update
	gone.LocalRef = update.LocalRef + "-deleted"
	if !capturePushBranchMoved(ctx, f.cwd, gone) {
		t.Fatal("a deleted branch still counts as deliverable")
	}
	upper := update
	upper.LocalOID = strings.ToUpper(update.LocalOID)
	if capturePushBranchMoved(ctx, f.cwd, upper) {
		t.Fatal("object ID comparison is case sensitive")
	}
}

func TestCapturePushRequestRejectsUnknownRetirement(t *testing.T) {
	request := capturePushRequest{}
	if !request.validRetries() {
		t.Fatal("empty request invalid")
	}
	request.Retired = "forgotten"
	if request.validRetries() {
		t.Fatal("unknown retirement reason accepted")
	}
	request.Retired, request.Deferred = capturePushSuperseded, capturePushMaxDeferred+1
	if request.validRetries() {
		t.Fatal("unbounded deferral count accepted")
	}
}
