package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func calibrationScope(name string) domain.ContentHash { return domain.HashContent([]byte(name)) }

func calibrationPath(root string, scope domain.ContentHash) string {
	return filepath.Join(root, ".cxt", "agent-input-calibrations", strings.TrimPrefix(scope, "sha256:")+".json")
}

func TestAgentInputCalibrationMissing(t *testing.T) {
	root := t.TempDir()
	scope := calibrationScope("missing")
	got, err := NewFileStore(root).ReadAgentInputCalibration(context.Background(), scope)
	if err != nil || got != (domain.AgentInputCalibration{Scope: scope}) {
		t.Fatalf("missing: %+v, %v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read created state: %v, %v", entries, err)
	}
}

func TestAgentInputCalibrationReorderedDuplicateAndPrivate(t *testing.T) {
	scope := calibrationScope("ordered")
	observations := []domain.AgentInputCalibration{
		{Scope: scope, OverheadTokens: 90},
		{Scope: scope, OverheadTokens: 10, InputCeilingTokens: 300},
		{Scope: scope, OverheadTokens: 20, InputCeilingTokens: 200},
	}
	want := domain.AgentInputCalibration{Scope: scope, OverheadTokens: 90, InputCeilingTokens: 200}
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			root := t.TempDir()
			store := NewFileStore(root)
			ctx := context.Background()
			for _, i := range order {
				if _, err := store.MergeAgentInputCalibration(ctx, observations[i]); err != nil {
					t.Fatal(err)
				}
			}
			path := calibrationPath(root, scope)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// A replacement also restores private permissions on an older record.
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			for _, i := range order {
				got, err := store.MergeAgentInputCalibration(ctx, observations[i])
				if err != nil || got != want {
					t.Fatalf("duplicate merge: %+v, %v", got, err)
				}
			}
			got, err := NewFileStore(root).ReadAgentInputCalibration(ctx, scope)
			if err != nil || got != want {
				t.Fatalf("persisted merge: %+v, %v", got, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("duplicate changed record: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("record is not 0600: %v, %v", info, err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
				t.Fatalf("unexpected persisted names: %v, %v", entries, err)
			}
		})
	}
}

func TestAgentInputCalibrationScopeIsolation(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	store := NewFileStore(root)
	first := domain.AgentInputCalibration{Scope: calibrationScope("runtime-a"), OverheadTokens: 800, InputCeilingTokens: 100}
	second := domain.AgentInputCalibration{Scope: calibrationScope("runtime-b"), OverheadTokens: 10, InputCeilingTokens: 900}
	for _, observation := range []domain.AgentInputCalibration{first, second} {
		if _, err := store.MergeAgentInputCalibration(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []domain.AgentInputCalibration{first, second, {Scope: calibrationScope("runtime-c")}} {
		got, err := store.ReadAgentInputCalibration(ctx, want.Scope)
		if err != nil || got != want {
			t.Fatalf("scope leak: %+v, %v; want %+v", got, err, want)
		}
	}
	otherRepo := NewFileStore(t.TempDir())
	got, err := otherRepo.ReadAgentInputCalibration(ctx, first.Scope)
	if err != nil || got != (domain.AgentInputCalibration{Scope: first.Scope}) {
		t.Fatalf("repository leak: %+v, %v", got, err)
	}
	// A valid, checksummed record copied under another scope must still fail.
	raw, err := os.ReadFile(calibrationPath(root, first.Scope))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(calibrationPath(root, second.Scope), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadAgentInputCalibration(ctx, second.Scope); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("copied record accepted: %v", err)
	}
}

func TestAgentInputCalibrationCorrupt(t *testing.T) {
	ctx := context.Background()
	scope := calibrationScope("corrupt")
	observation := domain.AgentInputCalibration{Scope: scope, OverheadTokens: 123, InputCeilingTokens: 456}
	seedRoot := t.TempDir()
	if _, err := NewFileStore(seedRoot).MergeAgentInputCalibration(ctx, observation); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(calibrationPath(seedRoot, scope))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty": nil, "truncated": valid[:len(valid)-1], "null": []byte("null"), "array": []byte("[]"),
		"trailing object":   append(append([]byte{}, valid...), []byte("{}")...),
		"oversized":         append(bytes.Repeat([]byte(" "), maxAgentInputCalibrationBytes), valid...),
		"duplicate":         bytes.Replace(valid, []byte(`"overhead_tokens":123`), []byte(`"overhead_tokens":1,"overhead_tokens":123`), 1),
		"duplicate escaped": bytes.Replace(valid, []byte(`"overhead_tokens":123`), []byte(`"overhead_tokens":1,"overhead_\u0074okens":123`), 1),
	}
	for _, field := range []string{"version", "scope", "overhead_tokens", "input_ceiling_tokens", "checksum"} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(valid, &fields); err != nil {
			t.Fatal(err)
		}
		original := fields[field]
		delete(fields, field)
		cases["missing "+field], _ = json.Marshal(fields)
		fields[field] = json.RawMessage("null")
		cases["null "+field], _ = json.Marshal(fields)
		delete(fields, field)
		fields[strings.ToUpper(field)] = original
		cases["case alias "+field], _ = json.Marshal(fields)
	}
	for name, replacement := range map[string]string{
		"negative": `"overhead_tokens":-1`, "string": `"overhead_tokens":"123"`,
		"fraction": `"overhead_tokens":1.5`, "overflow": `"overhead_tokens":999999999999999999999999999`,
		"checksum damage": `"overhead_tokens":124`, "unknown": `"overhead_tokens":123,"extra":0`,
	} {
		cases[name] = bytes.Replace(valid, []byte(`"overhead_tokens":123`), []byte(replacement), 1)
	}
	cases["version"] = bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":2`), 1)
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := calibrationPath(root, scope)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store := NewFileStore(root)
			if got, err := store.ReadAgentInputCalibration(ctx, scope); !errors.Is(err, domain.ErrHashMismatch) || got != (domain.AgentInputCalibration{}) {
				t.Fatalf("corrupt read: %+v, %v", got, err)
			}
			if got, err := store.MergeAgentInputCalibration(ctx, observation); !errors.Is(err, domain.ErrHashMismatch) || got != (domain.AgentInputCalibration{}) {
				t.Fatalf("corrupt merge: %+v, %v", got, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("corrupt record was replaced: %v", err)
			}
		})
	}
}

func TestAgentInputCalibrationInvalidAndCanceled(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(root)
	for _, scope := range []string{"", "../escape", "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 63)} {
		if _, err := store.ReadAgentInputCalibration(context.Background(), scope); err == nil {
			t.Fatal("invalid scope read succeeded")
		}
		if _, err := store.MergeAgentInputCalibration(context.Background(), domain.AgentInputCalibration{Scope: scope}); err == nil {
			t.Fatal("invalid scope merge succeeded")
		}
	}
	scope := calibrationScope("valid")
	for _, observation := range []domain.AgentInputCalibration{{Scope: scope, OverheadTokens: -1}, {Scope: scope, InputCeilingTokens: -1}} {
		if _, err := store.MergeAgentInputCalibration(context.Background(), observation); err == nil {
			t.Fatal("negative observation accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadAgentInputCalibration(ctx, scope); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	if _, err := store.MergeAgentInputCalibration(ctx, domain.AgentInputCalibration{Scope: scope}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled merge: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid request created state: %v, %v", entries, err)
	}
}

func TestAgentInputCalibrationRejectsSymlinkAndSpecialPaths(t *testing.T) {
	scope := calibrationScope("symlink")
	for _, component := range []string{"store", "directory", "file", "dangling", "special", "lock"} {
		t.Run(component, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			path := calibrationPath(root, scope)
			target := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			link := path
			switch component {
			case "store":
				link, target = filepath.Join(root, ".cxt"), outside
			case "directory":
				link, target = filepath.Dir(path), outside
			case "dangling":
				target = filepath.Join(outside, "absent")
			case "lock":
				link, target = filepath.Join(root, ".cxt", "locks"), outside
			}
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if component == "special" {
				if err := os.Mkdir(link, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			store := NewFileStore(root)
			if component != "lock" {
				if _, err := store.ReadAgentInputCalibration(context.Background(), scope); err == nil {
					t.Fatal("unsafe read succeeded")
				}
			}
			if _, err := store.MergeAgentInputCalibration(context.Background(), domain.AgentInputCalibration{Scope: scope, OverheadTokens: 1}); err == nil {
				t.Fatal("unsafe merge succeeded")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
				t.Fatalf("escaped into outside directory: %v, %v", entries, err)
			}
			raw, err := os.ReadFile(filepath.Join(outside, "sentinel"))
			if err != nil || string(raw) != "untouched" {
				t.Fatalf("outside file changed: %q, %v", raw, err)
			}
		})
	}
}

func TestAgentInputCalibrationConcurrentReadersAndWriters(t *testing.T) {
	root := t.TempDir()
	scope := calibrationScope("concurrent")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const writers = 24
	var wg sync.WaitGroup
	start, done := make(chan struct{}), make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			got, err := NewFileStore(root).ReadAgentInputCalibration(ctx, scope)
			if err != nil || got.Validate(scope) != nil {
				t.Errorf("concurrent read observed invalid state: %+v, %v", got, err)
				return
			}
		}
	}()
	for i := 1; i <= writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			observation := domain.AgentInputCalibration{Scope: scope, OverheadTokens: i, InputCeilingTokens: 1000 + i}
			got, err := NewFileStore(root).MergeAgentInputCalibration(ctx, observation)
			if err != nil || got.OverheadTokens < i || got.InputCeilingTokens > 1000+i || got.InputCeilingTokens == 0 {
				t.Errorf("merge lost its own bound: %+v, %v", got, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(done)
	<-readerDone
	got, err := NewFileStore(root).ReadAgentInputCalibration(ctx, scope)
	want := domain.AgentInputCalibration{Scope: scope, OverheadTokens: writers, InputCeilingTokens: 1001}
	if err != nil || got != want {
		t.Fatalf("lost concurrent bound: %+v, %v; want %+v", got, err, want)
	}
}

func TestAgentInputCalibrationCrossProcess(t *testing.T) {
	const childEnv = "CXT_CALIBRATION_TEST_CHILD"
	scope := calibrationScope("cross-process")
	if root := os.Getenv(childEnv); root != "" {
		i, err := strconv.Atoi(os.Getenv("CXT_CALIBRATION_TEST_INDEX"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "ready-"+strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for j := 0; j < 3; j++ {
			observation := domain.AgentInputCalibration{Scope: scope, OverheadTokens: 100 + i, InputCeilingTokens: 500 + i}
			if _, err := NewFileStore(root).MergeAgentInputCalibration(ctx, observation); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const children = 5
	results := make(chan error, children)
	err := NewFileStore(root).withMutationLock(ctx, "agent-input-calibrations", strings.TrimPrefix(scope, "sha256:"), func() error {
		for i := 0; i < children; i++ {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentInputCalibrationCrossProcess$")
			cmd.Env = append(os.Environ(), childEnv+"="+root, "CXT_CALIBRATION_TEST_INDEX="+strconv.Itoa(i))
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				return err
			}
			go func() {
				err := cmd.Wait()
				if err != nil {
					err = fmt.Errorf("child: %w: %s", err, output.String())
				}
				results <- err
			}()
		}
		for i := 0; i < children; i++ {
			for {
				if _, err := os.Stat(filepath.Join(root, "ready-"+strconv.Itoa(i))); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		select {
		case err := <-results:
			return fmt.Errorf("child did not wait for existing mutation lock: %v", err)
		case <-time.After(80 * time.Millisecond):
		}
		// A blocked merge must respect cancellation rather than resetting state.
		blocked, stop := context.WithTimeout(ctx, 30*time.Millisecond)
		defer stop()
		if _, err := NewFileStore(root).MergeAgentInputCalibration(blocked, domain.AgentInputCalibration{Scope: scope}); !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("lock cancellation: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < children; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	got, err := NewFileStore(root).ReadAgentInputCalibration(ctx, scope)
	want := domain.AgentInputCalibration{Scope: scope, OverheadTokens: 104, InputCeilingTokens: 500}
	if err != nil || got != want {
		t.Fatalf("cross-process merge: %+v, %v; want %+v", got, err, want)
	}
}
