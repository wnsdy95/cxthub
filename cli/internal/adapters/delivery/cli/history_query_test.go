package cli

import "testing"

func TestHistoryQueryCommandScopes(t *testing.T) {
	for _, cmd := range []string{"log", "list"} {
		for _, args := range [][]string{{}, {"HEAD"}, {"--branch", "main", "--server", "--json"}, {"--all"}, {"--retained", "--json"}} {
			p, _, err := parseCommand(cmd, args)
			if err != nil || p.effect != commandRead {
				t.Fatal(cmd, args, p, err)
			}
		}
		for _, args := range [][]string{{"HEAD", "--branch", "main"}, {"--all", "--retained"}, {"--all", "main"}, {"--server", "--all"}, {"main", "feature"}, {"--server", "--server"}} {
			if _, _, err := parseCommand(cmd, args); err == nil {
				t.Fatal("accepted", cmd, args)
			}
		}
	}
}
