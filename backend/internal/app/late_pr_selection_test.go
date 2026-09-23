package app

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestResolvedBranchCodeRequiresCausalAndGitEvidence(t *testing.T) {
	for _, scenario := range []string{"reversed history", "multiple older code observations", "missing Git evidence", "conflicting ordinary selection", "different branch identity", "cyclic receipts"} {
		t.Run(scenario, func(t *testing.T) {
			f, ref, history := branchContextFixture(t)
			ref.Target = f.a
			for i := range history {
				if history[i].PRCompleted && history[i].PR.Number == 10 {
					history[i].SharedTarget = f.b
				}
			}
			want := effectiveOID(3)
			switch scenario {
			case "reversed history":
				for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
					history[i], history[j] = history[j], history[i]
				}
			case "missing Git evidence":
				for i := range history {
					if history[i].PR.Number == 11 {
						copy := *history[i].PR
						copy.MergeSHA = effectiveOID(99)
						history[i].PR = &copy
					}
				}
				want = ""
			case "conflicting ordinary selection":
				history = append(history, domain.HistoryEvent{BranchID: ref.BranchID, Target: ref.Target, Kind: "position", GitAfter: effectiveOID(98)})
				want = ""
			case "multiple older code observations":
				for _, code := range []int{1, 2} {
					history = append(history, domain.HistoryEvent{BranchID: ref.BranchID, Target: hh("pre-merge-main"), Kind: "position", GitAfter: effectiveOID(code)})
				}
			case "different branch identity":
				for i := range history {
					if history[i].PR.Number == 11 {
						history[i].BranchID = "another-identity"
					}
				}
				want = effectiveOID(2)
			case "cyclic receipts":
				for i := range history {
					if history[i].PRCompleted && history[i].PR.Number == 11 {
						history[i].SharedTarget = f.a
					}
				}
				want = ""
			}
			evidence, err := f.svc.newCodeEvidence(systemTestContext(), f.repo)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolvedBranchCode(systemTestContext(), ref, history, evidence)
			if err != nil || got != want {
				t.Fatalf("got %q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestLatePRPromotionKeepsNewestVerifiedCodeAcrossQueries(t *testing.T) {
	for _, alreadyContained := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyContained), func(t *testing.T) {
			ctx := systemTestContext()
			f := newEffectiveFixture(t)
			base := prSnapshot(t, f.st, f.repo, "BASE CONTEXT")
			older := prSnapshot(t, f.st, f.repo, "OLDER CONTEXT", base)
			parent := base
			if alreadyContained {
				parent = older
			}
			newer := prSnapshot(t, f.st, f.repo, "NEWER CONTEXT", parent)
			ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", Target: base}
			if err := f.st.CompareAndSwapRef(ctx, f.repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			for id, text := range map[domain.ContentHash]string{older: "OLDER MEMORY", newer: "NEWER MEMORY"} {
				if _, err := f.svc.PutMemoryDigest(ctx, f.repo, domain.MemoryDigest{SnapshotID: id, Summary: text}); err != nil {
					t.Fatal(err)
				}
			}
			f.publish(t, older, 8, "older")
			f.publish(t, newer, 9, "newer")
			for _, p := range []domain.PullRequestMerge{{Number: 102, BaseBranch: "main", HeadBranch: "newer", HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(3)}, {Number: 101, BaseBranch: "main", HeadBranch: "older", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(2)}} {
				if _, err := f.svc.PromoteRepositoryPR(ctx, f.repo, p); err != nil {
					t.Fatal(err)
				}
			}
			full, err := f.svc.GetRepositoryView(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			got := full.Graph.BranchContexts["main"]
			if got.CodeCommit != effectiveOID(3) {
				t.Fatalf("late PR rewound shared code: %+v", got)
			}
			for _, m := range got.Merges {
				if m.State != "included" {
					t.Fatalf("lost merge: %+v", m)
				}
			}
			live, err := f.svc.GetPendingView(ctx, f.repo)
			if err != nil || !reflect.DeepEqual(full.Graph, live.Graph) {
				t.Fatalf("live mismatch: %v", err)
			}
			query, err := f.svc.QueryContext(ctx, f.repo, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
			if err != nil || !reflect.DeepEqual(query.Inclusion, &got) {
				t.Fatalf("context mismatch: %+v %v", query.Inclusion, err)
			}
			memory, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", got.SnapshotID, "")
			if err != nil || !reflect.DeepEqual(memory.Inclusion, &got) {
				t.Fatalf("memory mismatch: %+v %v", memory.Inclusion, err)
			}
			for _, text := range []string{"OLDER MEMORY", "NEWER MEMORY"} {
				if !strings.Contains(memory.Digest.Summary, text) {
					t.Fatalf("memory missing %q", text)
				}
			}
			past, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", got.SnapshotID, effectiveOID(2))
			if err != nil {
				t.Fatal(err)
			}
			if past.Inclusion.CodeCommit != effectiveOID(2) {
				t.Fatal("explicit historical selection changed")
			}
			for _, m := range past.Inclusion.Merges {
				if m.PRNumber == 102 && m.State != "not_selected" {
					t.Fatalf("future merge included: %+v", m)
				}
			}
		})
	}
}
