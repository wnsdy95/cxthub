package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type recordingSave struct {
	in inbound.SaveInput
}

func (r *recordingSave) Save(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	r.in = in
	return inbound.SaveOutput{
		SnapshotID: domain.HashContent([]byte("saved")),
		Branch:     "main",
		SessionID:  "11111111-1111-4111-8111-111111111111",
	}, nil
}

type recordingStash struct {
	in inbound.StashInput
}

func (r *recordingStash) Stash(_ context.Context, in inbound.StashInput) (inbound.StashOutput, error) {
	r.in = in
	return inbound.StashOutput{
		StashID: domain.HashContent([]byte("stash")),
		Branch:  "main",
	}, nil
}

func (*recordingStash) StashPop(context.Context, string) (inbound.StashPopOutput, error) {
	return inbound.StashPopOutput{}, nil
}

func (*recordingStash) StashList(context.Context, string) ([]domain.StashEntry, error) {
	return nil, nil
}

type noOpMemorize struct{}

func (noOpMemorize) Memorize(_ context.Context, in inbound.MemorizeInput) (inbound.MemorizeOutput, error) {
	return inbound.MemorizeOutput{
		SnapshotID: domain.ContentHash(in.Ref),
		MemoryHash: domain.HashContent([]byte("memory")),
	}, nil
}

func TestSaveAndStashImplicitProviderFollowOwningWrapper(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderCodex))
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
	sessionID := "11111111-1111-4111-8111-111111111111"
	t.Setenv("CXT_WRAPPED_SESSION_ID", sessionID)
	owned := writeCodexRollout(t, home, cwd, sessionID, time.Now())

	save := &recordingSave{}
	if err := Run(&Container{Save: save, Memorize: noOpMemorize{}}, []string{"cxt", "save"}); err != nil {
		t.Fatal(err)
	}
	if save.in.Provider != domain.ProviderCodex {
		t.Fatalf("save provider = %q, want codex", save.in.Provider)
	}
	if save.in.SessionPath != owned {
		t.Fatalf("save session path = %q, want %q", save.in.SessionPath, owned)
	}

	stash := &recordingStash{}
	if err := Run(&Container{Stash: stash}, []string{"cxt", "stash", "push"}); err != nil {
		t.Fatal(err)
	}
	if stash.in.Provider != domain.ProviderCodex {
		t.Fatalf("stash provider = %q, want codex", stash.in.Provider)
	}
	if stash.in.SessionPath != owned {
		t.Fatalf("stash session path = %q, want %q", stash.in.SessionPath, owned)
	}
}

func TestSaveExplicitProviderOverridesOwningWrapper(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderCodex))
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))

	save := &recordingSave{}
	if err := Run(&Container{Save: save, Memorize: noOpMemorize{}}, []string{"cxt", "save", "--provider", "claude"}); err != nil {
		t.Fatal(err)
	}
	if save.in.Provider != domain.ProviderClaude {
		t.Fatalf("save provider = %q, want explicit claude", save.in.Provider)
	}
}

func TestStashExplicitProviderOverridesOwningWrapper(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderCodex))
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))

	stash := &recordingStash{}
	if err := Run(&Container{Stash: stash}, []string{"cxt", "stash", "push", "--provider", "claude"}); err != nil {
		t.Fatal(err)
	}
	if stash.in.Provider != domain.ProviderClaude {
		t.Fatalf("stash provider = %q, want explicit claude", stash.in.Provider)
	}
}

func TestSaveStaleWrapperFallsBackToNewestCaptureEligibleProvider(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderClaude))
	t.Setenv("CXT_WRAPPER_PID", "99999999")

	rollout := filepath.Join(home, ".codex", "sessions", "2026", "08", "28", "rollout-test.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"cwd\":%q}}\n", cwd)
	if err := os.WriteFile(rollout, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	save := &recordingSave{}
	if err := Run(&Container{Save: save, Memorize: noOpMemorize{}}, []string{"cxt", "save"}); err != nil {
		t.Fatal(err)
	}
	if save.in.Provider != domain.ProviderCodex {
		t.Fatalf("save provider = %q, want newest capture-eligible codex", save.in.Provider)
	}
}

func TestSaveOwningWrapperUsesItsExactSessionInsteadOfNewerSibling(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderCodex))
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
	ownedID := "11111111-1111-4111-8111-111111111111"
	t.Setenv("CXT_WRAPPED_SESSION_ID", ownedID)

	owned := writeCodexRollout(t, home, cwd, ownedID, time.Now().Add(-time.Hour))
	writeCodexRollout(t, home, cwd, "22222222-2222-4222-8222-222222222222", time.Now())

	save := &recordingSave{}
	if err := Run(&Container{Save: save, Memorize: noOpMemorize{}}, []string{"cxt", "save"}); err != nil {
		t.Fatal(err)
	}
	if save.in.SessionPath != owned {
		t.Fatalf("save session path = %q, want owning wrapper session %q", save.in.SessionPath, owned)
	}
}

func TestSaveOwningWrapperUsesTerminalAffinityForPreUpgradeWrapper(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TERM_SESSION_ID", "provider-affinity-test-terminal")
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderCodex))
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
	t.Setenv("CXT_WRAPPED_SESSION_ID", "")
	ownedID := "33333333-3333-4333-8333-333333333333"

	owned := writeCodexRollout(t, home, cwd, ownedID, time.Now().Add(-time.Hour))
	writeCodexRollout(t, home, cwd, "44444444-4444-4444-8444-444444444444", time.Now())
	if err := os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	capture.RecordSessionAffinity(cwd, domain.ProviderCodex, ownedID)

	save := &recordingSave{}
	if err := Run(&Container{Save: save, Memorize: noOpMemorize{}}, []string{"cxt", "save"}); err != nil {
		t.Fatal(err)
	}
	if save.in.SessionPath != owned {
		t.Fatalf("save session path = %q, want terminal-affine session %q", save.in.SessionPath, owned)
	}
}

func TestSaveOwningWrapperFailsClosedWithoutExactSessionIdentity(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TERM_SESSION_ID", "unknown-wrapper-session-terminal")
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", string(domain.ProviderCodex))
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
	t.Setenv("CXT_WRAPPED_SESSION_ID", "")
	writeCodexRollout(t, home, cwd, "55555555-5555-4555-8555-555555555555", time.Now())

	save := &recordingSave{}
	err := Run(&Container{Save: save, Memorize: noOpMemorize{}}, []string{"cxt", "save"})
	if err == nil || !strings.Contains(err.Error(), "cannot identify the codex session") {
		t.Fatalf("save error = %v, want exact-session fail-closed error", err)
	}
	if save.in.Provider != "" {
		t.Fatalf("save was dispatched to sibling session: %+v", save.in)
	}
}

func writeCodexRollout(t *testing.T, home, cwd, sessionID string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".codex", "sessions", "2026", "08", "28", "rollout-test-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", sessionID, cwd)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}
