package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type recoveryFixture struct {
	attempts    []domain.CaptureAttempt
	events      []domain.HistoryEvent
	snapshots   map[domain.ContentHash]domain.Snapshot
	resolutions map[string]domain.CaptureResolution
	beforeWrite func()
}

func (f *recoveryFixture) ListCaptureAttempts(context.Context, string) ([]domain.CaptureAttempt, error) {
	return f.attempts, nil
}
func (f *recoveryFixture) ListHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return f.events, nil
}
func (f *recoveryFixture) GetSnapshot(_ context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	s, ok := f.snapshots[id]
	if !ok {
		return s, domain.ErrNotFound
	}
	return s, nil
}
func (f *recoveryFixture) ReadCaptureResolution(_ context.Context, p domain.CaptureAttempt) (*domain.CaptureResolution, error) {
	r, ok := f.resolutions[p.Proof.ID]
	if !ok {
		return nil, nil
	}
	return &r, nil
}
func (f *recoveryFixture) WriteCaptureResolution(_ context.Context, r domain.CaptureResolution, w []domain.CaptureAttempt) error {
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	for _, p := range w {
		found := false
		for _, cur := range f.attempts {
			if cur.Proof.ID == p.Proof.ID && cur.Fingerprint() == p.Fingerprint() {
				found = true
			}
		}
		if !found {
			return domain.ErrSyncConflict
		}
	}
	f.resolutions[r.AttemptID] = r
	return nil
}
func newRecoveryFixture(t *testing.T) (*recoveryFixture, *CaptureRecoveryService, string) {
	t.Helper()
	repo := string(domain.HashContent([]byte("recovery-repo")))
	base, tip := domain.HashContent([]byte("baseline")), domain.HashContent([]byte("later"))
	p := domain.CaptureAttempt{Version: 1, Initial: base, Proof: domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: "generation", Branch: "feature", WorktreeID: strings.Repeat("a", 32), Kind: "position", GitAfter: strings.Repeat("b", 40), CreatedAt: time.Unix(1, 0).UTC()}, Outcomes: []domain.CaptureOutcome{{Provider: domain.ProviderCodex, State: "failed", SessionPath: "/sessions/original", Error: "conflict"}}}
	q := p
	q.Proof.ID = strings.Repeat("2", 32)
	q.Proof.CreatedAt = time.Unix(2, 0).UTC()
	q.Proof.Source, q.Proof.Target = tip, tip
	q.Complete = true
	q.Outcomes = []domain.CaptureOutcome{{Provider: domain.ProviderCodex, State: "saved", SessionPath: "/sessions/original", Target: tip}}
	f := &recoveryFixture{attempts: []domain.CaptureAttempt{p, q}, events: []domain.HistoryEvent{q.Publication()}, snapshots: map[domain.ContentHash]domain.Snapshot{tip: {ID: tip, RepoID: repo, GraftParents: []domain.ContentHash{base}}, base: {ID: base, RepoID: repo}}, resolutions: map[string]domain.CaptureResolution{}}
	for _, p := range f.attempts {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	return f, NewCaptureRecoveryService(f, f), repo
}

func TestCaptureRecoveryRequiresExactPositionAndPublication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*recoveryFixture)
	}{
		{"worktree", func(f *recoveryFixture) { f.attempts[1].Proof.WorktreeID = strings.Repeat("c", 32) }},
		{"generation", func(f *recoveryFixture) { f.attempts[1].Proof.BranchID = "another-generation" }},
		{"branch", func(f *recoveryFixture) { f.attempts[1].Proof.Branch = "other" }},
		{"local branch", func(f *recoveryFixture) { f.attempts[1].Proof.LocalBranch = "other" }},
		{"code", func(f *recoveryFixture) { f.attempts[1].Proof.GitAfter = strings.Repeat("d", 40) }},
		{"session", func(f *recoveryFixture) { f.attempts[1].Outcomes[0].SessionPath = "/sessions/other" }},
		{"earlier", func(f *recoveryFixture) { f.attempts[1].Proof.CreatedAt = time.Unix(0, 1).UTC() }},
		{"absent provider", func(f *recoveryFixture) {
			f.attempts[0].Outcomes[0].SessionPath = ""
			f.attempts[1].Outcomes[0] = domain.CaptureOutcome{Provider: domain.ProviderCodex, State: "absent"}
			f.attempts[1].Initial = f.attempts[1].Proof.Target
		}},
		{"baseline missing from lineage", func(f *recoveryFixture) {
			id := f.attempts[1].Proof.Target
			s := f.snapshots[id]
			s.GraftParents = nil
			f.snapshots[id] = s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, repo := newRecoveryFixture(t)
			tc.mutate(f)
			f.events = []domain.HistoryEvent{f.attempts[1].Publication()}
			got, err := s.Inspect(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].State != "needs-review" {
				t.Fatalf("invented recovery: %+v", got[0])
			}
		})
	}
	t.Run("no publication", func(t *testing.T) {
		f, s, repo := newRecoveryFixture(t)
		f.events = nil
		got, err := s.Inspect(context.Background(), repo)
		if err != nil || got[0].State != "needs-review" {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestCaptureRecoveryPreservesOriginalAttemptAndHistoricalDecision(t *testing.T) {
	f, s, repo := newRecoveryFixture(t)
	ctx := context.Background()
	old := f.attempts[0]
	fingerprint := old.Fingerprint()
	st, err := s.Inspect(ctx, repo)
	if err != nil || st[0].State != "superseded" {
		t.Fatalf("%+v %v", st, err)
	}
	r, err := s.Resolve(ctx, repo, old.Proof.ID, fingerprint, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "superseded" || r.PublicationID != f.events[0].ID || !reflect.DeepEqual(old, f.attempts[0]) {
		t.Fatalf("rewrote historical completion: %+v", r)
	}
	// Later overlay movement cannot undo the independently recorded fact.
	id := f.attempts[1].Proof.Target
	snap := f.snapshots[id]
	snap.GraftParents = nil
	f.snapshots[id] = snap
	st, err = s.Inspect(ctx, repo)
	if err != nil || st[0].Resolution == nil || st[0].State != "superseded" {
		t.Fatalf("lost historical decision: %+v %v", st, err)
	}
	if _, err = s.Resolve(ctx, repo, old.Proof.ID, fingerprint, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RetryAttempt(ctx, repo, old.Proof.ID, fingerprint); err == nil {
		t.Fatal("resolved failed attempt became publishable")
	}
	if _, err = s.Resolve(ctx, repo, old.Proof.ID, fingerprint, "different interpretation", true); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("conflicting decision: %v", err)
	}
}

func TestCaptureRecoveryAcknowledgesGapWithoutPublication(t *testing.T) {
	f, s, repo := newRecoveryFixture(t)
	f.attempts = f.attempts[:1]
	f.events = nil
	p := f.attempts[0]
	ctx := context.Background()
	if _, err := s.Resolve(ctx, repo, p.Proof.ID, p.Fingerprint(), "", true); err == nil {
		t.Fatal("acknowledged without reason")
	}
	r, err := s.Resolve(ctx, repo, p.Proof.ID, p.Fingerprint(), "No surviving recorded capture output", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "acknowledged-gap" || len(f.events) != 0 || f.attempts[0].Complete {
		t.Fatal("gap was turned into completion")
	}
	st, err := s.Inspect(ctx, repo)
	if err != nil || st[0].State != "acknowledged-gap" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestCaptureRecoveryRejectsStaleEvidenceAndCancellation(t *testing.T) {
	for _, index := range []int{0, 1} {
		t.Run(string(rune('0'+index)), func(t *testing.T) {
			f, s, repo := newRecoveryFixture(t)
			p := f.attempts[0]
			f.beforeWrite = func() { f.attempts[index].Proof.CreatedAt = f.attempts[index].Proof.CreatedAt.Add(time.Nanosecond) }
			if _, err := s.Resolve(context.Background(), repo, p.Proof.ID, p.Fingerprint(), "", false); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("stale witness: %v", err)
			}
			if len(f.resolutions) != 0 {
				t.Fatal("stale resolution persisted")
			}
		})
	}
	f, s, repo := newRecoveryFixture(t)
	p := f.attempts[0]
	if _, err := s.Resolve(context.Background(), repo, p.Proof.ID, domain.HashContent([]byte("stale")), "", false); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale review: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Inspect(ctx, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
