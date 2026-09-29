package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestWorkStateLaunchPrefixAndLoadGrammar(t *testing.T) {
	for _, args := range [][]string{{"--work-state", "work.json", "codex", "--yolo", "prompt"}, {"--pull", "--work-state=work.json", "codex", "--yolo", "prompt"}} {
		intent, ok, err := ParseLaunchIntent(args)
		if err != nil || !ok || intent.WorkStatePath != "work.json" || !reflect.DeepEqual(intent.ProviderArgs, []string{"--yolo", "prompt"}) {
			t.Fatalf("%v: %+v %v", args, intent, err)
		}
	}
	intent, _, err := ParseLaunchIntent([]string{"codex", "--", "--work-state", "provider-owned.json"})
	if err != nil || intent.WorkStatePath != "" || len(intent.ProviderArgs) != 3 {
		t.Fatalf("provider boundary changed: %+v %v", intent, err)
	}
	for _, args := range [][]string{{"--work-state", "work.json"}, {"main", "--work-state=work.json", "--output", "out.json"}} {
		p, _, err := parseCommand("load", args)
		if err != nil || p.flags["--work-state"] != "work.json" {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--work-state", "work.json", "--work-state=other.json", "codex"},
		{"--work-state=", "codex"}, {"--work-state"}, {"--work-state", "--pull", "codex"},
		{"--work-state", "work.json", "codex", "resume", launchSessionID},
		{"--work-state", "work.json", "codex", "fork", launchSessionID},
		{"--work-state", "work.json", "claude", "--continue"},
		{"--work-state", "work.json", "claude", "--resume=" + launchSessionID},
		{"--work-state", "work.json", "codex", "exec", "prompt"},
		{"--work-state", "work.json", "claude", "-p", "prompt"},
		{"--work-state", "work.json", "codex", "--help"},
	} {
		if _, ok, err := ParseLaunchIntent(args); !ok || err == nil {
			t.Fatalf("accepted invalid prefix %v", args)
		}
	}
	for _, args := range [][]string{{"--work-state"}, {"--work-state="}, {"--work-state", "a", "--work-state=b"}, {"--work-state", "a", "--mode", "full"}, {"--work-state", "a", "--mode", "memory", "--output", "out"}} {
		if _, _, err := parseCommand("load", args); err == nil {
			t.Fatalf("accepted invalid load %v", args)
		}
	}
}

func TestWorkStateLaunchFailsClosedBeforeChildStart(t *testing.T) {
	for _, mode := range []string{"noninteractive", "import failure", "unavailable hooks"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			calls := 0
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				calls++
				if req.Intent.WorkStatePath != filepath.Join(root, "handoff.json") {
					t.Errorf("artifact was rebound to provider --cd: %s", req.Intent.WorkStatePath)
				}
				return PreparedProviderLaunch{}, errors.New("invalid personal source")
			}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
			runtime := launchTestRuntime()
			if mode == "noninteractive" {
				runtime.interactive = false
			}
			if mode == "unavailable hooks" {
				hooks = ProviderLaunchHooks{}
			}
			err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: domain.ProviderClaude, WorkStatePath: "handoff.json"}, hooks, runtime)
			if err == nil {
				t.Fatal("invalid handoff launched")
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
				t.Fatal("bare fallback started child")
			}
			if mode == "import failure" && calls != 1 {
				t.Fatal("handoff was not prepared")
			}
		})
	}
}

type personalLoadCapture struct{ seen inbound.LoadInput }

func (f *personalLoadCapture) Load(_ context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
	f.seen = in
	return inbound.LoadOutput{}, nil
}

type personalArtifactPreparer struct {
	seen inbound.PrepareAgentContextInput
	err  error
}

func (f *personalArtifactPreparer) PrepareAgentContext(_ context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
	f.seen = in
	p := domain.AgentContextPackage{Version: 1, Policy: domain.MemoryInputPolicy(), Delivery: "prepared", ArtifactOnly: true}
	p.ID, _ = p.Digest()
	return p, f.err
}

type personalArtifactHistory struct{}

func (personalArtifactHistory) QueryHistory(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return domain.HistoryQueryResult{Position: domain.HashContent([]byte("selected")), Selection: domain.HistorySelection{Branch: "main"}}, nil
}

func TestWorkStateLoadAndArtifactWireExplicitFile(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	load := &personalLoadCapture{}
	if err := Run(&Container{Load: load}, []string{"cxt", "load", "--work-state", "handoff.json"}); err != nil {
		t.Fatal(err)
	}
	if load.seen.WorkStatePath != "handoff.json" || load.seen.Mode != "" || load.seen.PersonalScope.Complete() {
		t.Fatalf("load selection=%+v", load.seen)
	}
	preparer := &personalArtifactPreparer{}
	c := &Container{PrepareAgent: preparer, HistoryQuery: personalArtifactHistory{}, ResolveRepo: func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: "repo"}, nil }}
	output := filepath.Join(cwd, "package.json")
	p, _, err := parseCommand("load", []string{"--work-state", "handoff.json", "--output", output, "--provider", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runAgentArtifact(context.Background(), c, cwd, p); err != nil {
		t.Fatal(err)
	}
	if preparer.seen.WorkStatePath != "handoff.json" || !preparer.seen.ArtifactOnly {
		t.Fatalf("artifact selection=%+v", preparer.seen)
	}
	preparer.err = errors.New("invalid constraint")
	p.flags["--output"] = filepath.Join(cwd, "rejected.json")
	if err := runAgentArtifact(context.Background(), c, cwd, p); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := os.Stat(p.flags["--output"]); !os.IsNotExist(err) {
		t.Fatal("artifact written on validation failure")
	}
}
