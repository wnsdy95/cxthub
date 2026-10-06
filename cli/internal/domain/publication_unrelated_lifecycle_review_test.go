package domain

import (
	"errors"
	"testing"
)

func reviewUnrelatedLifecycle() *publicationFixture {
	f := newPublicationFixture()
	f.in.Scope.Branches[0].Branch = "main"
	f.in.History[0].Branch = "main"
	f.in.Refs[0].Name = "main"
	server := f.event("birth", "server-web-X", "web-fork-x", f.b)
	local := f.event("birth", "local-web-X", "web-fork-x", f.b)
	rename := f.event("rename", local.BranchID, "web-fork-renamed", f.b)
	rename.PreviousBranch, rename.BindingParent = "web-fork-x", local.ID
	archive := f.event("archive", local.BranchID, "web-fork-renamed", f.b)
	archive.BindingParent = rename.ID
	f.in.Accepted = []HistoryEvent{server}
	f.in.History = append(f.in.History, local, rename, archive)
	return f
}

func TestReviewSelectedPublicationIgnoresUnrelatedLifecycleDisagreement(t *testing.T) {
	for _, mode := range []string{"identity", "manual-ref", "history-only", "idless-local-ref", "ordinary-foreign-witness"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewUnrelatedLifecycle()
			if _, err := ProjectContextBranches(f.in.History); err != nil {
				t.Fatal("local history is not valid", err)
			}
			if _, err := ProjectContextBranches(f.in.Accepted); err != nil {
				t.Fatal("accepted history is not valid", err)
			}
			switch mode {
			case "manual-ref":
				f.in.Scope = PublicationScope{}
				f.in.Ref = "main"
			case "history-only":
				f.in.Scope.HistoryOnly = true
				f.in.Refs = nil
			case "idless-local-ref":
				f.in.Scope = PublicationScope{}
				f.in.Ref = "main"
				f.in.Refs[0].BranchID = ""
			case "ordinary-foreign-witness":
				proof := f.event("position", "local-web-X", "web-fork-x", f.b)
				f.in.History = append(f.in.History, proof)
				f.in.History[0].Creation = &GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", "main", "web-fork-x"}, StartRef: "web-fork-x", StartCommit: proof.GitAfter, OriginBranch: "web-fork-x", OriginBranchID: proof.BranchID}
			}
			plan, err := PlanPublication(f.in)
			if err != nil {
				t.Fatalf("independent main was blocked by foreign lifecycle: %v", err)
			}
			for _, e := range plan.HistoryToSend {
				if e.BranchID != "R" && !(mode == "ordinary-foreign-witness" && e.Kind == "position" && e.BranchID == "local-web-X") {
					t.Fatalf("foreign effect authorized: %+v", e)
				}
			}
			if len(plan.Authority) != 1 || plan.Authority[0].BranchID != "R" {
				t.Fatalf("authority widened: %+v", plan)
			}
		})
	}
}

func TestReviewSelectedPublicationStillRejectsRelevantDisagreement(t *testing.T) {
	for _, mode := range []string{"selected-name", "selected-previous-name", "immutable-ID-collision", "explicit-foreign-dependency"} {
		t.Run(mode, func(t *testing.T) {
			f := newPublicationFixture()
			switch mode {
			case "selected-name":
				f.in.Accepted = []HistoryEvent{f.event("birth", "other", "feature", f.b)}
			case "selected-previous-name":
				f.in.History[0].Branch = "prior-name"
				rename := f.event("rename", "R", "feature", f.a)
				rename.PreviousBranch, rename.BindingParent = "prior-name", f.in.History[0].ID
				f.in.History = append(f.in.History, rename)
				f.in.Accepted = []HistoryEvent{f.event("birth", "other", "prior-name", f.b)}
			case "immutable-ID-collision":
				other := f.in.History[0]
				other.Target = f.b
				f.in.Accepted = []HistoryEvent{other}
			case "explicit-foreign-dependency":
				foreign := f.event("birth", "X", "foreign", f.b)
				f.in.History = append(f.in.History, foreign)
				f.in.History[0].Creation = &GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", "feature", "foreign"}, StartRef: "foreign", StartCommit: f.in.History[0].GitAfter, OriginBranch: "foreign", OriginBranchID: "X"}
			}
			p, err := PlanPublication(f.in)
			if err == nil || len(p.Authority) != 0 {
				t.Fatalf("relevant conflict escaped: %+v %v", p, err)
			}
			if mode == "immutable-ID-collision" && !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("wrong collision cause: %v", err)
			}
		})
	}
}
