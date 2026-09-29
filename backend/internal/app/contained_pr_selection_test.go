package app

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// An already-contained squash does not move the destination to its source.
// A later merge can still encounter that source as an older code observation.
func containedSelectionFixture(t *testing.T) (*effectiveFixture, domain.Ref, []domain.HistoryEvent) {
	t.Helper()
	f, ref, history := branchContextFixture(t)
	ref.Target = f.b
	if err := f.st.AddGraftParents(systemTestContext(), f.repo, f.b, []domain.ContentHash{f.a}); err != nil {
		t.Fatal(err)
	}
	for i := range history {
		if !history[i].PRCompleted {
			continue
		}
		if history[i].PR.Number == 10 {
			history[i].Target = f.b // source A already belongs to destination B
			history[i].SharedTarget = f.b
		} else {
			history[i].SharedTarget = f.a
		}
	}
	history = append(history, domain.HistoryEvent{RepoID: string(f.repo), ID: "00000000000000000000000000000300", Branch: ref.Name, BranchID: ref.BranchID, Kind: "position", Target: f.a, GitAfter: effectiveOID(8), CreatedAt: time.Unix(40, 0)})
	return f, ref, history
}

func TestContainedSquashResolvesOnlyCausalSourceCode(t *testing.T) {
	for _, scenario := range []string{"contained source", "reversed history", "unchanged Git SHA", "unrelated older code", "different source", "different destination", "missing receipt", "missing completion", "source selected before merge", "missing Git evidence"} {
		t.Run(scenario, func(t *testing.T) {
			f, ref, history := containedSelectionFixture(t)
			want := effectiveOID(3)
			switch scenario {
			case "unchanged Git SHA":
				filtered := []domain.HistoryEvent{}
				for _, h := range history {
					if h.PR != nil && h.PR.Number == 11 {
						pr := *h.PR
						pr.HeadSHA = pr.MergeSHA
						h.PR = &pr
						filtered = append(filtered, h)
					}
				}
				history = filtered
			case "reversed history":
				for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
					history[i], history[j] = history[j], history[i]
				}
			case "unrelated older code":
				history[len(history)-1].GitAfter = effectiveOID(9)
				want = ""
			case "different source":
				for i := range history {
					if history[i].PR != nil && history[i].PR.Number == 10 {
						history[i].Source = f.root
					}
				}
				want = ""
			case "different destination":
				for i := range history {
					if history[i].PR != nil && history[i].PR.Number == 10 {
						history[i].BranchID = "another-branch"
					}
				}
				want = ""
			case "missing receipt", "missing completion":
				filtered := history[:0]
				for _, h := range history {
					if h.PR != nil && h.PR.Number == 10 && h.PRCompleted == (scenario == "missing completion") {
						continue
					}
					filtered = append(filtered, h)
				}
				history = filtered
				want = ""
			case "source selected before merge":
				ref.Target = f.a
				want = effectiveOID(8)
			case "missing Git evidence":
				for i := range history {
					if history[i].PR != nil && history[i].PR.Number == 10 {
						pr := *history[i].PR
						pr.MergeSHA = effectiveOID(99)
						history[i].PR = &pr
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
				t.Fatalf("code=%q, want %q: %v", got, want, err)
			}
		})
	}
}

func TestContainedSquashFullLiveContextAndMemoryAgree(t *testing.T) {
	f, ref, history := containedSelectionFixture(t)
	ctx := systemTestContext()
	if err := f.st.CompareAndSwapRef(ctx, f.repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	for _, h := range history {
		if err := f.st.ApplyHistoryEvent(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	storedBefore, err := f.st.ListSnapshots(ctx, f.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	full, err := f.svc.GetRepositoryView(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	in := full.Graph.BranchContexts["main"]
	if in.CodeCommit != effectiveOID(3) || len(full.Graph.Integrations) != 1 {
		t.Fatalf("lost integrated main: %+v", in)
	}
	for _, m := range in.Merges {
		if m.State != "included" {
			t.Fatalf("lost PR: %+v", m)
		}
	}
	live, err := f.svc.GetPendingView(ctx, f.repo)
	if err != nil || !reflect.DeepEqual(full.Graph, live.Graph) {
		t.Fatalf("full/live mismatch: %v", err)
	}
	context, err := f.svc.QueryContext(ctx, f.repo, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
	if err != nil || !reflect.DeepEqual(context.Inclusion, &in) {
		t.Fatalf("context mismatch: %v", err)
	}
	memory, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", ref.Target, "")
	if err != nil || !reflect.DeepEqual(memory.Inclusion, &in) {
		t.Fatalf("memory mismatch: %v", err)
	}
	for _, text := range []string{"MERGED A", "MERGED B"} {
		if !strings.Contains(memory.Digest.Summary, text) {
			t.Fatalf("missing memory %q", text)
		}
	}
	past, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", ref.Target, effectiveOID(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range past.Inclusion.Merges {
		if m.State != "not_selected" {
			t.Fatal("explicit past selection included a future PR")
		}
	}
	storedAfter, err := f.st.ListSnapshots(ctx, f.repo, "")
	if err != nil || !reflect.DeepEqual(storedBefore, storedAfter) {
		t.Fatalf("query changed stored snapshots: %v", err)
	}
}
