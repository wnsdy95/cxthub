package cli

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCreationCommandSanitizesWithoutGuessing(t *testing.T) {
	tests := []struct {
		args          []string
		target, start string
		valid         bool
	}{
		{[]string{"git", "checkout", "-b", "x"}, "x", "HEAD", true},
		{[]string{"git", "switch", "-c", "x", "main"}, "x", "main", true},
		{[]string{"git", "-c", "http.extraHeader=secret", "-C", "/private/path", "branch", "x", "main"}, "x", "main", true},
		{[]string{"git", "worktree", "add", "-b", "x", "/private/path with spaces", "main"}, "x", "main", true},
		{[]string{"git", "switch", "--track", "origin/x"}, "x", "origin/x", true},
		{[]string{"git", "switch", "--orphan", "x"}, "x", "HEAD", true},
		{[]string{"git", "switch", "x"}, "x", "", false},
		{[]string{"git", "branch", "--unsupported", "secret", "x"}, "x", "", false},
		{[]string{"git", "branch", "other"}, "x", "", false},
		{[]string{"git", "branch", "x", "https://user:secret@example.test"}, "x", "", false},
	}
	for _, tt := range tests {
		c, ok := parseCreationCommand(tt.args, tt.target)
		if ok != tt.valid {
			t.Fatalf("%v valid=%v", tt.args, ok)
		}
		if ok {
			if c.StartRef != tt.start {
				t.Fatal(c)
			}
			s := strings.Join(c.Command, " ")
			if strings.Contains(s, "secret") || strings.Contains(s, "/private/") {
				t.Fatal("private argument persisted", s)
			}
		}
	}
}
func TestReadProcessArgvPreservesArgumentBoundaries(t *testing.T) {
	got, err := readProcessArgv(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, os.Args) {
		t.Fatalf("argument boundaries differ: count %d/%d", len(got), len(os.Args))
	}
}

func TestCreationUsesNamedBranchEvenAtSameGitCommit(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			cwd, c, st, repo, mainID := historyFixture(t)
			ctx := context.Background()
			runLifecycleGit(t, cwd, "switch", "-c", "feature")
			doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderCodex}}}
			featureID, err := st.PutDoc(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.PutSnapshot(ctx, domain.Snapshot{ID: featureID, DocHash: featureID, RepoID: repo, Branch: "feature"}); err != nil {
				t.Fatal(err)
			}
			if err = st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "feature", RepoID: repo, Target: featureID}); err != nil {
				t.Fatal(err)
			}
			if err = st.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "feature"}); err != nil {
				t.Fatal(err)
			}
			hooks := t.TempDir()
			pidFile := filepath.Join(hooks, "git.pid")
			release := filepath.Join(hooks, "release")
			script := "#!/bin/sh\nif test \"$1\" = prepared; then\n echo \"$PPID\" > '" + pidFile + "'\n while ! test -f '" + release + "'; do sleep 0.02; done\nfi\n"
			if err = os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"-C", cwd, "-c", "core.hooksPath=" + hooks, "branch", "new"}
			if explicit {
				args = append(args, "main")
			}
			cmd := exec.Command("git", args...)
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				os.WriteFile(release, []byte("done"), 0600)
				if err := cmd.Wait(); err != nil {
					t.Error(err)
				}
			}()
			var pid string
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				raw, _ := os.ReadFile(pidFile)
				pid = strings.TrimSpace(string(raw))
				if pid != "" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid == "" {
				t.Fatal("Git never reached prepared hook")
			}
			oid := gitOut(cwd, "rev-parse", "HEAD")
			e, err := prepareBranchHistory(ctx, c, cwd, "new", oid, false, pid)
			if err != nil {
				t.Fatal(err)
			}
			want, origin := featureID, "feature"
			if explicit {
				want, origin = mainID, "main"
			}
			if e.Source != want || e.Creation == nil || e.Creation.Evidence != "process-argv" || e.Creation.OriginBranch != origin || e.Creation.OriginBranchID == "" {
				t.Fatalf("origin not retained: %+v creation=%+v", e, e.Creation)
			}
		})
	}
}
