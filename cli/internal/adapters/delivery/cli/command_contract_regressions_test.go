package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func captureContractStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	previous := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = previous }()
	runErr := run()
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), runErr
}

func TestForcedPullRejectsBeforeQueuedWork(t *testing.T) {
	cwd, c, store, repoID, _ := historyFixture(t)
	t.Chdir(cwd)
	ctx := context.Background()
	oid := gitOut(cwd, "rev-parse", "HEAD")
	if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/queued"); err != nil {
		t.Fatal(err)
	}
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := j.List()
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations: %+v %v", ops, err)
	}
	runLifecycleGit(t, cwd, "branch", "queued")
	commitBirthJournal(t, cwd, ops[0].Event.ID)
	before, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	c.ResolveSyncDestination = func(context.Context, string, string) (SyncDestination, error) {
		t.Fatal("forced pull reached sync resolution")
		return SyncDestination{}, nil
	}
	for _, args := range [][]string{{"cxt", "pull", "--force"}, {"cxt", "pull", "-f"}, {"cxt", "pull", "origin", "main", "--force"}} {
		if handled, err := PreflightArgs(args); handled || err == nil {
			t.Fatalf("forced pull accepted: %v", args)
		}
		for _, container := range []*Container{nil, c} {
			err := Run(container, args)
			if err == nil {
				t.Fatal("forced pull succeeded")
			}
			failure := ClassifyCommandFailure(err)
			if failure.Code != "invalid_arguments" || failure.ExitCode != 2 || failure.State != "unchanged" {
				t.Fatalf("not rejected in preflight: %+v", failure)
			}
		}
	}
	after, err := j.List()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("forced pull replayed queue: before=%+v after=%+v err=%v", before, after, err)
	}
	if events, err := store.ListHistoryEvents(ctx, repoID); err != nil || len(events) != 0 {
		t.Fatalf("forced pull published history: %+v %v", events, err)
	}
	if _, err := store.GetRef(ctx, repoID, domain.RefBranch, "queued"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("forced pull published a branch: %v", err)
	}
}

func TestRepairFromServerUsesRegistryOptions(t *testing.T) {
	remote := "https://example.test/team/repository"
	for _, args := range [][]string{{"--from-server", "--remote", remote}, {"--remote=" + remote, "--from-server"}} {
		options, err := ParseRepairFromServerArgs(args)
		if err != nil || options.RemoteURL != remote {
			t.Fatalf("%v: %+v %v", args, options, err)
		}
	}
	for _, args := range [][]string{{"--from-server", "--remote=" + remote, "--remote", remote}, {"--from-server", "--remote="}, {"--from-server", "--", "--remote=" + remote}, {"--preview", "--ref", "main", "--reason", "reviewed", "--output", "plan.json"}} {
		if _, err := ParseRepairFromServerArgs(args); err == nil || ClassifyCommandFailure(err).Code != "invalid_arguments" {
			t.Fatalf("invalid server repair accepted: %v %v", args, err)
		}
	}
}

type contractSettingsSync struct{ inbound.SyncRepo }

func (contractSettingsSync) Connect(context.Context, inbound.SyncInput) (inbound.ConnectOutput, error) {
	return inbound.ConnectOutput{Repo: domain.Repo{ID: domain.HashContent([]byte("settings fixture"))}}, nil
}

type contractSettings struct {
	errs    map[string]error
	bundles map[string]domain.SettingsBundle
	calls   []string
}

func (s *contractSettings) PullSettings(_ context.Context, _, kind string) (domain.SettingsBundle, error) {
	s.calls = append(s.calls, kind)
	if err := s.errs[kind]; err != nil {
		return domain.SettingsBundle{}, err
	}
	if bundle, ok := s.bundles[kind]; ok {
		return bundle, nil
	}
	return domain.SettingsBundle{}, fmt.Errorf("optional bundle: %w", domain.ErrNotFound)
}

func TestSettingsPullPropagatesFailures(t *testing.T) {
	for _, cause := range []error{
		&backendclient.HTTPError{Status: 401}, &backendclient.HTTPError{Status: 403},
		&backendclient.HTTPError{Status: 503}, &backendclient.HTTPError{Status: 404},
		&url.Error{Op: "Get", URL: "https://example.test/settings", Err: errors.New("connection lost")},
		domain.ErrHashMismatch, os.ErrPermission, errors.New("404 not found"),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("CXT_REMOTE", "https://example.test/api/v1")
			settings := &contractSettings{errs: map[string]error{"claude": cause}}
			out, err := captureContractStdout(t, func() error {
				return Run(&Container{Sync: contractSettingsSync{}, Settings: settings}, []string{"cxt", "settings", "pull"})
			})
			if !errors.Is(err, cause) || !reflect.DeepEqual(settings.calls, []string{"claude"}) {
				t.Fatalf("failure swallowed or processing continued: %v calls=%v", err, settings.calls)
			}
			if strings.Contains(out, "no team settings") || strings.Contains(out, "applied") {
				t.Fatalf("false success output: %q", out)
			}
			if ClassifyCommandFailure(err).Code != ClassifyCommandFailure(cause).Code {
				t.Fatalf("typed classification changed: %v", err)
			}
		})
	}
}

func TestSettingsPullSkipsOnlyAbsentBundlesAndReportsPartialProgress(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CXT_REMOTE", "https://example.test/api/v1")
	settings := &contractSettings{}
	c := &Container{Sync: contractSettingsSync{}, Settings: settings}
	out, err := captureContractStdout(t, func() error { return Run(c, []string{"cxt", "settings", "pull"}) })
	if err != nil || !strings.Contains(out, "no team settings") || len(settings.calls) != 3 {
		t.Fatalf("typed absence: %q %v %v", out, err, settings.calls)
	}
	settings.calls = nil
	settings.bundles = map[string]domain.SettingsBundle{"agents": {Kind: "agents", Files: []domain.SettingsFile{{Path: "config.txt", ContentB64: base64.StdEncoding.EncodeToString([]byte("applied"))}}}}
	cause := &backendclient.HTTPError{Status: 403}
	settings.errs = map[string]error{"codex": cause}
	out, err = captureContractStdout(t, func() error { return Run(c, []string{"cxt", "settings", "pull"}) })
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "after applying 1 file(s)") || !strings.Contains(out, "applied .agents/") || strings.Contains(out, "no team settings") {
		t.Fatalf("partial failure: %q %v", out, err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, ".agents", "config.txt")); err != nil || string(raw) != "applied" {
		t.Fatalf("earlier applied bundle lost: %q %v", raw, err)
	}
}

func TestSettingsPullReturnsLocalWriteFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CXT_REMOTE", "https://example.test/api/v1")
	settings := &contractSettings{bundles: map[string]domain.SettingsBundle{"claude": {Kind: "claude", Files: []domain.SettingsFile{{Path: strings.Repeat("x", 300), ContentB64: "eA=="}}}}}
	out, err := captureContractStdout(t, func() error {
		return Run(&Container{Sync: contractSettingsSync{}, Settings: settings}, []string{"cxt", "settings", "pull"})
	})
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || strings.Contains(out, "no team settings") || len(settings.calls) != 1 {
		t.Fatalf("write failure swallowed: output=%q err=%v calls=%v", out, err, settings.calls)
	}
}

func TestRemoteRepairRepeatedApplyReportsRecordedReceipt(t *testing.T) {
	plan := outbound.RemoteRepairPlan{Version: 1, RepoID: "fixture", Reason: "reviewed"}
	plan.ID = outbound.RemoteRepairPlanID(plan)
	path := filepath.Join(t.TempDir(), "repair.json")
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	appliedAt := time.Date(2025, 1, 2, 3, 4, 5, 123, time.UTC)
	calls := 0
	c := &Container{ApplyRemoteRepair: func(_ context.Context, _ string, approved domain.ContentHash, got outbound.RemoteRepairPlan) (outbound.RemoteRepairReceipt, error) {
		calls++
		if approved != plan.ID || !reflect.DeepEqual(got, plan) {
			t.Fatal("changed approved repair")
		}
		return outbound.RemoteRepairReceipt{Plan: plan, AppliedAt: appliedAt}, nil
	}}
	var previous string
	for attempt := 0; attempt < 2; attempt++ {
		out, err := captureContractStdout(t, func() error { return Run(c, []string{"cxt", "repair", "--apply", path, "--expect", string(plan.ID)}) })
		if err != nil || !strings.Contains(out, "repair receipt "+string(plan.ID)+": state=applied applied_at="+appliedAt.Format(time.RFC3339Nano)) || !strings.Contains(out, "without reapplying or checking current remote/local state") || strings.Contains(out, "applied exact repair") {
			t.Fatalf("receipt output: %q %v", out, err)
		}
		if attempt > 0 && out != previous {
			t.Fatalf("retry claimed a new application: %q -> %q", previous, out)
		}
		previous = out
	}
	if calls != 2 {
		t.Fatalf("apply calls: %d", calls)
	}
}
