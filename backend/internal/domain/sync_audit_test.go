package domain

import (
	"strings"
	"testing"
)

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
