package githooks

import (
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// What the wait for a push relies on, seen with the generated hook script and
// real Git over each way of pushing: the process the pre-push hook names is
// the one that pushes, and when it has ended this clone holds the branch as
// origin has it. Git runs pre-push before the push and has no hook after it,
// so cxt waits for that process and reads the clone then.
//
// Smart HTTP is served by Git's own git-http-backend; a push over http and
// one over https are made by the same helper program. For ssh a stand-in for
// the ssh program runs the remote command here: Git's side of the transport,
// which starts that program and waits for it, is the real one.
func TestPrePushNamesThePushingProcessOverEveryTransport(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"file", "http", "ssh"} {
		t.Run(transport, func(t *testing.T) {
			root := t.TempDir()
			repo, remote := filepath.Join(root, "work"), filepath.Join(root, "bare.git")
			var env []string
			for _, entry := range cleanHookTestEnv() {
				key := strings.SplitN(entry, "=", 2)[0]
				if key == "PATH" || key == "TMPDIR" || strings.HasPrefix(key, "GIT_CONFIG_") {
					env = append(env, entry)
				}
			}
			env = append(env, "HOME="+root, "XDG_CONFIG_HOME="+root, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0",
				"REVIEW_CXT_ARGS="+filepath.Join(root, "cxt-args"), "REVIEW_CXT_PUSHING="+filepath.Join(root, "cxt-pushing"))
			must := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command(git, args...)
				cmd.Dir, cmd.Env = dir, env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %q: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			must(root, "init", "--bare", remote)
			must(root, "init", "-b", "main", repo)
			must(repo, "config", "user.name", "Synthetic Hook Reviewer")
			must(repo, "config", "user.email", "reviewer@example.invalid")
			must(repo, "config", "commit.gpgsign", "false")
			must(repo, "commit", "--allow-empty", "-m", "synthetic base")
			head := must(repo, "rev-parse", "HEAD")

			url := remote
			switch transport {
			case "http":
				backend := filepath.Join(must(root, "--exec-path"), "git-http-backend")
				if _, err := os.Stat(backend); err != nil {
					t.Skipf("Git's HTTP server program is not installed beside git: %v", err)
				}
				must(remote, "config", "http.receivepack", "true")
				server := httptest.NewServer(&cgi.Handler{Path: backend, Env: append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, env...)})
				defer server.Close()
				url = server.URL + "/bare.git"
			case "ssh":
				standIn := filepath.Join(root, "ssh-stand-in")
				writeHookTestFile(t, standIn, "#!/bin/sh\nshift\nexec sh -c \"$*\"\n")
				// The simple variant is given the host and the command, no options.
				env = append(env, "GIT_SSH_COMMAND="+shellQuote(standIn), "GIT_SSH_VARIANT=simple")
				url = "ssh://reviewer@host.invalid" + remote
			}
			must(repo, "remote", "add", "origin", url)

			// The stand-in for cxt notes what it was called with and whether
			// the process the hook named runs at that moment.
			standInCxt := filepath.Join(root, "cxt-stand-in")
			writeHookTestFile(t, standInCxt, "#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$REVIEW_CXT_ARGS\"\n"+
				"if kill -0 \"$4\" 2>/dev/null; then echo runs; else echo ended; fi > \"$REVIEW_CXT_PUSHING\"\n")
			writeHookTestFile(t, filepath.Join(repo, ".git", "hooks", "pre-push"), script(standInCxt))

			push := exec.Command(git, "push", "origin", "main")
			push.Dir, push.Env = repo, env
			var output strings.Builder
			push.Stdout, push.Stderr = &output, &output
			if err := push.Start(); err != nil {
				t.Fatal(err)
			}
			pushing := push.Process.Pid
			if err := push.Wait(); err != nil {
				t.Fatalf("git push: %v\n%s", err, output.String())
			}

			raw, err := os.ReadFile(filepath.Join(root, "cxt-args"))
			if err != nil {
				t.Fatalf("the hook did not call cxt: %v\n%s", err, output.String())
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
			if !reflect.DeepEqual(args, []string{"git-hook", "wake", "pre-push", strconv.Itoa(pushing)}) {
				t.Fatalf("the hook called cxt with %q; the process that pushes is %d", args, pushing)
			}
			if during, err := os.ReadFile(filepath.Join(root, "cxt-pushing")); err != nil || string(during) != "runs\n" {
				t.Fatalf("during the hook the process it named: %q %v", during, err)
			}
			if err := syscall.Kill(pushing, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("the process that pushed is still there after the push: %v", err)
			}
			// The process has ended: what it pushed is on origin, and this
			// clone already holds origin's branch at that commit.
			if got := must(remote, "rev-parse", "refs/heads/main"); got != head {
				t.Fatalf("origin holds %s, pushed %s", got, head)
			}
			if got := must(repo, "rev-parse", "refs/remotes/origin/main"); got != head {
				t.Fatalf("this clone holds origin's branch at %s after the push of %s", got, head)
			}
		})
	}
}
