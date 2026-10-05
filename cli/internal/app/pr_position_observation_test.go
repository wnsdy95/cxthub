package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Supplied observations must not acquire a second, mutable history input or be
// installed locally as a side effect of resolving a PR source position.
type prObservationHistoryProbe struct {
	outbound.HistoryStore
	readErr       error
	reads, writes int
}

func (h *prObservationHistoryProbe) ListHistoryEvents(ctx context.Context, repo string) ([]domain.HistoryEvent, error) {
	h.reads++
	if h.readErr != nil {
		return nil, h.readErr
	}
	return h.HistoryStore.ListHistoryEvents(ctx, repo)
}

func (h *prObservationHistoryProbe) PutHistoryEvent(context.Context, domain.HistoryEvent) error {
	h.writes++
	return errors.New("observation resolver must not adopt history")
}

func (h *prObservationHistoryProbe) assertUnused(t *testing.T) {
	t.Helper()
	if h.reads != 0 || h.writes != 0 {
		t.Fatalf("supplied-history resolution accessed local history: reads=%d writes=%d", h.reads, h.writes)
	}
}

func TestResolvePRSourcePositionFromHistoryUsesRemoteProofWithoutLocalRead(t *testing.T) {
	for _, local := range []string{"absent", "conflicting pin", "reader unavailable"} {
		t.Run(local, func(t *testing.T) {
			ctx := context.Background()
			f := newPRPositionMemoryFixture(t)
			first := f.memory(f.receipt.Source, "", "remote first")
			middle := f.memory(f.receipt.Source, first, "unobserved causal intermediate")
			last := f.memory(f.receipt.Source, middle, "remote pinned maximum")
			mutable := f.memory(f.receipt.Source, last, "later mutable attachment")
			if err := f.store.CompareAndSwapSnapshotMemory(ctx, f.receipt.Source, "", mutable); err != nil {
				t.Fatal(err)
			}
			older, newer := f.observation(first), f.observation(last)
			newer.ID = strings.Repeat("2", 32)
			older.CreatedAt = time.Unix(1000, 0).UTC() // Clock order opposes causality.
			supplied := []domain.HistoryEvent{older, newer}
			probe := &prObservationHistoryProbe{HistoryStore: f.store}
			if local == "conflicting pin" {
				conflict := older // Same ID, different payload: merging must not occur.
				conflict.MemoryHash = mutable
				if err := f.store.PutHistoryEvent(ctx, conflict); err != nil {
					t.Fatal(err)
				}
			}
			if local == "reader unavailable" {
				probe.readErr = errors.New("local history deliberately unavailable")
			}
			f.svc = NewContextHistoryService(f.store, probe)
			beforeHistory, err := f.store.ListHistoryEvents(ctx, f.receipt.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			beforeSnapshot, err := f.store.GetSnapshot(ctx, f.receipt.Source)
			if err != nil {
				t.Fatal(err)
			}
			for _, events := range [][]domain.HistoryEvent{supplied, {newer, older}} {
				before, err := json.Marshal([]any{f.receipt, events})
				if err != nil {
					t.Fatal(err)
				}
				p, err := f.svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, events)
				if err != nil || p.Snapshot != f.receipt.Source || p.MemoryHash != last || !p.MemoryPinned || p.BranchID != f.receipt.BranchID || p.GitCommit != f.receipt.PR.MergeSHA {
					t.Fatalf("remote source pin lost: position=%+v err=%v", p, err)
				}
				after, err := json.Marshal([]any{f.receipt, events})
				if err != nil || string(before) != string(after) {
					t.Fatalf("caller-owned receipt/observations changed: %v", err)
				}
			}
			probe.assertUnused(t)
			afterHistory, err := f.store.ListHistoryEvents(ctx, f.receipt.RepoID)
			if err != nil || !reflect.DeepEqual(beforeHistory, afterHistory) {
				t.Fatalf("remote history adopted or local history changed: %v", err)
			}
			afterSnapshot, err := f.store.GetSnapshot(ctx, f.receipt.Source)
			if err != nil || !reflect.DeepEqual(beforeSnapshot, afterSnapshot) {
				t.Fatalf("mutable source attachment changed: %v", err)
			}
		})
	}
}

func TestResolvePRSourcePositionFromHistoryPreservesPinnedEmpty(t *testing.T) {
	ctx := context.Background()
	f := newPRPositionMemoryFixture(t)
	mutable := f.memory(f.receipt.Source, "", "local mutable memory must not fill the empty pin")
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, f.receipt.Source, "", mutable); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutHistoryEvent(ctx, f.observation(mutable)); err != nil {
		t.Fatal(err)
	}
	probe := &prObservationHistoryProbe{HistoryStore: f.store}
	f.svc = NewContextHistoryService(f.store, probe)
	p, err := f.svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, []domain.HistoryEvent{f.observation("")})
	if err != nil || !p.MemoryPinned || p.MemoryHash != "" || p.MemorySource != "" || p.Snapshot != f.receipt.Source {
		t.Fatalf("remote pinned-empty proof was replaced: %+v %v", p, err)
	}
	probe.assertUnused(t)
}

func TestResolvePRSourcePositionFromHistoryDoesNotFallbackToLocalProof(t *testing.T) {
	for _, missing := range []string{"empty observation", "wrong head"} {
		t.Run(missing, func(t *testing.T) {
			ctx := context.Background()
			f := newPRPositionMemoryFixture(t)
			local := f.observation(f.memory(f.receipt.Source, "", "valid local-only pin"))
			if err := f.store.PutHistoryEvent(ctx, local); err != nil {
				t.Fatal(err)
			}
			var supplied []domain.HistoryEvent
			if missing == "wrong head" {
				other := local
				other.GitAfter = strings.Repeat("c", 40)
				supplied = []domain.HistoryEvent{other}
			}
			probe := &prObservationHistoryProbe{HistoryStore: f.store}
			f.svc = NewContextHistoryService(f.store, probe)
			p, err := f.svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, supplied)
			if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(p, domain.WorkingPosition{}) {
				t.Fatalf("missing remote proof borrowed local evidence: %+v %v", p, err)
			}
			probe.assertUnused(t)
		})
	}
}

func TestResolvePRSourcePositionFromHistoryRejectsConflictingSuppliedPins(t *testing.T) {
	for _, conflict := range []string{"divergent chains", "empty and nonempty", "duplicate event ID"} {
		t.Run(conflict, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			root := f.memory(f.receipt.Source, "", "root")
			left := f.observation(f.memory(f.receipt.Source, root, "left"))
			right := f.observation(f.memory(f.receipt.Source, root, "right"))
			right.ID = strings.Repeat("2", 32)
			right.CreatedAt = time.Unix(1000, 0).UTC()
			if conflict == "empty and nonempty" {
				right.MemoryHash = ""
			}
			if conflict == "duplicate event ID" {
				right.ID = left.ID
			}
			probe := &prObservationHistoryProbe{HistoryStore: f.store, readErr: errors.New("local read must not explain this rejection")}
			f.svc = NewContextHistoryService(f.store, probe)
			p, err := f.svc.ResolvePRSourcePositionFromHistory(context.Background(), f.receipt, []domain.HistoryEvent{left, right})
			if err == nil || !reflect.DeepEqual(p, domain.WorkingPosition{}) {
				t.Fatalf("conflicting supplied proof accepted: %+v %v", p, err)
			}
			if conflict == "duplicate event ID" {
				if !strings.Contains(err.Error(), "conflicting history operation") {
					t.Fatalf("wrong rejection cause: %v", err)
				}
			} else if !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("wrong conflict classification: %v", err)
			}
			probe.assertUnused(t)
		})
	}
}

type prObservationMemoryFault struct {
	outbound.SessionStore
	corrupt domain.ContentHash
}

func (s prObservationMemoryFault) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	d, err := s.SessionStore.GetMemory(ctx, hash)
	if hash == s.corrupt {
		d.Summary = "bytes do not match requested immutable hash"
	}
	return d, err
}

func TestResolvePRSourcePositionFromHistoryVerifiesSourceAndMemoryHashes(t *testing.T) {
	for _, corrupt := range []string{"source document", "pinned memory", "memory parent"} {
		t.Run(corrupt, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			parent := f.memory(f.receipt.Source, "", "parent")
			pin := f.memory(f.receipt.Source, parent, "remote pin")
			probe := &prObservationHistoryProbe{HistoryStore: f.store, readErr: errors.New("unexpected local history read")}
			var store outbound.SessionStore = f.store
			switch corrupt {
			case "source document":
				store = prMemoryReadFault{SessionStore: f.store, badDoc: true}
			case "pinned memory":
				store = prObservationMemoryFault{SessionStore: f.store, corrupt: pin}
			case "memory parent":
				store = prObservationMemoryFault{SessionStore: f.store, corrupt: parent}
			}
			f.svc = NewContextHistoryService(store, probe)
			p, err := f.svc.ResolvePRSourcePositionFromHistory(context.Background(), f.receipt, []domain.HistoryEvent{f.observation(pin)})
			if !errors.Is(err, domain.ErrHashMismatch) || !reflect.DeepEqual(p, domain.WorkingPosition{}) {
				t.Fatalf("corrupt supplied source proof accepted: %+v %v", p, err)
			}
			probe.assertUnused(t)
		})
	}
}

func TestResolvePRSourcePositionFromHistoryRetainsInheritedOwner(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "implicit birth source"
		if explicit {
			name = "explicit memory source"
		}
		t.Run(name, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			owner := f.snapshot(f.receipt.RepoID, "remote inherited owner")
			pin := f.memory(owner, "", "remote inherited memory")
			e := f.observation(pin)
			e.Kind, e.Source = "birth", owner
			if explicit {
				e.MemorySource = owner
			}
			probe := &prObservationHistoryProbe{HistoryStore: f.store, readErr: errors.New("unexpected local history read")}
			f.svc = NewContextHistoryService(f.store, probe)
			p, err := f.svc.ResolvePRSourcePositionFromHistory(context.Background(), f.receipt, []domain.HistoryEvent{e})
			if err != nil || p.Snapshot != f.receipt.Source || p.MemoryHash != pin || p.MemorySource != owner || !p.MemoryPinned {
				t.Fatalf("remote memory ownership lost: %+v %v", p, err)
			}
			probe.assertUnused(t)
		})
	}
}
