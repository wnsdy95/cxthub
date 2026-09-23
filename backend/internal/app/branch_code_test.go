package app

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestBranchCodeUsesCompletedPRToDisambiguateTrackingAlias(t *testing.T) {
	ref := domain.Ref{RepoID: hh("repo"), BranchID: "main-id", Target: hh("context")}
	publication := domain.HistoryEvent{Kind: "publish", BranchID: ref.BranchID, Source: ref.Target, Target: ref.Target, GitAfter: effectiveOID(9)}
	completion := domain.HistoryEvent{Kind: "pr-merge", BranchID: ref.BranchID, SourceBranchID: ref.BranchID, Source: ref.Target, Target: ref.Target, PRCompleted: true,
		PR: &domain.PullRequestMerge{HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(3)}}
	for _, name := range []string{"completed", "reversed delivery", "incomplete", "other identity", "other source", "unrelated code", "conflicting completions"} {
		t.Run(name, func(t *testing.T) {
			p, c := publication, completion
			history := []domain.HistoryEvent{p, c}
			want := effectiveOID(3)
			switch name {
			case "reversed delivery":
				history = []domain.HistoryEvent{c, p}
			case "incomplete":
				history[1].PRCompleted = false
				want = p.GitAfter
			case "other identity":
				history[1].BranchID = "other-id"
				want = p.GitAfter
			case "other source":
				history[1].Source = hh("other")
				want = ""
			case "unrelated code":
				history = append(history, domain.HistoryEvent{Kind: "position", BranchID: ref.BranchID, Target: ref.Target, GitAfter: effectiveOID(8)})
				want = ""
			case "conflicting completions":
				other := c
				other.PR = &domain.PullRequestMerge{HeadSHA: effectiveOID(9), MergeSHA: effectiveOID(4)}
				history = append(history, other)
				want = ""
			}
			if got := branchCode(ref, history); got != want {
				t.Fatalf("code=%q want %q", got, want)
			}
		})
	}
}

func TestTrackingAliasPromotionKeepsSharedGraphAndMemoryIncluded(t *testing.T) {
	f, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	ref.Target = f.b
	if err := f.st.CompareAndSwapRef(ctx, f.repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	// The tracking alias shares main's identity. Its source publication and
	// completed squash integration legitimately associate one context with two
	// different Git SHAs. Delivery order must not hide every earlier merge.
	history = append(history, domain.HistoryEvent{ID: fmt.Sprintf("%032x", 301), RepoID: string(f.repo), Branch: "main", BranchID: ref.BranchID, Kind: "publish", Source: ref.Target, Target: ref.Target, GitAfter: effectiveOID(9), CreatedAt: time.Unix(20, 0)})
	for _, e := range history {
		if err := f.st.ApplyHistoryEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	full, err := f.svc.GetRepositoryView(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.svc.GetPendingView(ctx, f.repo)
	if err != nil || !reflect.DeepEqual(full.Graph, live.Graph) {
		t.Fatalf("full/live mismatch: %v", err)
	}
	in := full.Graph.BranchContexts["main"]
	if in.CodeCommit != effectiveOID(3) {
		t.Fatalf("automatic merged position: %+v", in)
	}
	for _, m := range in.Merges {
		if m.State != "included" {
			t.Fatalf("completed context lost: %+v", m)
		}
	}
	memory, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", ref.Target, "")
	if err != nil || !reflect.DeepEqual(memory.Inclusion, &in) {
		t.Fatalf("shared memory differs: %+v %v", memory.Inclusion, err)
	}
	for _, text := range []string{"PRE-MERGE MAIN", "MERGED A", "MERGED B"} {
		if !strings.Contains(memory.Digest.Summary, text) {
			t.Fatalf("memory lost %q", text)
		}
	}
	past, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", ref.Target, effectiveOID(2))
	if err != nil {
		t.Fatal(err)
	}
	if past.Inclusion.CodeCommit != effectiveOID(2) {
		t.Fatal("explicit historical code was replaced")
	}
	for _, m := range past.Inclusion.Merges {
		if m.PRNumber == 11 && m.State != "not_selected" {
			t.Fatalf("later merge included at earlier code: %+v", m)
		}
	}
}
