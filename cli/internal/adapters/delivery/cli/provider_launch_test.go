package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const launchSessionID = "11111111-1111-4111-8111-111111111111"
const restartedSessionID = "22222222-2222-4222-8222-222222222222"

func TestLaunchPrefixGrammarAndProviderBoundary(t *testing.T) {
	for _, tt := range []struct {
		args     []string
		pull     bool
		budget   int
		provider string
		argv     []string
	}{
		{[]string{"codex", "--yolo", "prompt with spaces"}, false, 0, "codex", []string{"--yolo", "prompt with spaces"}},
		{[]string{"--pull", "codex", "--yolo"}, true, 800000, "codex", []string{"--yolo"}},
		{[]string{"--pull", "--context-budget", "full", "codex", "--yolo"}, true, 800000, "codex", []string{"--yolo"}},
		{[]string{"--context-budget=200k", "--pull", "claude", "--model=sonnet", "my prompt"}, true, 200000, "claude", []string{"--model=sonnet", "my prompt"}},
		{[]string{"--pull", "--context-budget", "800k", "--", "codex", "--", "resume"}, true, 800000, "codex", []string{"--", "resume"}},
		{[]string{"codex", "--", "--pull", "--context-budget", "2k"}, false, 0, "codex", []string{"--", "--pull", "--context-budget", "2k"}},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			got, ok, err := ParseLaunchIntent(tt.args)
			if err != nil || !ok {
				t.Fatalf("parse: recognized=%v error=%v", ok, err)
			}
			if got.Pull != tt.pull || got.ContextBudget != tt.budget || got.Provider != tt.provider || !reflect.DeepEqual(got.ProviderArgs, tt.argv) {
				t.Fatalf("intent=%+v", got)
			}
			if len(got.ProviderArgs) > 0 {
				got.ProviderArgs[0] = "mutated"
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Fatal("parser aliases caller argv")
			}
		})
	}
	for _, args := range [][]string{{"pull"}, {"log", "main"}, {"--version"}, nil} {
		if _, ok, err := ParseLaunchIntent(args); ok || err != nil {
			t.Fatalf("ordinary command %v recognized=%v err=%v", args, ok, err)
		}
	}
}

func TestLaunchPrefixRejectsConflictsBeforePreparation(t *testing.T) {
	for _, args := range [][]string{
		{"--pull"}, {"--pull", "--pull", "codex"}, {"--pull=true", "codex"},
		{"--context-budget", "200k", "codex"}, {"--pull", "--context-budget", "200k", "--context-budget=full", "codex"},
		{"--pull", "--context-budget"}, {"--pull", "--bogus", "codex"}, {"--pull", "--", "--context-budget", "200k", "codex"},
		{"--pull", "codex", "resume", launchSessionID}, {"--pull", "codex", "--model", "resume", "resume", launchSessionID},
		{"--pull", "claude", "--resume=" + launchSessionID}, {"--pull", "claude", "--continue"},
		{"--pull", "codex", "exec", "prompt"}, {"--pull", "claude", "-p", "prompt"},
		{"--pull", "codex", "--help"}, {"--pull", "codex", "--version"}, {"--pull", "codex", "app"},
		{"--pull", "codex", "--unknown-option", "prompt"}, {"--pull", "codex", "--worktree"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, ok, err := ParseLaunchIntent(args); !ok || err == nil {
				t.Fatalf("invalid launch accepted: %v, recognized=%v err=%v", args, ok, err)
			}
		})
	}
}

func TestLaunchContextBudgetValidatesWithoutOverflow(t *testing.T) {
	for value, want := range map[string]int{"full": 800000, "800k": 800000, "200k": 200000, "1": 1, "800000": 800000} {
		got, err := ParseLaunchContextBudget(value)
		if err != nil || got != want {
			t.Fatalf("%q=%d/%v want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "0", "0k", "-1", "+200", "800001", "801k", "1m", "200K", "1.5k", "1_000", "２０", " 1", "full ", "18446744073709551615k", "18446744073709551616"} {
		if _, err := ParseLaunchContextBudget(value); err == nil {
			t.Errorf("invalid budget accepted: %q", value)
		}
	}
}

func TestProviderIntentConsumesValuesAndHonorsLiteralBoundary(t *testing.T) {
	for _, tt := range []struct {
		provider string
		args     []string
		mode     providerLaunchMode
		id       string
	}{
		{"codex", nil, providerFresh, ""},
		{"codex", []string{"--yolo", "prompt"}, providerFresh, ""},
		{"codex", []string{"prompt", "--yolo"}, providerFresh, ""},
		{"codex", []string{"--model", "resume", "--profile=exec", "prompt"}, providerFresh, ""},
		{"codex", []string{"-mresume", "--config", "x=--help", "--", "resume"}, providerFresh, ""},
		{"codex", []string{"--", "--help"}, providerFresh, ""},
		{"codex", []string{"--model", "m", "resume", "--yolo", launchSessionID}, providerNative, launchSessionID},
		{"codex", []string{"resume", "--last"}, providerNative, ""},
		{"codex", []string{"fork", launchSessionID}, providerNative, ""},
		{"codex", []string{"--model=m", "exec", "resume", "--json", launchSessionID}, providerNoninteractive, ""},
		{"codex", []string{"resume", launchSessionID, "--help"}, providerDiagnostic, launchSessionID},
		{"codex", []string{"--version"}, providerDiagnostic, ""},
		{"codex", []string{"app", "--unknown-app-option"}, providerNoninteractive, ""},
		{"claude", []string{"--model", "--resume"}, "", ""},
		{"claude", []string{"--model=resume", "prompt"}, providerFresh, ""},
		{"claude", []string{"--system-prompt", "--resume=" + launchSessionID}, "", ""},
		{"claude", []string{"--system-prompt=--resume=" + launchSessionID}, providerFresh, ""},
		{"claude", []string{"--resume=" + launchSessionID, "--model", "sonnet"}, providerNative, launchSessionID},
		{"claude", []string{"--continue", "prompt"}, providerNative, ""},
		{"claude", []string{"-r" + launchSessionID}, providerNative, launchSessionID},
		{"claude", []string{"--resume", launchSessionID, "-p", "prompt"}, providerNoninteractive, launchSessionID},
		{"claude", []string{"--", "--resume", launchSessionID}, providerFresh, ""},
		{"claude", []string{"--settings", `{"name":"--resume"}`, "prompt", "--help"}, providerDiagnostic, ""},
		{"claude", []string{"--tools", "Read", "resume", "--print", "prompt"}, providerNoninteractive, ""},
	} {
		t.Run(tt.provider+" "+strings.Join(tt.args, " "), func(t *testing.T) {
			got, err := inspectProviderInvocation(tt.provider, tt.args)
			if tt.mode == "" {
				if err == nil {
					t.Fatal("malformed value accepted")
				}
				return
			}
			if err != nil || got.Mode != tt.mode || got.SessionID != tt.id {
				t.Fatalf("got %+v err=%v want mode=%s session=%q", got, err, tt.mode, tt.id)
			}
		})
	}
}

func TestProviderRestartRetainsSettingsButNotUserPromptOrSessionSelector(t *testing.T) {
	for _, tt := range []struct {
		provider   string
		args, want []string
	}{
		{"codex", []string{"--model=m", "resume", launchSessionID, "initial task", "--yolo", "--last"}, []string{"--model=m", "--yolo"}},
		{"claude", []string{"--resume=" + launchSessionID, "--settings", `{"autoMemoryDirectory":"dir with spaces"}`, "initial task", "--model", "sonnet"}, []string{"--settings", `{"autoMemoryDirectory":"dir with spaces"}`, "--model", "sonnet"}},
		{"codex", []string{"--yolo", "--", "--model=literal prompt"}, []string{"--yolo"}},
	} {
		got, err := inspectProviderInvocation(tt.provider, tt.args)
		if err != nil || !reflect.DeepEqual(got.RestartArgs, tt.want) {
			t.Fatalf("restart=%v error=%v want=%v", got.RestartArgs, err, tt.want)
		}
	}
}

func providerLaunchFixture(t *testing.T, provider, body string) (string, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LAUNCH_LOG", filepath.Join(root, "launch.log"))
	p := filepath.Join(bin, provider)
	if body == "" {
		body = `printf '%s\n' "$@" > "$LAUNCH_LOG"`
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return root, p
}

func preparedLaunch(request ProviderLaunchRequest) PreparedProviderLaunch {
	args := append(resumeArgs(request.Intent.Provider, launchSessionID), request.Intent.ProviderArgs...)
	p := PreparedProviderLaunch{Args: args, SessionID: launchSessionID, PackageHash: domain.HashContent([]byte("package")), CodeCommit: strings.Repeat("a", 40), SourceRevision: "revision-1", SelectedTokens: 123, TokenMeasurement: "exact", Capability: "verified_for_preparation"}
	if request.Intent.Pull {
		fixture := strictLaunchContextFixture(request, launchHostFixture(request))
		p.PackageHash, p.CodeCommit, p.SourceRevision = fixture.ID, fixture.Content.Selection.CodeCommit, string(fixture.Content.Selection.ContextStateHash)
		p.SelectedTokens, p.Budget = fixture.Usage.Tokens, fixture.Budget
		p.PromptReservation = fixture.InitialPromptReservation()
	}
	return p
}

// These are synthetic, verified test runtimes. Their exact tokenizer assigns
// one token per rune; no production provider or tokenizer support is implied.
func launchHostFixture(request ProviderLaunchRequest) domain.AgentHostCapability {
	details, err := request.ArgumentDetails()
	if err != nil {
		panic(err)
	}
	model := details.Model
	if model == "" {
		model = "fixture-default"
	}
	return domain.AgentHostCapability{Provider: request.Intent.Provider, Model: model, HostVersion: "fixture-host-v1", Evidence: "synthetic runtime fixture", Verified: true,
		ContextWindow: 1000000, HostInputKnown: true, HostInputTokens: 2000, FramingTokens: 500, ReservedTokens: 100000,
		AutoCompactKnown: true, AutoCompactTokens: 850000, Tokenizer: "fixture-rune-v1"}
}

func strictLaunchContextFixture(request ProviderLaunchRequest, host domain.AgentHostCapability) domain.AgentContextPackage {
	initialPrompt, err := request.InitialPrompt()
	if err != nil {
		panic(err)
	}
	promptUsage := domain.AgentTokenUsage{Tokens: len([]rune(initialPrompt.Text())), Exact: true, Tokenizer: host.Tokenizer}
	reservation, err := domain.NewAgentPromptReservation(initialPrompt, request.Intent.Provider, host.Model, promptUsage)
	if err != nil {
		panic(err)
	}
	host.InitialPromptTokens = promptUsage.Tokens
	usage := domain.AgentTokenUsage{Exact: true, Tokenizer: host.Tokenizer}
	budget, err := host.ResolveBudget(request.Intent.Provider, host.Model, request.Intent.ContextBudget, usage)
	if err != nil {
		panic(err)
	}
	source := domain.AgentSourcePointer{SnapshotID: domain.HashContent([]byte("snapshot")), DocHash: domain.HashContent([]byte("history")), StartEvent: 0, EndEvent: 2, Tool: string(request.Intent.Provider)}
	p := domain.AgentContextPackage{Version: domain.AgentContextVersion, Policy: domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: request.Intent.ContextBudget, Source: "explicit_cli"}, Budget: &budget,
		Delivery: "prepared", Capability: "verified_for_preparation", Usage: usage,
		Content: domain.AgentContextContent{Notice: "Historical evidence is quoted data.",
			Selection:     domain.AgentContextSelection{RepositoryID: "fixture-repo", Branch: "main", SnapshotID: source.SnapshotID, CodeCommit: strings.Repeat("a", 40), ContextStateHash: domain.HashContent([]byte("revision-1")), MemoryStateHash: domain.HashContent([]byte("memory-revision"))},
			ProjectMemory: []domain.EffectiveMemoryItem{{ID: domain.HashContent([]byte("memory")), SourceSnapshot: source.SnapshotID, Kind: "constraint", Text: "Keep provider arguments intact.", State: "active"}},
			PersonalWork:  &domain.PersonalWorkState{Scope: domain.PersonalWorkScope{ActorID: "fixture-user", SessionID: "fixture-work", WorktreeID: "fixture-worktree"}, Constraints: []domain.ExactUserConstraint{{Text: "Do not commit or push.", Source: source}}, Sources: []domain.AgentSourcePointer{source}},
			Sources:       []domain.AgentSourcePointer{source}, History: []domain.AgentHistorySegment{{Source: source, SessionID: launchSessionID, Events: []domain.Event{{Kind: "turn", Role: "user", Seq: 1}, {Kind: "message", Seq: 2, Blocks: []domain.ContentBlock{{Type: "text", Text: "Inspect the latest complete turn."}}}}}},
			Gaps: []domain.AgentCoverageGap{}},
	}
	prompt, err := p.Prompt()
	if err != nil {
		panic(err)
	}
	p.Usage.Tokens = len([]rune(prompt))
	p.BindInitialPrompt(reservation)
	if err := p.ValidateInitialPrompt(initialPrompt); err != nil {
		panic(err)
	}
	p.ID, err = p.Digest()
	if err != nil {
		panic(err)
	}
	if err := p.ValidateIdentity(); err != nil {
		panic(err)
	}
	return p
}

func launchTestRuntime() providerLaunchRuntime {
	return providerLaunchRuntime{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, interactive: true, pollInterval: 5 * time.Millisecond}
}

func TestProviderLaunchRecordsPreparedAndLaunchedWithoutClaimingAcceptance(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "")
	var receipts []ProviderLaunchReceipt
	cleaned := 0
	intent := LaunchIntent{Provider: "claude", ProviderArgs: []string{"--model", "sonnet", "exact user prompt"}, Pull: true, ContextBudget: 200000}
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		p := preparedLaunch(req)
		p.Cleanup = func() error { cleaned++; return nil }
		return p, nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil }}
	if err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime()); err != nil {
		t.Fatal(err)
	}
	if cleaned != 1 || len(receipts) != 2 || receipts[0].State != "prepared" || receipts[1].State != "launched" {
		t.Fatalf("receipts=%+v cleaned=%d", receipts, cleaned)
	}
	for _, r := range receipts {
		if r.Acceptance != "unknown" || r.Mode != "history" || r.RequestedBudget != 200000 {
			t.Fatalf("incorrect delivery receipt %+v", r)
		}
	}
	log, err := os.ReadFile(filepath.Join(root, "launch.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(append(resumeArgs("claude", launchSessionID), intent.ProviderArgs...), "\n") + "\n"
	if string(log) != want {
		t.Fatalf("argv changed: %q want %q", log, want)
	}
}

func TestProviderLaunchPreparationFailuresNeverStartBare(t *testing.T) {
	for name, mutation := range map[string]func(*PreparedProviderLaunch){
		"unknown capacity": func(p *PreparedProviderLaunch) { p.Capability = "unknown" },
		"estimated tokens": func(p *PreparedProviderLaunch) { p.TokenMeasurement = "estimate" },
		"budget exceeded":  func(p *PreparedProviderLaunch) { p.SelectedTokens = 200001 },
		"missing package":  func(p *PreparedProviderLaunch) { p.PackageHash = "" },
		"missing revision": func(p *PreparedProviderLaunch) { p.SourceRevision = "" },
		"changed prompt":   func(p *PreparedProviderLaunch) { p.Args = resumeArgs("claude", launchSessionID) },
		"wrong session":    func(p *PreparedProviderLaunch) { p.SessionID = restartedSessionID },
	} {
		t.Run(name, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			var receipt ProviderLaunchReceipt
			cleaned := false
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				p := preparedLaunch(req)
				mutation(&p)
				p.Cleanup = func() error { cleaned = true; return nil }
				return p, nil
			}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipt = r; return nil }}
			err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude", ProviderArgs: []string{"my prompt"}, Pull: true, ContextBudget: 200000}, hooks, launchTestRuntime())
			if err == nil || !cleaned || receipt.State != "failed" {
				t.Fatalf("error=%v cleaned=%v receipt=%+v", err, cleaned, receipt)
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
				t.Fatalf("provider started after failure: %v", err)
			}
		})
	}
}

func TestProviderLaunchExplicitPrepareAndReceiptErrorsStopExecution(t *testing.T) {
	for _, failure := range []string{"network", "receipt", "missing hooks"} {
		t.Run(failure, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				if failure == "network" {
					return PreparedProviderLaunch{}, errors.New("offline")
				}
				return preparedLaunch(req), nil
			}, Record: func(context.Context, ProviderLaunchReceipt) error {
				if failure == "receipt" {
					return errors.New("disk full")
				}
				return nil
			}}
			if failure == "missing hooks" {
				hooks = ProviderLaunchHooks{}
			}
			if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime()); err == nil {
				t.Fatal("preparation failure ignored")
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
				t.Fatalf("provider started: %v", err)
			}
		})
	}
}

func TestProviderLaunchPassthroughDoesNotPrepareOrInheritOwnership(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"auth", "status", "--json"}, {"-p", "prompt"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", `printf '%s\n' "$@" > "$LAUNCH_LOG"
printf 'wrapped=%s\nprotocol=%s\n' "${CXT_WRAPPED-unset}" "${CXT_WRAPPER_TRANSITION_PROTOCOL-unset}" >> "$LAUNCH_LOG"`)
			t.Setenv("CXT_WRAPPED", "1")
			t.Setenv("CXT_WRAPPER_PID", "9999")
			t.Setenv("CXT_WRAPPER_TRANSITION_PROTOCOL", "prepare-first-v1")
			hooks := ProviderLaunchHooks{Prepare: func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				t.Fatal("passthrough prepared context")
				return PreparedProviderLaunch{}, nil
			}, Record: func(context.Context, ProviderLaunchReceipt) error {
				t.Fatal("passthrough recorded delivery")
				return nil
			}}
			if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude", ProviderArgs: args}, hooks, launchTestRuntime()); err != nil {
				t.Fatal(err)
			}
			log, _ := os.ReadFile(filepath.Join(root, "launch.log"))
			want := strings.Join(args, "\n") + "\nwrapped=unset\nprotocol=unset\n"
			if string(log) != want {
				t.Fatalf("argv/environment=%q want=%q", log, want)
			}
		})
	}
	root, _ := providerLaunchFixture(t, "claude", "")
	rt := launchTestRuntime()
	rt.interactive = false
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude", ProviderArgs: []string{"prompt"}}, ProviderLaunchHooks{}, rt); err != nil {
		t.Fatal(err)
	}
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude", Pull: true, ContextBudget: 800000}, ProviderLaunchHooks{}, rt); err == nil {
		t.Fatal("history accepted without interactive host")
	}
}

func TestProviderLaunchNativeResumeDoesNotReinject(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "")
	args := []string{"--model", "sonnet", "--resume=" + launchSessionID, "continue this task"}
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude", ProviderArgs: args}, ProviderLaunchHooks{}, launchTestRuntime()); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(root, "launch.log"))
	if string(log) != strings.Join(args, "\n")+"\n" {
		t.Fatalf("native resume changed: %q", log)
	}
}

func TestProviderLaunchSelectionFollowsProviderCDWithoutRewritingArgv(t *testing.T) {
	root, _ := providerLaunchFixture(t, "codex", "")
	if err := os.Mkdir(filepath.Join(root, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	var got string
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		got = req.Cwd
		return preparedLaunch(req), nil
	}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
	args := []string{"-C", "other", "--yolo", "prompt"}
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "codex", ProviderArgs: args}, hooks, launchTestRuntime()); err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "other") {
		t.Fatalf("selected wrong worktree %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(root, "launch.log"))
	if string(log) != strings.Join(append(resumeArgs("codex", launchSessionID), args...), "\n")+"\n" {
		t.Fatalf("rewritten argv=%q", log)
	}
}

func TestProviderLaunchPreparationCannotMutateOriginalArgs(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "")
	args := []string{"original prompt"}
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		req.Intent.ProviderArgs[0] = "changed prompt"
		return preparedLaunch(req), nil
	}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude", ProviderArgs: args}, hooks, launchTestRuntime()); err == nil {
		t.Fatal("mutating preparer accepted")
	}
	if args[0] != "original prompt" {
		t.Fatal("caller argv mutated")
	}
	if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
		t.Fatal("provider started")
	}
}

// Recording the boundary on child output keeps the receipt hook nonblocking,
// so the supervisor can observe child exit and cancellation during startup.
type providerLaunchBoundaryWriter struct {
	root string
	once sync.Once
	err  error
}

func (w *providerLaunchBoundaryWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		w.err = boundary.Record(w.root, boundary.Boundary{PrevBranch: "main", Branch: "feature", SeedID: launchSessionID})
	})
	return len(p), w.err
}

func TestProviderLaunchRestartRepreparesSamePolicyWithoutRepeatingTask(t *testing.T) {
	// Signal readiness only after the handler is installed. Select the first
	// invocation by its session ID rather than a separate filesystem marker.
	root, _ := providerLaunchFixture(t, "claude", `printf '%s\n' "$@" >> "$LAUNCH_LOG"
if [ "$2" = "`+launchSessionID+`" ]; then
  trap 'exit 0' TERM
  printf 'ready\n'
  while :; do sleep 0.01; done
fi`)
	var requests []ProviderLaunchRequest
	var receipts []ProviderLaunchReceipt
	var preparations []PreparedProviderLaunch
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		requests = append(requests, req)
		p := preparedLaunch(req)
		if len(requests) > 1 {
			p.Args = append(resumeArgs("claude", restartedSessionID), req.Intent.ProviderArgs...)
			p.SessionID = restartedSessionID
			p.CodeCommit = strings.Repeat("b", 40)
			p.SourceRevision = "revision-2"
		}
		preparations = append(preparations, p)
		return p, nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
		receipts = append(receipts, r)
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	intent := LaunchIntent{Provider: "claude", ProviderArgs: []string{"--model", "sonnet", "first task"}, Pull: true, ContextBudget: 200000}
	var stderr bytes.Buffer
	runtime := launchTestRuntime()
	boundaryWriter := &providerLaunchBoundaryWriter{root: root}
	runtime.stdout = boundaryWriter
	runtime.stderr = &stderr
	if err := runProviderLaunch(ctx, root, intent, hooks, runtime); err != nil || boundaryWriter.err != nil {
		log, logErr := os.ReadFile(filepath.Join(root, "launch.log"))
		t.Fatalf("launch: %v; boundary: %v\nstderr: %s\nlaunch log (%v): %q", err, boundaryWriter.err, stderr.String(), logErr, log)
	}
	if len(requests) != 2 || requests[1].Transition == nil || requests[1].Transition.Branch != "feature" || !requests[1].Intent.Pull || requests[1].Intent.ContextBudget != 200000 || !reflect.DeepEqual(requests[1].Intent.ProviderArgs, []string{"--model", "sonnet"}) {
		t.Fatalf("restart requests=%+v", requests)
	}
	if len(receipts) != 4 || receipts[2].CodeCommit != strings.Repeat("b", 40) || receipts[2].SourceRevision != "revision-2" {
		t.Fatalf("stale restart receipt %+v", receipts)
	}
	if len(preparations) != 2 || preparations[0].Budget.InitialPromptTokens != len([]rune("first task")) || preparations[1].Budget.InitialPromptTokens != 0 {
		t.Fatal("restart retained the first task's token reservation")
	}
	if prompt, err := requests[1].InitialPrompt(); err != nil || prompt.Present() {
		t.Fatal("restart retained the first task")
	}
	stale := preparations[1]
	stale.PromptReservation = preparations[0].PromptReservation
	if err := validatePreparedProviderLaunch(requests[1], stale); err == nil {
		t.Fatal("restart accepted the initial launch's prompt reservation")
	}
	log, _ := os.ReadFile(filepath.Join(root, "launch.log"))
	if strings.Count(string(log), "first task") != 1 || !strings.Contains(string(log), restartedSessionID) {
		t.Fatalf("restart transcript=%q", log)
	}
}

func TestProviderLaunchRejectsTransitionDuringPreparation(t *testing.T) {
	for _, during := range []string{"prepare", "receipt", "last-validation"} {
		t.Run(during, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", `printf 'started' > "$LAUNCH_LOG"`)
			mark := func() error {
				return boundary.Record(root, boundary.Boundary{PrevBranch: "main", Branch: "changed", SeedID: launchSessionID})
			}
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				p := preparedLaunch(req)
				if during == "prepare" {
					return p, mark()
				}
				if during == "last-validation" {
					p.Validate = func(context.Context) error { return domain.ErrSelectionChanged }
				}
				return p, nil
			}, Record: func(_ context.Context, receipt ProviderLaunchReceipt) error {
				if during == "receipt" && receipt.State == "prepared" {
					return mark()
				}
				return nil
			}}
			err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime())
			if !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("stale launch accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
				t.Fatal("stale provider started")
			}
		})
	}
}

func TestProviderRestartPreparationFailurePreservesRunningChild(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", `trap 'printf killed >> "$LAUNCH_LOG"; exit 1' TERM
printf ready
while [ ! -f "$LAUNCH_LOG.release" ]; do sleep 0.01; done
printf continued >> "$LAUNCH_LOG"`)
	calls := 0
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		calls++
		if calls == 1 {
			return preparedLaunch(req), nil
		}
		if err := os.WriteFile(filepath.Join(root, "launch.log.release"), []byte("release"), 0600); err != nil {
			return PreparedProviderLaunch{}, err
		}
		return PreparedProviderLaunch{}, errors.New("seed not published")
	}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
	runtime := launchTestRuntime()
	runtime.stdout = &providerLaunchBoundaryWriter{root: root}
	var stderr bytes.Buffer
	runtime.stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runProviderLaunch(ctx, root, LaunchIntent{Provider: "claude"}, hooks, runtime); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "launch.log"))
	if err != nil || string(raw) != "continued" || calls != 2 || !strings.Contains(stderr.String(), "current session was preserved") {
		t.Fatalf("child lost: %q %v calls=%d stderr=%s", raw, err, calls, stderr.String())
	}
}

func TestProviderLaunchCancellationRecordsFailureAndStopsChild(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", `trap 'exit 0' TERM
while :; do sleep 0.01; done`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var final ProviderLaunchReceipt
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		return preparedLaunch(req), nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
		final = r
		if r.State == "launched" {
			cancel()
		}
		return nil
	}}
	started := time.Now()
	err := runProviderLaunch(ctx, root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime())
	if !errors.Is(err, context.Canceled) || final.State != "failed" || time.Since(started) > 3*time.Second {
		t.Fatalf("cancel=%v final=%+v elapsed=%s", err, final, time.Since(started))
	}
}

func TestProviderLaunchArgumentDetailsUsesSameParser(t *testing.T) {
	req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: "codex", ProviderArgs: []string{"--profile", "team profile", "-c", `model="configured"`, "--model=explicit", "-C", "other", "--", "--resume is a literal prompt"}}}
	got, err := req.ArgumentDetails()
	if err != nil || got.Model != "explicit" || got.Profile != "team profile" || got.Directory != "other" || !reflect.DeepEqual(got.Prompts, []string{"--resume is a literal prompt"}) || !reflect.DeepEqual(got.ConfigOverrides, []string{`model="configured"`}) {
		t.Fatalf("details=%+v err=%v", got, err)
	}
	args, err := req.ResumeArguments(launchSessionID)
	if err != nil || !reflect.DeepEqual(args, append(resumeArgs("codex", launchSessionID), req.Intent.ProviderArgs...)) {
		t.Fatalf("prepared args=%v err=%v", args, err)
	}
	if _, err := req.ResumeArguments("../../invalid"); err == nil {
		t.Fatal("invalid resume ID accepted")
	}
}

func TestProviderLaunchDoesNotTreatNullDeviceOrPipeAsTerminal(t *testing.T) {
	file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if providerLaunchTerminal(file) {
		t.Fatal("null device is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if providerLaunchTerminal(r) || providerLaunchTerminal(w) {
		t.Fatal("pipe is not a terminal")
	}
}

func TestProviderLaunchRejectsFalsePreparationProof(t *testing.T) {
	request := ProviderLaunchRequest{Intent: LaunchIntent{Provider: "claude", ProviderArgs: []string{"--model", "sonnet", "user task"}}}
	for name, mutation := range map[string]func(*PreparedProviderLaunch){
		"bare arguments": func(p *PreparedProviderLaunch) { p.SessionID = ""; p.Args = request.Intent.ProviderArgs },
		"model override": func(p *PreparedProviderLaunch) { p.Args = append(p.Args, "--model", "different") },
		"literal boundary moved": func(p *PreparedProviderLaunch) {
			p.Args = append(resumeArgs("claude", launchSessionID), append([]string{"--"}, request.Intent.ProviderArgs...)...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := preparedLaunch(request)
			mutation(&p)
			if err := validatePreparedProviderLaunch(request, p); err == nil {
				t.Fatal("invalid preparation proof accepted")
			}
		})
	}
}

func TestProviderLaunchFailureAfterStartStopsProcessAndPersistsFailure(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", `trap 'exit 0' TERM
while :; do sleep 0.01; done`)
	var states []string
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		return preparedLaunch(req), nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error {
		states = append(states, r.State)
		if r.State == "launched" {
			return errors.New("receipt storage lost")
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	err := runProviderLaunch(ctx, root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime())
	if err == nil || !strings.Contains(err.Error(), "provider started") || !reflect.DeepEqual(states, []string{"prepared", "launched", "failed"}) {
		t.Fatalf("error=%v states=%v", err, states)
	}
}

func TestProviderLaunchStartFailureIsNotReportedAsLaunched(t *testing.T) {
	root, bin := providerLaunchFixture(t, "claude", "")
	var states []string
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		if err := os.Remove(bin); err != nil {
			return PreparedProviderLaunch{}, err
		}
		return preparedLaunch(req), nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { states = append(states, r.State); return nil }}
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime()); err == nil {
		t.Fatal("missing executable ignored")
	}
	if !reflect.DeepEqual(states, []string{"prepared", "failed"}) {
		t.Fatalf("states=%v", states)
	}
}
