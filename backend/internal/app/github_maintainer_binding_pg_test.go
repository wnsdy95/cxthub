//go:build postgres

package app

import (
	"testing"
	"time"
)

func TestPGGitHubBindingIsFoundByItsRepository(t *testing.T) {
	_, st, _ := collaborationPG(t)
	githubBindingLookup(t, st, makeTeamFixture(t, st), time.Now().UnixMicro())
}
