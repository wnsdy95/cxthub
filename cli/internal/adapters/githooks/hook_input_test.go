package githooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManagedHooksReplayExactInput(t *testing.T) {
	for _, event := range []string{"pre-push", "post-rewrite"} {
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
					writeHookTestFile(t, hook, script(event, binary))
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
					check := func(inputPath, argsPath string, wantArgs []string) {
						t.Helper()
						got, err := os.ReadFile(inputPath)
						if err != nil || string(got) != input {
							t.Fatalf("input changed: got=%q want=%q err=%v", got, input, err)
						}
						got, err = os.ReadFile(argsPath)
						if err != nil || !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(got), "\x00"), "\x00"), wantArgs) {
							t.Fatalf("arguments changed: %q %v", got, err)
						}
					}
					if userStatus >= 0 {
						check(userInput, userArgs, args)
					}
					if event == "pre-push" && userStatus > 0 {
						if _, err := os.Stat(cxtInput); !os.IsNotExist(err) {
							t.Fatal("cxt ran after user rejected push")
						}
					} else {
						check(cxtInput, cxtArgs, append([]string{"git-hook", event}, args...))
					}
				})
			}
		}
	}
}
