package domain

import (
	"strings"
	"testing"
	"time"
)

func TestAuditCatchesValidButIncorrectBirthProjection(t *testing.T) {
	source := HashContent([]byte("source"))
	wrong := HashContent([]byte("wrong"))
	e := HistoryEvent{ID: strings.Repeat("a", 32), BranchID: strings.Repeat("a", 32), Branch: "feature", RepoID: string(HashContent([]byte("repo"))), Kind: "birth", Source: source, Target: source, CreatedAt: time.Now().UTC()}
	v := RepositoryView{History: []HistoryEvent{e}, Snapshots: []Snapshot{{ID: source}, {ID: wrong}}}
	g := GraphState{Operations: GraphOperations{Births: []GraphBirth{{EventID: e.ID, Source: wrong}}}}
	checks := AuditGraphContracts(v, &g)
	found := false
	for _, c := range checks {
		if c.Code == "birth_projection_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatal("structurally valid but wrong origin passed", checks)
	}
	g.Operations.Births[0].Source = source
	for _, c := range AuditGraphContracts(v, &g) {
		if c.State == "mismatch" {
			t.Fatal(c)
		}
	}
}
func TestAuditIncludedContextAndGraphAreIndependent(t *testing.T) {
	id := HashContent([]byte("source"))
	g := GraphState{BranchContexts: map[string]BranchContext{"main": {BranchID: "main", Merges: []BranchContextMerge{{EventID: "event", Source: id, State: "included"}}}}}
	checks := AuditIntegrationContracts(g)
	if len(checks) != 2 {
		t.Fatal("missing context and path passed", checks)
	}
	c := g.BranchContexts["main"]
	c.Merges[0].State = "not_selected"
	g.BranchContexts["main"] = c
	if len(AuditIntegrationContracts(g)) != 0 {
		t.Fatal("withdrawn PR treated as current inclusion")
	}
}

func TestCreationOriginSurvivesRenameButRejectsConflictingIdentity(t *testing.T) {
	e := HistoryEvent{RepoID: "repo", Kind: "birth", BranchID: "new", Creation: &GitCreation{Evidence: "process-argv", OriginBranch: "old-name", OriginBranchID: "origin"}}
	prior := HistoryEvent{RepoID: "repo", Kind: "rename", BranchID: "origin", Branch: "renamed", PreviousBranch: "old-name"}
	if err := ValidateCreationOrigin([]HistoryEvent{prior}, e); err != nil {
		t.Fatal(err)
	}
	e.Creation.OriginBranchID = "wrong"
	if ValidateCreationOrigin([]HistoryEvent{prior}, e) == nil {
		t.Fatal("unknown origin accepted")
	}
	e.Creation.OriginBranchID = e.BranchID
	if ValidateCreationOrigin(nil, e) == nil {
		t.Fatal("self origin accepted")
	}
}

func TestCreationCommandMustMatchCheckpointAndOrphanStart(t *testing.T) {
	before, start := strings.Repeat("a", 40), strings.Repeat("b", 40)
	e := HistoryEvent{Kind: "orphan", Branch: "new", GitBefore: before, Creation: &GitCreation{Evidence: "process-argv", Command: []string{"git", "checkout", "--orphan", "new", "main"}, StartRef: "main", StartCommit: start}}
	if err := ValidateGitCreation(e); err != nil {
		t.Fatal("explicit orphan start was confused with previous HEAD", err)
	}
	e.Creation.StartRef = "other"
	if ValidateGitCreation(e) == nil {
		t.Fatal("command/start contradiction passed")
	}
	e.Creation.StartRef = "main"
	e.Branch = "another"
	if ValidateGitCreation(e) == nil {
		t.Fatal("wrong command target passed")
	}
}
