//go:build postgres

package app

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGRepositoryInitializationBranchLocalFlow(t *testing.T) {
	svc, st, owner, member, request, receipt := protectedInitializationFixture(t)
	id := receipt.Repo.ID
	a := collaborationSnapshot(t, st, id, "preexisting main checkpoint")
	snap, err := st.GetSnapshot(owner, id, a)
	if err != nil {
		t.Fatal(err)
	}
	// A real member birth may refer to the still-unpublished legacy main identity.
	birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(id), Kind: "birth", Branch: "feature", BranchID: "genuine-feature", Source: a, Target: a, CreatedAt: time.Now().UTC(), Creation: &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature"}, StartRef: "HEAD", OriginBranch: "main", OriginBranchID: domain.LegacyContextBranchID(string(id), "main")}}
	if err := svc.RecordHistory(member, birth); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main", "other"} {
		if _, available, err := svc.GetRepositoryInitializationView(member, id, ""); err != nil || available {
			t.Fatal("unselected availability", err)
		}
		if _, available, err := svc.GetRepositoryInitializationView(member, id, name); err != nil || !available {
			t.Fatal("independent branch blocked", name, err)
		}
		in := initializationFinalize(receipt, snap)
		in.Anchor.Ref.Name = name
		in.Anchor.Ref.BranchID = domain.LegacyContextBranchID(string(id), name)
		// Exact ordinary observations are legal foreign evidence, not ref ownership.
		position := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(id), Kind: "position", Branch: name, BranchID: in.Anchor.Ref.BranchID, Source: a, Target: a, CreatedAt: time.Now().UTC()}
		if name == "other" {
			position.ID = strings.Repeat("3", 32)
		}
		if err := svc.RecordHistory(member, position); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.FinalizeRepositoryInitialization(member, id, in); !errors.Is(err, domain.ErrForbidden) {
			t.Fatal("member observed legacy", err)
		}
		got, err := svc.FinalizeRepositoryInitialization(owner, id, in)
		if err != nil || got.Anchor == nil || !got.Anchor.Equal(in.Anchor) {
			t.Fatal("explicit branch observation", name, err)
		}
		if _, available, err := svc.GetRepositoryInitializationView(owner, id, name); err != nil || available {
			t.Fatal("accepted still available", err)
		}
		replay, err := svc.FinalizeRepositoryInitialization(owner, id, in)
		if err != nil || !reflect.DeepEqual(got, replay) {
			t.Fatal("exact replay", err)
		}
	}
	refs, err := st.ListRefs(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ref := range refs {
		if ref.Kind == domain.RefBranch {
			count++
		}
	}
	if count != 3 {
		t.Fatal("extra or missing branches", count)
	}
	actor, _ := inbound.RepositoryActor(owner)
	recovered, err := svc.BeginRepositoryInitialization(owner, actor, id, request)
	if err != nil || !reflect.DeepEqual(receipt, recovered) || recovered.Anchor != nil {
		t.Fatal("Begin not immutable creation-only", err)
	}
	got, err := svc.GetRepositoryInitialization(owner, id)
	if err != nil || !reflect.DeepEqual(receipt, got) {
		t.Fatal("GET receipt", err)
	}
	if _, err := svc.GetRepositoryInitialization(member, id); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("member read privileged receipt", err)
	}
}

func TestPGRepositoryInitializationBranchFinalizationRaces(t *testing.T) {
	for _, mode := range []string{"same_exact", "same_changed", "different_branches"} {
		t.Run(mode, func(t *testing.T) {
			svc, st, owner, _, _, receipt := protectedInitializationFixture(t)
			id := receipt.Repo.ID
			a := collaborationSnapshot(t, st, id, "first")
			b := collaborationSnapshot(t, st, id, "second")
			as, _ := st.GetSnapshot(owner, id, a)
			bs, _ := st.GetSnapshot(owner, id, b)
			first := initializationFinalize(receipt, as)
			second := initializationFinalize(receipt, as)
			if mode == "same_changed" {
				second = initializationFinalize(receipt, bs)
			}
			if mode == "different_branches" {
				second.Anchor.Ref.Name = "other"
				second.Anchor.Ref.BranchID = domain.LegacyContextBranchID(string(id), "other")
			}
			start := make(chan struct{})
			done := make(chan error, 2)
			var wg sync.WaitGroup
			for _, in := range []domain.RepositoryInitializationFinalize{first, second} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := svc.FinalizeRepositoryInitialization(owner, id, in)
					done <- err
				}()
			}
			close(start)
			wg.Wait()
			close(done)
			success := 0
			for err := range done {
				if err == nil {
					success++
				} else if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
					t.Fatal(err)
				}
			}
			want := 2
			if mode == "same_changed" {
				want = 1
			}
			if success != want {
				t.Fatal("wrong winners", success)
			}
			log, err := st.ReadReflog(owner, id)
			if err != nil {
				t.Fatal(err)
			}
			wantLog := 1
			if mode == "different_branches" {
				wantLog = 2
			}
			if len(log) != wantLog {
				t.Fatal("wrong reflog", len(log))
			}
		})
	}
}

func TestPGRepositoryInitializationOriginEnrichmentRecovery(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpublished", true: "modern_first"}[modern], func(t *testing.T) {
			svc, st, owner, member, request, receipt := protectedInitializationFixture(t)
			id := receipt.Repo.ID
			actor, _ := inbound.RepositoryActor(owner)
			registration := receipt.Repo
			registration.GitRemoteURL = "https://git.test/later-origin"
			if _, err := svc.EnsureRepo(owner, actor, registration); err != nil {
				t.Fatal(err)
			}
			if modern {
				if err := svc.RecordHistory(member, domain.HistoryEvent{ID: strings.Repeat("4", 32), RepoID: string(id), Kind: "birth", Branch: "feature", BranchID: "genuine-birth", CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := svc.BeginRepositoryInitialization(owner, actor, id, request)
			if err != nil || !reflect.DeepEqual(recovered, receipt) {
				t.Fatal("exact immutable Begin retry stranded", err)
			}
			changed := request
			changed.GitRemoteURL = registration.GitRemoteURL
			if _, err := svc.BeginRepositoryInitialization(owner, actor, id, changed); !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
				t.Fatal("changed request accepted", err)
			}
			got, err := svc.GetRepositoryInitialization(owner, id)
			if err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatal("GET after metadata change", err)
			}
			wrong := registration
			wrong.GitRemoteURL = "https://git.test/wrong"
			if _, err := svc.EnsureRepo(owner, actor, wrong); !errors.Is(err, domain.ErrGitOriginMismatch) {
				t.Fatal("ordinary origin guard loosened", err)
			}
			a := collaborationSnapshot(t, st, id, "explicit legacy checkpoint")
			snap, _ := st.GetSnapshot(owner, id, a)
			if _, available, err := svc.GetRepositoryInitializationView(owner, id, "main"); err != nil || !available {
				t.Fatal("enrichment unavailable", err)
			}
			if _, err := svc.FinalizeRepositoryInitialization(owner, id, initializationFinalize(receipt, snap)); err != nil {
				t.Fatal("enrichment stranded branch", err)
			}
		})
	}
}
