package cli

import (
	"testing"
)

func TestOrphanRecoveryRequiresExplicitFlag(t *testing.T) {
	for _, args := range [][]string{{"cxt", "branch", "recover", "id"}, {"cxt", "branch", "replay", "--confirm-orphan"}, {"cxt", "branch", "recover", "id", "--confirm-orphan", "--provider", "claude"}} {
		if _, err := PreflightArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
