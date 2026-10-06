package domain

import "testing"

func TestPublicationScopeIdentityShapeWithoutFinalRef(t *testing.T) {
	for _, id := range []string{"R\rX", "R\nX"} {
		f := newPublicationFixture()
		f.in.Scope.HistoryOnly = true
		f.in.Refs = nil
		f.in.Scope.Branches[0].BranchID = id
		f.in.History[0].BranchID = id
		if p, err := PlanPublication(f.in); err == nil || len(p.Authority) != 0 {
			t.Fatalf("invalid history-only identity authorized: %v %v", p, err)
		}
	}
}

func TestPublicationLocalRefCannotAdoptAcceptedIdentity(t *testing.T) {
	for _, reused := range []bool{false, true} {
		f := newPublicationFixture()
		localID := LegacyContextBranchID(f.in.RepoID, "feature")
		f.in.History[0].Kind = "position"
		f.in.History[0].BranchID = localID
		f.in.Refs[0].BranchID = ""
		f.in.Scope = PublicationScope{}
		f.in.Ref = "feature"
		before := planMust(t, f.in)
		if before.Authority[0].BranchID != localID {
			t.Fatal("invalid local control")
		}
		birth := f.event("birth", "X", "feature", f.b)
		if reused {
			archive := f.event("archive", localID, "feature", "")
			archive.Source = f.a
			birth.BindingParent = archive.ID
			f.in.Accepted = append(f.in.Accepted, archive)
		}
		f.in.Accepted = append(f.in.Accepted, birth)
		planMustFail(t, f.in, ErrSyncConflict)
	}
}

func TestPublicationRefRejectsHistoryOnlyMode(t *testing.T) {
	f := newPublicationFixture()
	f.in.Scope = PublicationScope{}
	f.in.Ref = "feature"
	if p := planMust(t, f.in); len(p.RefsToPush) != 1 {
		t.Fatal("invalid final-ref control")
	}
	f.in.Scope.HistoryOnly = true
	planMustFail(t, f.in, ErrInvalidRef)
}
