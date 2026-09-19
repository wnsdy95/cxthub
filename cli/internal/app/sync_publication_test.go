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

type publicationRemote struct {
	outbound.RemoteSync
	accepted         []domain.HistoryEvent
	terminal         map[[3]string]domain.ContentHash // repo, PR head name, full SHA
	requiredOrdinary []string
	failID           string
	failAfterAccept  bool
}

func (r *publicationRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return r.accepted, nil
}
func (r *publicationRemote) PushHistoryEvent(_ context.Context, e domain.HistoryEvent) error {
	if e.ID == r.failID && !r.failAfterAccept {
		return errors.New("injected history failure")
	}
	if e.Kind == "publish" {
		proven := false
		for _, old := range r.accepted {
			if old.Kind == "position" && old.RepoID == e.RepoID && old.BranchID == e.BranchID &&
				old.Branch == e.Branch && old.LocalBranch == e.LocalBranch && old.WorktreeID == e.WorktreeID &&
				old.Target == e.Target && old.GitAfter == e.GitAfter {
				proven = true
			}
		}
		if !proven {
			return fmt.Errorf("publication arrived before proof")
		}
		for _, id := range r.requiredOrdinary {
			found := false
			for _, old := range r.accepted {
				found = found || old.ID == id
			}
			if !found {
				return fmt.Errorf("publication arrived before ordinary event %s", id)
			}
		}
		// Run a waiting worker immediately, before the next upload. Its first
		// eligible source is terminal even if later publications contain more.
		if r.terminal == nil {
			r.terminal = map[[3]string]domain.ContentHash{}
		}
		names := []string{e.Branch}
		for _, proof := range r.accepted {
			if proof.Kind != "position" || proof.Target != e.Target || proof.BranchID != e.BranchID ||
				proof.GitAfter != e.GitAfter || proof.WorktreeID != e.WorktreeID || proof.LocalBranch != e.LocalBranch {
				continue
			}
			for _, attach := range r.accepted {
				if e.LocalBranch != "" && e.WorktreeID != "" && attach.Kind == "attach" &&
					attach.RepoID == e.RepoID && attach.BranchID == e.BranchID && attach.WorktreeID == e.WorktreeID &&
					attach.LocalBranch != "" && !attach.CreatedAt.After(proof.CreatedAt) {
					names = append(names, e.LocalBranch)
				}
			}
		}
		for _, name := range names {
			key := [3]string{e.RepoID, name, e.GitAfter}
			if r.terminal[key] == "" {
				r.terminal[key] = e.Target
			}
		}
	}
	r.accepted = append(r.accepted, e)
	if e.ID == r.failID {
		return errors.New("injected lost acknowledgement")
	}
	return nil
}

func publicationSnapshot(t *testing.T, st *storage.FileStore, repo, label string, parents, grafts []domain.ContentHash) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: label}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "feature", Parents: parents, GraftParents: grafts}); err != nil {
		t.Fatal(err)
	}
	return id
}

func putPublication(t *testing.T, st *storage.FileStore, n int, p domain.HistoryEvent) (domain.HistoryEvent, domain.HistoryEvent) {
	t.Helper()
	p.ID, p.Kind, p.Source = fmt.Sprintf("%032x", n), "publish", p.Target
	proof := p
	proof.ID, proof.Kind = fmt.Sprintf("%032x", n+1000), "position"
	// Even the proof of an older publication can have a later wall clock.
	proof.CreatedAt = p.CreatedAt.Add(time.Hour)
	for _, e := range []domain.HistoryEvent{proof, p} {
		if err := st.PutHistoryEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	return p, proof
}

func publicationTargets(r *publicationRemote) []domain.ContentHash {
	var out []domain.ContentHash
	for _, e := range r.accepted {
		if e.Kind == "publish" {
			out = append(out, e.Target)
		}
	}
	return out
}

func TestHistoryPushPublishesMaximalCompletedSourceFirst(t *testing.T) {
	for _, edge := range []string{"natural", "graft", "indirect", "clock rollback", "linked worktree alias"} {
		t.Run(edge, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			a := publicationSnapshot(t, st, repo, "A", nil, nil)
			parents, grafts := []domain.ContentHash{a}, []domain.ContentHash(nil)
			if edge == "graft" {
				parents, grafts = nil, parents
			}
			if edge == "indirect" {
				parents = []domain.ContentHash{publicationSnapshot(t, st, repo, "middle", parents, nil)}
			}
			b := publicationSnapshot(t, st, repo, "B", parents, grafts)
			p := domain.HistoryEvent{RepoID: repo, BranchID: "task", Branch: "team/task", Target: a, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
			if edge == "linked worktree alias" {
				p.LocalBranch, p.WorktreeID = "local-task", strings.Repeat("1", 32)
			}
			pa, oa := putPublication(t, st, 1, p)
			p.Target, p.CreatedAt = b, p.CreatedAt.Add(time.Second)
			if edge == "clock rollback" {
				p.CreatedAt = p.CreatedAt.Add(-time.Minute)
			}
			if edge == "linked worktree alias" {
				p.WorktreeID = strings.Repeat("2", 32)
				for i, source := range []domain.HistoryEvent{pa, p} {
					source.ID, source.Kind, source.CreatedAt = fmt.Sprintf("%032x", i+2000), "attach", time.Unix(1, 0).UTC()
					if err := st.PutHistoryEvent(ctx, source); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, ob := putPublication(t, st, 2, p)
			r := &publicationRemote{requiredOrdinary: []string{oa.ID, ob.ID}}
			if err := newTestSyncService(st, r, nil).pushHistory(ctx, repo); err != nil {
				t.Fatal(err)
			}
			if got := publicationTargets(r); !reflect.DeepEqual(got, []domain.ContentHash{b, a}) {
				t.Fatalf("publication order = %v, want B then A", got)
			}
			for key, target := range r.terminal {
				if target != b {
					t.Fatalf("worker bound %v to %s before descendant %s", key, target, b)
				}
			}
			if p.LocalBranch != "" && r.terminal[[3]string{repo, p.LocalBranch, p.GitAfter}] != b {
				t.Fatal("alias worker did not bind B")
			}
		})
	}
}

func TestHistoryPushRejectsAmbiguousPublicationsBeforePublishing(t *testing.T) {
	for _, scenario := range []string{"incomparable", "worktree incomparable", "same name different identity", "alias collides with canonical", "maximal missing old alias"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			a := publicationSnapshot(t, st, repo, "A", nil, nil)
			var parents []domain.ContentHash
			if scenario == "maximal missing old alias" {
				parents = []domain.ContentHash{a}
			}
			b := publicationSnapshot(t, st, repo, "B", parents, nil)
			p := domain.HistoryEvent{RepoID: repo, BranchID: "task", Branch: "team/task", Target: a, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
			if strings.Contains(scenario, "alias") {
				p.LocalBranch, p.WorktreeID = "local-task", strings.Repeat("1", 32)
				attach := p
				attach.ID, attach.Kind = strings.Repeat("f", 32), "attach"
				if err := st.PutHistoryEvent(ctx, attach); err != nil {
					t.Fatal(err)
				}
			}
			putPublication(t, st, 1, p)
			p.Target, p.CreatedAt = b, p.CreatedAt.Add(time.Second)
			switch scenario {
			case "worktree incomparable":
				p.LocalBranch, p.WorktreeID = "another-local", strings.Repeat("2", 32)
			case "same name different identity":
				p.BranchID = "another-task"
			case "alias collides with canonical":
				p.BranchID, p.Branch = "another-task", p.LocalBranch
				p.LocalBranch, p.WorktreeID = "", ""
			case "maximal missing old alias":
				p.LocalBranch, p.WorktreeID = "another-local", strings.Repeat("2", 32)
			}
			putPublication(t, st, 2, p)
			r := &publicationRemote{}
			err := newTestSyncService(st, r, nil).pushHistory(ctx, repo)
			if !errors.Is(err, domain.ErrSyncConflict) || len(publicationTargets(r)) != 0 || len(r.terminal) != 0 {
				t.Fatalf("ambiguous group published: targets=%v terminal=%v err=%v", publicationTargets(r), r.terminal, err)
			}
		})
	}
}

func TestHistoryPushPublicationPartialRetry(t *testing.T) {
	for _, scenario := range []string{"maximal rejected", "maximal acknowledgement lost", "ancestor rejected", "ordinary rejected"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			a := publicationSnapshot(t, st, repo, "A", nil, nil)
			b := publicationSnapshot(t, st, repo, "B", []domain.ContentHash{a}, nil)
			p := domain.HistoryEvent{RepoID: repo, BranchID: "task", Branch: "feature", Target: a, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
			pa, _ := putPublication(t, st, 1, p)
			p.Target, p.CreatedAt = b, p.CreatedAt.Add(time.Second)
			pb, ob := putPublication(t, st, 2, p)
			r := &publicationRemote{failID: pb.ID}
			switch scenario {
			case "maximal acknowledgement lost":
				r.failAfterAccept = true
			case "ancestor rejected":
				r.failID = pa.ID
			case "ordinary rejected":
				r.failID = ob.ID
			}
			svc := newTestSyncService(st, r, nil)
			if err := svc.pushHistory(ctx, repo); err == nil {
				t.Fatal("injected failure did not stop push")
			}
			got := publicationTargets(r)
			if scenario == "maximal rejected" || scenario == "ordinary rejected" {
				if len(got) != 0 {
					t.Fatalf("published ancestor before failed prerequisite: %v", got)
				}
			} else if !reflect.DeepEqual(got, []domain.ContentHash{b}) {
				t.Fatalf("partial publication = %v, want B only", got)
			}
			r.failID = ""
			for i := 0; i < 2; i++ {
				if err := svc.pushHistory(ctx, repo); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(publicationTargets(r), []domain.ContentHash{b, a}) || r.terminal[[3]string{repo, p.Branch, p.GitAfter}] != b {
				t.Fatalf("retry changed terminal source or duplicated events: %+v", r)
			}
		})
	}
}

func TestHistoryPushLaterPublicationPreservesAcceptedTerminal(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	a := publicationSnapshot(t, st, repo, "A", nil, nil)
	b := publicationSnapshot(t, st, repo, "B", []domain.ContentHash{a}, nil)
	p := domain.HistoryEvent{RepoID: repo, BranchID: "task", Branch: "feature", Target: a, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	putPublication(t, st, 1, p)
	r := &publicationRemote{}
	svc := newTestSyncService(st, r, nil)
	if err := svc.pushHistory(ctx, repo); err != nil {
		t.Fatal(err)
	}
	p.Target = b
	putPublication(t, st, 2, p)
	if err := svc.pushHistory(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(publicationTargets(r), []domain.ContentHash{a, b}) || r.terminal[[3]string{repo, p.Branch, p.GitAfter}] != a {
		t.Fatalf("later publication changed an already terminal source: %+v", r)
	}
}
func TestHistoryPushKeepsUnrelatedPublicationGroupsIndependent(t *testing.T) {
	for _, scenario := range []string{"unrelated branch names", "different full SHA"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			a := publicationSnapshot(t, st, repo, "A", nil, nil)
			b := publicationSnapshot(t, st, repo, "unrelated B", nil, nil)
			p := domain.HistoryEvent{RepoID: repo, BranchID: "task", Branch: "feature", Target: a, GitAfter: strings.Repeat("a", 64), CreatedAt: time.Unix(100, 0).UTC()}
			pa, _ := putPublication(t, st, 1, p)
			p.Target = b
			if scenario == "unrelated branch names" {
				p.BranchID, p.Branch = "other-task", "other-feature"
			} else {
				// Equal abbreviated prefixes must never combine distinct revisions.
				p.GitAfter = strings.Repeat("a", 63) + "b"
			}
			pb, _ := putPublication(t, st, 2, p)
			r := &publicationRemote{}
			if err := newTestSyncService(st, r, nil).pushHistory(ctx, repo); err != nil {
				t.Fatal(err)
			}
			if len(publicationTargets(r)) != 2 || r.terminal[[3]string{repo, pa.Branch, pa.GitAfter}] != a || r.terminal[[3]string{repo, pb.Branch, pb.GitAfter}] != b {
				t.Fatalf("unrelated groups were combined: %+v", r)
			}
		})
	}
}

func TestHistoryPushFinalizesAfterProofDespiteClockRollback(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("publication")))
	target := publicationSnapshot(t, st, repo, "source", nil, nil)
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: "feature", Branch: "feature", Kind: "position", Target: target, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	p := e
	p.ID, p.Kind, p.Source, p.CreatedAt = strings.Repeat("2", 32), "publish", e.Target, time.Unix(50, 0).UTC()
	for _, event := range []domain.HistoryEvent{e, p} {
		if err := st.PutHistoryEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	r := &publicationRemote{}
	svc := newTestSyncService(st, r, nil)
	for i := 0; i < 2; i++ {
		if err := svc.pushHistory(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.accepted) != 2 || r.accepted[0].ID != e.ID || r.accepted[1].ID != p.ID {
		t.Fatalf("publication order: %+v", r.accepted)
	}
}
