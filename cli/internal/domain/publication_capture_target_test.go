package domain

import "testing"

func TestPublicationFrozenCaptureTarget(t *testing.T) {
	for _, mode := range []string{"exact", "advanced", "missing", "foreign", "history-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newPublicationFixture()
			f.in.Scope.ExpectedTargets = map[string]ContentHash{"R": f.a}
			switch mode {
			case "advanced":
				f.in.Refs[0].Target = f.b
			case "missing":
				f.in.Scope.ExpectedTargets["R"] = ""
			case "foreign":
				f.in.Scope.ExpectedTargets = map[string]ContentHash{"another": f.a}
			case "history-only":
				f.in.Scope.HistoryOnly = true
			}
			if mode == "exact" {
				p := planMust(t, f.in)
				if len(p.RefsToPush) != 1 || p.RefsToPush[0].Target != f.a {
					t.Fatal("lost capture target")
				}
			} else if mode == "history-only" {
				planMustFail(t, f.in, ErrInvalidRef)
			} else {
				planMustFail(t, f.in, ErrSyncConflict)
			}
		})
	}
}
