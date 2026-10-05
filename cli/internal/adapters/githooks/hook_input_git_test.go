package githooks

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The only remote is an owned local bare repository. Every git push executes
// the actual generated managed pre-push hook; no hooksPath or --no-verify bypass.
func TestManagedPrePushWithLocalGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "local fixture ' with spaces")
	repo, remote := filepath.Join(root, "work"), filepath.Join(root, "bare ' remote.git")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Reuse the hook test isolation policy and whitelist only process essentials
	// and its synthetic Git configuration. No inherited provider/SSH environment.
	var env []string
	for _, entry := range cleanHookTestEnv() {
		key := strings.SplitN(entry, "=", 2)[0]
		if key == "PATH" || key == "TMPDIR" || strings.HasPrefix(key, "GIT_CONFIG_") {
			env = append(env, entry)
		}
	}
	env = append(env, "HOME="+root, "XDG_CONFIG_HOME="+root, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0",
		"REVIEW_STATUS_FILE="+filepath.Join(root, "user-status"),
		"REVIEW_USER_INPUT="+filepath.Join(root, "user-input"),
		"REVIEW_USER_ARGS="+filepath.Join(root, "user-args"),
		"REVIEW_CXT_INPUT="+filepath.Join(root, "cxt-input"),
		"REVIEW_CXT_ARGS="+filepath.Join(root, "cxt-args"),
		"REVIEW_CXT_CALLS="+filepath.Join(root, "cxt-calls"))
	run := func(dir string, args ...string) ([]byte, error) {
		cmd := exec.Command(git, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := run(dir, args...)
		if err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	must(root, "init", "--bare", remote)
	must(root, "init", "-b", "main", repo)
	must(repo, "config", "user.name", "Synthetic Hook Reviewer")
	must(repo, "config", "user.email", "reviewer@example.invalid")
	must(repo, "config", "commit.gpgsign", "false")
	must(repo, "config", "tag.gpgsign", "false")
	must(repo, "commit", "--allow-empty", "-m", "synthetic base")
	head := must(repo, "rev-parse", "HEAD")
	zero := strings.Repeat("0", len(head))
	must(repo, "branch", "topic-one")
	must(repo, "branch", "topic-two")
	must(repo, "tag", "review-tag")
	must(repo, "remote", "add", "origin", remote)

	mockCxt := filepath.Join(root, "mock cxt ' executable")
	write(mockCxt, "#!/bin/sh\ncat > \"$REVIEW_CXT_INPUT\"\nprintf '%s\\0' \"$@\" > \"$REVIEW_CXT_ARGS\"\nprintf 'called\\n' >> \"$REVIEW_CXT_CALLS\"\nexit 23\n", 0700)
	hook := filepath.Join(repo, ".git", "hooks", "pre-push")
	write(hook, script("pre-push", mockCxt), 0700)
	write(hook+".pre-cxt", "#!/bin/sh\ncat > \"$REVIEW_USER_INPUT\"\nprintf '%s\\0' \"$@\" > \"$REVIEW_USER_ARGS\"\nexit \"$(cat \"$REVIEW_STATUS_FILE\")\"\n", 0700)
	write(filepath.Join(root, "user-status"), "0\n", 0600)
	argv := func(name string) []string {
		return strings.Split(strings.TrimSuffix(string(read(name)), "\x00"), "\x00")
	}
	checkArgs := func() {
		t.Helper()
		if got := argv("user-args"); !reflect.DeepEqual(got, []string{"origin", remote}) {
			t.Fatalf("user args=%q", got)
		}
		if got := argv("cxt-args"); !reflect.DeepEqual(got, []string{"git-hook", "pre-push", "origin", remote}) {
			t.Fatalf("cxt args=%q", got)
		}
	}
	checkRows := func(want map[string][]string, expectCxt bool) {
		t.Helper()
		input := read("user-input")
		if expectCxt && !bytes.Equal(input, read("cxt-input")) {
			t.Fatalf("stdin not replayed exactly: user=%q cxt=%q", input, read("cxt-input"))
		}
		rows := map[string][]string{}
		for _, line := range strings.Split(strings.TrimSuffix(string(input), "\n"), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 4 {
				t.Fatalf("not a Git pre-push record: %q", line)
			}
			if _, exists := rows[fields[2]]; exists {
				t.Fatalf("duplicate row: %q", line)
			}
			rows[fields[2]] = fields
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("rows=%v want=%v", rows, want)
		}
	}
	remoteRefs := func() string {
		return must(remote, "for-each-ref", "--sort=refname", "--format=%(refname) %(objectname)")
	}

	// The mocked CXT exits 23 on each invocation: a successful local push also
	// proves that only the user's hook may veto Git publication.
	must(repo, "push", "origin", "refs/heads/topic-one", "refs/heads/topic-two", "refs/tags/review-tag")
	checkRows(map[string][]string{
		"refs/heads/topic-one": {"refs/heads/topic-one", head, "refs/heads/topic-one", zero},
		"refs/heads/topic-two": {"refs/heads/topic-two", head, "refs/heads/topic-two", zero},
		"refs/tags/review-tag": {"refs/tags/review-tag", head, "refs/tags/review-tag", zero},
	}, true)
	checkArgs()
	wantRefs := "refs/heads/topic-one " + head + "\nrefs/heads/topic-two " + head + "\nrefs/tags/review-tag " + head
	if got := remoteRefs(); got != wantRefs {
		t.Fatalf("created refs=%q want=%q", got, wantRefs)
	}
	if got := string(read("cxt-calls")); got != "called\n" {
		t.Fatalf("unexpected calls=%q", got)
	}

	must(repo, "push", "origin", ":refs/heads/topic-two")
	checkRows(map[string][]string{"refs/heads/topic-two": {"(delete)", zero, "refs/heads/topic-two", head}}, true)
	checkArgs()
	wantRefs = "refs/heads/topic-one " + head + "\nrefs/tags/review-tag " + head
	if got := remoteRefs(); got != wantRefs {
		t.Fatalf("deleted refs=%q want=%q", got, wantRefs)
	}
	if got := string(read("cxt-calls")); got != "called\ncalled\n" {
		t.Fatalf("unexpected calls=%q", got)
	}

	beforeRemote, beforeCalls := remoteRefs(), read("cxt-calls")
	must(repo, "commit", "--allow-empty", "-m", "synthetic update rejected")
	next := must(repo, "rev-parse", "HEAD")
	must(repo, "branch", "-f", "topic-one", next)
	write(filepath.Join(root, "user-status"), "19\n", 0600)
	if out, err := run(repo, "push", "origin", "refs/heads/topic-one"); err == nil {
		t.Fatalf("user rejection did not reject Git push: %s", out)
	}
	checkRows(map[string][]string{"refs/heads/topic-one": {"refs/heads/topic-one", next, "refs/heads/topic-one", head}}, false)
	if got := argv("user-args"); !reflect.DeepEqual(got, []string{"origin", remote}) {
		t.Fatalf("reject args=%q", got)
	}
	if got := remoteRefs(); got != beforeRemote {
		t.Fatalf("rejected push changed remote refs: %q vs %q", got, beforeRemote)
	}
	if got := read("cxt-calls"); !bytes.Equal(got, beforeCalls) {
		t.Fatalf("CXT ran after user rejection: %q vs %q", got, beforeCalls)
	}
}
