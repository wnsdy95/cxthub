package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestGraphMergePlacementReturnsAfterRewind(t *testing.T) {
	base := graphFixture("base")
	source := graphFixture("feature", base.ID)
	source.Branch = "feature"
	checkpoint := graphFixture("checkpoint", source.ID)
	returned := graphFixture("returned", checkpoint.ID)
	for _, through := range []string{"reflog", "history"} {
		for _, scenario := range []string{"rewound", "source only", "returned", "rewound again", "other identity"} {
			t.Run(through+"/"+scenario, func(t *testing.T) {
				v := RepositoryView{Snapshots: []Snapshot{base, source, checkpoint, returned}, Refs: []Ref{{Kind: RefBranch, Name: "main", BranchID: "main-id", Target: returned.ID}}, History: []HistoryEvent{{ID: "complete", Kind: "pr-merge", Branch: "main", BranchID: "main-id", SourceBranchID: "feature-id", Source: source.ID, Target: source.ID, SharedTarget: base.ID, PRCompleted: true, PR: &PullRequestMerge{Number: 1, HeadBranch: "feature", BaseBranch: "main"}, CreatedAt: time.Unix(10, 0)}}}
				move := func(old, next ContentHash, n int64) {
					if through == "reflog" {
						v.Reflog = append(v.Reflog, RefLogEntry{Kind: RefBranch, Name: "main", Old: old, New: next, CreatedAt: time.Unix(n, 0)})
					} else {
						v.History = append(v.History, HistoryEvent{ID: time.Unix(n, 0).String(), Kind: "advance", Branch: "main", BranchID: "main-id", Source: old, Target: next, CreatedAt: time.Unix(n, 0)})
					}
				}
				move(checkpoint.ID, base.ID, 20)
				move(base.ID, returned.ID, 30)
				wantWithdrawn := scenario != "returned"
				switch scenario {
				case "rewound":
					v.Refs[0].Target = base.ID
				case "source only":
					v.Refs[0].Target = source.ID
				case "rewound again":
					move(returned.ID, base.ID, 40)
					v.Refs[0].Target = base.ID
				case "other identity":
					v.Refs[0].Target = base.ID
					v.Refs = append(v.Refs, Ref{Kind: RefBranch, Name: "peer", BranchID: "peer-id", Target: returned.ID})
				}
				g, err := ProjectGraphState(v, "main", "")
				if err != nil || len(g.Operations.Merges) != 1 {
					t.Fatalf("merge evidence: %+v %v", g.Operations.Merges, err)
				}
				m := g.Operations.Merges[0]
				if m.Withdrawn != wantWithdrawn {
					t.Fatalf("withdrawn=%v, want %v", m.Withdrawn, wantWithdrawn)
				}
				wantChildren := []ContentHash{}
				if !wantWithdrawn {
					wantChildren = append(wantChildren, checkpoint.ID)
				}
				if !reflect.DeepEqual(m.RedirectChildren, wantChildren) {
					t.Fatalf("checkpoint placement: %v, want %v", m.RedirectChildren, wantChildren)
				}
			})
		}
	}
}
