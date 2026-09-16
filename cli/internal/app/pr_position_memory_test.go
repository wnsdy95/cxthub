package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type prPositionHistory struct {
	outbound.HistoryStore
	events []domain.HistoryEvent
}

func (h *prPositionHistory) ListHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return append([]domain.HistoryEvent(nil), h.events...), nil
}

type prPositionMemoryFixture struct {
	t       *testing.T
	store   *storage.FileStore
	history *prPositionHistory
	svc     *ContextHistoryService
	receipt domain.HistoryEvent
}

func newPRPositionMemoryFixture(t *testing.T) *prPositionMemoryFixture {
	t.Helper()
	f := &prPositionMemoryFixture{t: t, store: storage.NewFileStore(t.TempDir()), history: &prPositionHistory{}}
	f.svc = NewContextHistoryService(f.store, f.history)
	repo := string(domain.HashContent([]byte(t.Name())))
	source := f.snapshot(repo, "source")
	f.receipt = domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: repo, Branch: "main", BranchID: "base-id",
		Kind: "pr-merge", PRCompleted: true, SourceBranchID: "feature-id", Source: source,
		Target: domain.HashContent([]byte("later shared work must not be read")), CreatedAt: time.Unix(100, 0).UTC(),
		PR: &domain.PullRequestMerge{Number: 176, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}}
	return f
}

func (f *prPositionMemoryFixture) snapshot(repo, label string) domain.ContentHash {
	f.t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: label}}}
	id, err := f.store.PutDoc(context.Background(), doc)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.PutSnapshot(context.Background(), domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "feature"}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *prPositionMemoryFixture) memory(owner, previous domain.ContentHash, label string) domain.ContentHash {
	f.t.Helper()
	id, err := f.store.PutMemory(context.Background(), domain.MemoryDigest{SnapshotID: owner, PreviousMemoryHash: previous, Summary: label})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *prPositionMemoryFixture) observation(memory domain.ContentHash) domain.HistoryEvent {
	return domain.HistoryEvent{ID: fmt.Sprintf("%032x", len(f.history.events)+1), RepoID: f.receipt.RepoID,
		Branch: "feature", BranchID: f.receipt.SourceBranchID, Kind: "position", Source: f.receipt.Source,
		Target: f.receipt.Source, GitAfter: f.receipt.PR.HeadSHA, MemoryHash: memory, MemoryPinned: true,
		WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Unix(int64(10+len(f.history.events)), 0).UTC()}
}

func TestResolvePRSourcePositionCausalMaximumIgnoresClockAndMutableMemory(t *testing.T) {
	f := newPRPositionMemoryFixture(t)
	first := f.memory(f.receipt.Source, "", "before memorize")
	middle := f.memory(f.receipt.Source, first, "unobserved intermediate")
	last := f.memory(f.receipt.Source, middle, "final observation")
	e := f.observation(first)
	e.CreatedAt = time.Unix(1000, 0).UTC()
	f.history.events = append(f.history.events, e)
	f.history.events = append(f.history.events, f.observation(last))
	// A later mutable attachment is not an observed source-memory candidate.
	future := f.memory(f.receipt.Source, last, "later unobserved attachment")
	if err := f.store.CompareAndSwapSnapshotMemory(context.Background(), f.receipt.Source, "", future); err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.GetSnapshot(context.Background(), f.receipt.Source)
	events := append([]domain.HistoryEvent(nil), f.history.events...)
	for i := 0; i < 2; i++ {
		if i == 1 {
			f.history.events[0], f.history.events[1] = f.history.events[1], f.history.events[0]
		}
		p, err := f.svc.ResolvePRSourcePosition(context.Background(), f.receipt)
		if err != nil || p.Snapshot != f.receipt.Source || p.MemoryHash != last || !p.MemoryPinned || p.GitCommit != f.receipt.PR.MergeSHA || p.BranchID != f.receipt.BranchID {
			t.Fatalf("causal source memory: %+v %v", p, err)
		}
	}
	f.history.events[0], f.history.events[1] = f.history.events[1], f.history.events[0]
	after, _ := f.store.GetSnapshot(context.Background(), f.receipt.Source)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(events, f.history.events) {
		t.Fatal("resolution changed stored source or observations")
	}
}

func TestResolvePRSourcePositionPreservesPinnedEmpty(t *testing.T) {
	f := newPRPositionMemoryFixture(t)
	f.history.events = append(f.history.events, f.observation(""))
	mutable := f.memory(f.receipt.Source, "", "later mutable memory")
	if err := f.store.CompareAndSwapSnapshotMemory(context.Background(), f.receipt.Source, "", mutable); err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.ResolvePRSourcePosition(context.Background(), f.receipt)
	if err != nil || !p.MemoryPinned || p.MemoryHash != "" {
		t.Fatalf("explicit empty memory lost: %+v %v", p, err)
	}
}

func TestResolvePRSourcePositionPreservesInheritedOwner(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			owner := f.snapshot(f.receipt.RepoID, "inherited owner")
			first := f.memory(owner, "", "inherited first")
			last := f.memory(owner, first, "inherited last")
			for _, memory := range []domain.ContentHash{first, last} {
				e := f.observation(memory)
				e.Kind, e.Source = "birth", owner
				if explicit {
					e.MemorySource = owner
				}
				f.history.events = append(f.history.events, e)
			}
			p, err := f.svc.ResolvePRSourcePosition(context.Background(), f.receipt)
			if err != nil || p.Snapshot != f.receipt.Source || p.MemoryHash != last || p.MemorySource != owner || !p.MemoryPinned {
				t.Fatalf("inherited source provenance lost: %+v %v", p, err)
			}
		})
	}
}

func TestResolvePRSourcePositionRejectsAmbiguousOrBrokenMemory(t *testing.T) {
	for _, scenario := range []string{"divergent", "empty and nonempty", "missing memory", "missing parent", "foreign parent owner", "different candidate owners", "unproven owner", "foreign repository owner", "ambiguous provenance"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			root := f.memory(f.receipt.Source, "", "root")
			left := f.memory(f.receipt.Source, root, "left")
			e := f.observation(left)
			f.history.events = append(f.history.events, e)
			otherOwner := f.snapshot(f.receipt.RepoID, "other owner")
			otherMemory := f.memory(otherOwner, "", "other memory")
			switch scenario {
			case "divergent":
				f.history.events = append(f.history.events, f.observation(f.memory(f.receipt.Source, root, "right")))
			case "empty and nonempty":
				f.history.events = append(f.history.events, f.observation(""))
			case "missing memory":
				f.history.events[0].MemoryHash = domain.HashContent([]byte("missing"))
			case "missing parent":
				f.history.events[0].MemoryHash = f.memory(f.receipt.Source, domain.HashContent([]byte("missing")), "broken parent")
			case "foreign parent owner":
				f.history.events[0].MemoryHash = f.memory(f.receipt.Source, otherMemory, "wrong owner parent")
			case "different candidate owners":
				other := f.observation(otherMemory)
				other.MemorySource = otherOwner
				f.history.events = append(f.history.events, other)
			case "unproven owner":
				f.history.events[0].MemoryHash = otherMemory
			case "foreign repository owner":
				foreign := f.snapshot(string(domain.HashContent([]byte("foreign repo"))), "foreign snapshot")
				f.history.events[0].MemoryHash = f.memory(foreign, "", "foreign memory")
				f.history.events[0].MemorySource = foreign
			case "ambiguous provenance":
				other := f.observation(left)
				other.MemorySource = otherOwner
				f.history.events = append(f.history.events, other)
			}
			if _, err := f.svc.ResolvePRSourcePosition(context.Background(), f.receipt); err == nil {
				t.Fatal("accepted ambiguous or invalid historical memory")
			}
		})
	}
}

func TestResolvePRSourcePositionRequiresExactPinnedOrdinaryProof(t *testing.T) {
	for _, scenario := range []string{"repo", "branch identity", "head SHA", "target", "unpinned", "publish", "pr-merge"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			e := f.observation("")
			switch scenario {
			case "repo":
				e.RepoID = string(domain.HashContent([]byte("other repo")))
			case "branch identity":
				e.BranchID = "other"
			case "head SHA":
				e.GitAfter = strings.Repeat("c", 40)
			case "target":
				e.Target = domain.HashContent([]byte("other snapshot"))
			case "unpinned":
				e.MemoryPinned = false
			default:
				e.Kind = scenario
			}
			f.history.events = append(f.history.events, e)
			if _, err := f.svc.ResolvePRSourcePosition(context.Background(), f.receipt); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("inexact proof accepted: %v", err)
			}
		})
	}
}

type prMemoryReadFault struct {
	outbound.SessionStore
	badDoc    bool
	badMemory bool
}

func (s prMemoryReadFault) GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error) {
	d, err := s.SessionStore.GetDoc(ctx, hash)
	if s.badDoc {
		d.CIR.Envelope.SessionOriginID = "corrupted"
	}
	return d, err
}
func (s prMemoryReadFault) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	d, err := s.SessionStore.GetMemory(ctx, hash)
	if s.badMemory {
		d.Summary = "corrupted"
	}
	return d, err
}

func TestResolvePRSourcePositionVerifiesContentAndCompletion(t *testing.T) {
	for _, scenario := range []string{"pending receipt", "missing source", "corrupt doc", "corrupt memory", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			memory := f.memory(f.receipt.Source, "", "source memory")
			f.history.events = append(f.history.events, f.observation(memory))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "pending receipt":
				f.receipt.PRCompleted, f.receipt.Target = false, f.receipt.Source
			case "missing source":
				f.receipt.Source = domain.HashContent([]byte("missing source"))
			case "corrupt doc":
				f.svc.store = prMemoryReadFault{SessionStore: f.store, badDoc: true}
			case "corrupt memory":
				f.svc.store = prMemoryReadFault{SessionStore: f.store, badMemory: true}
			case "canceled":
				cancel()
			}
			if _, err := f.svc.ResolvePRSourcePosition(ctx, f.receipt); err == nil {
				t.Fatal("invalid source/receipt accepted")
			}
		})
	}
}
