package domain

import (
	"fmt"
	"sort"
	"time"
)

// A collector is the program of cxt that uploads sessions from one computer
// without being asked. What it tells the server about itself answers one
// question about a push that came without a session: was it made on a
// computer where cxt does not run, or on one where cxt runs and failed.
//
// Git records in a clone each push the clone made. A collector reports those
// pushes, so a push some collector named was made where cxt runs, and a push
// no collector names was not, once every collector the server knows was heard
// from after it.

const (
	// CollectorPushLimit bounds the pushes of one report.
	CollectorPushLimit = 200
	// CollectorErrorLimit bounds what a collector says went wrong, in bytes.
	CollectorErrorLimit   = 512
	collectorVersionLimit = 64
	// CollectorKnownFor is how long a collector that says nothing is still
	// waited for. Afterwards the computer is taken to be gone.
	CollectorKnownFor = 30 * 24 * time.Hour
	// PushOriginLimit bounds the pushes one answer says the origin of.
	PushOriginLimit = 500
)

// CollectorPush is a push as the clone recorded it. At is the clone's own
// clock, in seconds.
type CollectorPush struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	At     int64  `json:"at"`
}

// CollectorReport is what a collector sends: the newest pushes of its clone,
// and what went wrong in its run, empty when nothing did.
type CollectorReport struct {
	Machine  string          `json:"machine,omitempty"`
	Version  string          `json:"cxt_version,omitempty"`
	RunError string          `json:"run_error,omitempty"`
	Pushes   []CollectorPush `json:"pushes"`
}

func collectorInvalid(what string) error {
	return fmt.Errorf("%w: collector report: %s", ErrValidation, what)
}

// Validate checks a report a CLI sent.
func (r CollectorReport) Validate() error {
	if !anchoredText(r.Machine, anchoredNameLimit, false) {
		return collectorInvalid("machine label is not plain text")
	}
	if !anchoredText(r.Version, collectorVersionLimit, false) || !anchoredText(r.RunError, CollectorErrorLimit, false) {
		return collectorInvalid("version or error is not plain text")
	}
	if len(r.Pushes) > CollectorPushLimit {
		return collectorInvalid("too many pushes")
	}
	for _, push := range r.Pushes {
		if (RemoteBranch{Name: push.Branch, SHA: push.SHA}).Validate() != nil || push.At <= 0 {
			return collectorInvalid("a push needs its branch, its commit and its time")
		}
	}
	return nil
}

// ReportedCollector is the last thing one collector said. A member's
// collectors are told apart by the machine's label.
type ReportedCollector struct {
	Account  string    `json:"account"`
	Machine  string    `json:"machine,omitempty"`
	Version  string    `json:"cxt_version,omitempty"`
	RunError string    `json:"run_error,omitempty"`
	HeardAt  time.Time `json:"heard_at"`
}

// Validate checks a collector before it is stored.
func (c ReportedCollector) Validate() error {
	if !anchoredText(c.Account, anchoredNameLimit, true) || c.HeardAt.IsZero() ||
		(CollectorReport{Machine: c.Machine, Version: c.Version, RunError: c.RunError}).Validate() != nil {
		return fmt.Errorf("%w: stored collector", ErrValidation)
	}
	return nil
}

// ClaimedPush is a push a collector said its clone made. Clean is true once
// a run of that collector ended well after the push: whatever session there
// was of it has been uploaded since.
type ClaimedPush struct {
	Branch    string    `json:"branch"`
	SHA       string    `json:"sha"`
	Account   string    `json:"account"`
	Machine   string    `json:"machine,omitempty"`
	PushedAt  time.Time `json:"pushed_at"`
	ClaimedAt time.Time `json:"claimed_at"`
	Clean     bool      `json:"clean"`
}

// Validate checks a claimed push before it is stored.
func (p ClaimedPush) Validate() error {
	if (RemoteBranch{Name: p.Branch, SHA: p.SHA}).Validate() != nil || !anchoredText(p.Account, anchoredNameLimit, true) ||
		!anchoredText(p.Machine, anchoredNameLimit, false) || p.PushedAt.IsZero() || p.ClaimedAt.IsZero() {
		return fmt.Errorf("%w: claimed push", ErrValidation)
	}
	return nil
}

// Heard turns a report an account sent at a moment into what is stored of
// it: the collector as it is now, and the pushes it claims. A clone that
// pushed the same commit to the same branch more than once claims it once, at
// the earliest of those times.
func (r CollectorReport) Heard(account string, at time.Time) (ReportedCollector, []ClaimedPush) {
	at = at.UTC()
	collector := ReportedCollector{Account: account, Machine: r.Machine, Version: r.Version, RunError: r.RunError, HeardAt: at}
	claims := make([]ClaimedPush, 0, len(r.Pushes))
	named := make(map[[2]string]int, len(r.Pushes))
	for _, push := range r.Pushes {
		pushed := time.Unix(push.At, 0).UTC()
		if index, again := named[[2]string{push.Branch, push.SHA}]; again {
			if pushed.Before(claims[index].PushedAt) {
				claims[index].PushedAt = pushed
			}
			continue
		}
		named[[2]string{push.Branch, push.SHA}] = len(claims)
		claims = append(claims, ClaimedPush{Branch: push.Branch, SHA: push.SHA, Account: account, Machine: r.Machine,
			PushedAt: pushed, ClaimedAt: at, Clean: r.RunError == ""})
	}
	return collector, claims
}

// MergeClaimedPush is the one rule for a push that is claimed again. Who
// claimed it first, and when, stays. It becomes clean when the later claim is
// clean, and never the other way round.
func MergeClaimedPush(stored, later ClaimedPush) ClaimedPush {
	stored.Clean = stored.Clean || later.Clean
	return stored
}

// PushOriginState says where a push came from, as far as the server knows.
type PushOriginState string

const (
	// PushByCollector: a computer where cxt runs made the push, and its
	// collector has ended a run well since.
	PushByCollector PushOriginState = "cxt"
	// PushByFailingCollector: a computer where cxt runs made the push, and
	// its collector has reported only failed runs since.
	PushByFailingCollector PushOriginState = "cxt_error"
	// PushWithoutCollector: no collector claims the push, and every
	// collector the server knows was heard from after it.
	PushWithoutCollector PushOriginState = "no_cxt"
	// PushOriginNotKnown: no collector claims the push yet, and a collector
	// the server knows has not been heard from since.
	PushOriginNotKnown PushOriginState = "unknown"
)

// PushOrigin is one push and where it came from. Collector and Waiting are
// positions in the list of collectors the origins were computed with.
type PushOrigin struct {
	Branch string          `json:"branch"`
	SHA    string          `json:"sha"`
	At     time.Time       `json:"at"`
	State  PushOriginState `json:"state"`
	// Collector is the collector that claimed the push.
	Collector *int       `json:"collector,omitempty"`
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// Waiting names the collectors that have not been heard from since.
	Waiting []int `json:"waiting,omitempty"`
}

// PushOrigins says where the pushes came from: the pushes collectors claimed
// and the pushes the server saw on the Git host, each once, the newest first.
// A claimed push is dated by the clone that made it, another one by the
// moment the server saw it.
func PushOrigins(seen []RemoteBranchPush, claims []ClaimedPush, collectors []ReportedCollector, now time.Time) []PushOrigin {
	type key struct{ branch, sha string }
	type owner struct{ account, machine string }
	position := make(map[owner]int, len(collectors))
	for index, collector := range collectors {
		position[owner{collector.Account, collector.Machine}] = index
	}
	origins := make([]PushOrigin, 0, len(seen)+len(claims))
	claimed := make(map[key]struct{}, len(claims))
	for _, claim := range claims {
		if _, twice := claimed[key{claim.Branch, claim.SHA}]; twice {
			continue
		}
		claimed[key{claim.Branch, claim.SHA}] = struct{}{}
		at := claim.ClaimedAt.UTC()
		origin := PushOrigin{Branch: claim.Branch, SHA: claim.SHA, At: claim.PushedAt.UTC(), State: PushByFailingCollector, ClaimedAt: &at}
		if claim.Clean {
			origin.State = PushByCollector
		}
		if index, known := position[owner{claim.Account, claim.Machine}]; known {
			origin.Collector = &index
		}
		origins = append(origins, origin)
	}
	for _, push := range seen {
		if _, done := claimed[key{push.Branch, push.SHA}]; done {
			continue
		}
		claimed[key{push.Branch, push.SHA}] = struct{}{}
		origin := PushOrigin{Branch: push.Branch, SHA: push.SHA, At: push.SeenAt.UTC(), State: PushWithoutCollector}
		for index, collector := range collectors {
			if now.Sub(collector.HeardAt) <= CollectorKnownFor && !collector.HeardAt.After(push.SeenAt) {
				origin.Waiting = append(origin.Waiting, index)
			}
		}
		if len(origin.Waiting) > 0 {
			origin.State = PushOriginNotKnown
		}
		origins = append(origins, origin)
	}
	sort.SliceStable(origins, func(left, right int) bool { return origins[left].At.After(origins[right].At) })
	if len(origins) > PushOriginLimit {
		origins = origins[:PushOriginLimit]
	}
	return origins
}

// PushCollector is a collector as a reader of the repository is told of it.
// What went wrong in its run is told to the member whose collector it is, and
// to nobody else. A reader who is not signed in is told neither whose it is
// nor on which machine.
type PushCollector struct {
	By       string    `json:"by,omitempty"`
	Machine  string    `json:"machine,omitempty"`
	Version  string    `json:"cxt_version,omitempty"`
	HeardAt  time.Time `json:"heard_at"`
	Failing  bool      `json:"failing"`
	RunError string    `json:"run_error,omitempty"`
	Own      bool      `json:"own"`
}

// PushOriginView is what the Sessions tab reads: the collectors of the
// repository, and where its newest pushes came from.
type PushOriginView struct {
	Collectors []PushCollector `json:"collectors"`
	Pushes     []PushOrigin    `json:"pushes"`
}

// PushCollectors turns the stored collectors into what one reader is told.
// reader is the account that reads, empty for a reader who is not signed in;
// names holds the public handle of each account that has one.
func PushCollectors(collectors []ReportedCollector, reader string, names map[string]string) []PushCollector {
	told := make([]PushCollector, 0, len(collectors))
	for _, collector := range collectors {
		entry := PushCollector{HeardAt: collector.HeardAt.UTC(), Failing: collector.RunError != ""}
		if reader != "" {
			entry.By, entry.Machine, entry.Version = names[collector.Account], collector.Machine, collector.Version
			if entry.Own = reader == collector.Account; entry.Own {
				entry.RunError = collector.RunError
			}
		}
		told = append(told, entry)
	}
	return told
}
