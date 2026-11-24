package githooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A hook of the user's that ran before cxt was installed still gets exactly
// what Git gives the hook. cxt gets none of it: it is only told to wake.
func TestManagedHooksGiveTheUsersHookItsInputUnchanged(t *testing.T) {
	for _, event := range HookNames {
		for _, input := range []string{"", "first record\nsecond record\n", "record without newline", "record\n\n"} {
			for _, userStatus := range []int{-1, 0, 19} {
				t.Run(fmt.Sprintf("%s/%d/%d", event, len(input), userStatus), func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "hook space's")
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					cxtInput, userInput := filepath.Join(dir, "cxt-input"), filepath.Join(dir, "user-input")
					cxtArgs, userArgs := filepath.Join(dir, "cxt-args"), filepath.Join(dir, "user-args")
					recorder := func(inputPath, argsPath string, status int) string {
						return "#!/bin/sh\ncat > " + shellQuote(inputPath) + "\nprintf '%s\\000' \"$@\" > " + shellQuote(argsPath) + fmt.Sprintf("\nexit %d\n", status)
					}
					binary := filepath.Join(dir, "cxt")
					writeHookTestFile(t, binary, recorder(cxtInput, cxtArgs, 23))
					hook := filepath.Join(dir, event)
					writeHookTestFile(t, hook, script(binary))
					if userStatus >= 0 {
						writeHookTestFile(t, hook+".pre-cxt", recorder(userInput, userArgs, userStatus))
					}
					args := []string{"origin", "ssh://git@example.test/quote' space/$literal.git"}
					cmd := exec.Command(hook, args...)
					cmd.Env = cleanHookTestEnv()
					cmd.Stdin = strings.NewReader(input)
					out, err := cmd.CombinedOutput()
					status := 0
					if err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) {
							t.Fatalf("run: %v %s", err, out)
						}
						status = exit.ExitCode()
					}
					wantStatus := userStatus
					if wantStatus < 0 {
						wantStatus = 0
					}
					if status != wantStatus {
						t.Fatalf("status=%d want=%d: %s", status, wantStatus, out)
					}
					if len(out) != 0 {
						t.Fatalf("the hook printed something of its own: %q", out)
					}
					read := func(path string) string {
						t.Helper()
						got, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						return string(got)
					}
					if userStatus >= 0 {
						if got := read(userInput); got != input {
							t.Fatalf("the user's hook input changed: got=%q want=%q", got, input)
						}
						if got := strings.Split(strings.TrimSuffix(read(userArgs), "\x00"), "\x00"); !reflect.DeepEqual(got, args) {
							t.Fatalf("the user's hook arguments changed: %q", got)
						}
					}
					if got := read(cxtInput); got != "" {
						t.Fatalf("cxt was given the hook's input: %q", got)
					}
					// cxt is told which hook woke it, by the hook's own file name,
					// and which process runs the hook: here this test. It is told
					// nothing of what Git passed to the hook.
					if got := strings.Split(strings.TrimSuffix(read(cxtArgs), "\x00"), "\x00"); !reflect.DeepEqual(got, []string{"git-hook", "wake", event, strconv.Itoa(os.Getpid())}) {
						t.Fatalf("cxt was called with %q", got)
					}
				})
			}
		}
	}
}
