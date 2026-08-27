package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

func gitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newClaudeMemoryRepo(t *testing.T) (home, repo, linked string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CLAUDE_CODE_SIMPLE", "")
	t.Setenv("CLAUDE_CODE_SAFE_MODE", "")
	t.Setenv("CLAUDE_CODE_MANAGED_SETTINGS_PATH", "")
	t.Setenv("CLAUDE_CODE_REMOTE", "")
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", "")
	t.Setenv("CLAUDE_COWORK_MEMORY_PATH_OVERRIDE", "")
	t.Setenv("CLAUDE_MEMORY_STORES", "")
	t.Setenv("CXT_CLAUDE_MEMORY_PROFILE", "v1")
	t.Setenv("CXT_CLAUDE_SETTING_SOURCES", "user,project,local")
	t.Setenv("CXT_CLAUDE_BARE", "")
	t.Setenv("CXT_CLAUDE_SAFE_MODE", "")
	t.Setenv("CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY", "")
	t.Setenv("CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED", "")
	t.Setenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT", "")

	repo = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "init", "-b", "main")
	gitFixture(t, repo, "config", "core.hooksPath", "/dev/null")
	gitFixture(t, repo, "config", "gc.auto", "0")
	gitFixture(t, repo, "config", "maintenance.auto", "false")
	gitFixture(t, repo, "-c", "user.name=cxt test", "-c", "user.email=cxt@example.test", "commit", "--allow-empty", "-m", "root")

	linked = filepath.Join(t.TempDir(), "linked")
	gitFixture(t, repo, "worktree", "add", "-b", "linked", linked)
	return home, repo, linked
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(abs)
}

func writeClaudeMemory(t *testing.T, dir, text string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func defaultClaudeMemoryDir(home, projectRoot string) string {
	return filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(projectRoot), "memory")
}

func readClaudeNative(t *testing.T, cwd string) (string, bool) {
	t.Helper()
	t.Setenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT", ClaudeMemoryConfigFingerprint(context.Background(), cwd))
	return readClaudeNativeWithoutRefreshingProfile(t, cwd)
}

func readClaudeNativeWithoutRefreshingProfile(t *testing.T, cwd string) (string, bool) {
	t.Helper()
	native, found, err := NewClaudeMemorySource().ReadNative(context.Background(), cwd, "")
	if err != nil {
		t.Fatalf("ReadNative(%s): %v", cwd, err)
	}
	if !found {
		return "", false
	}
	return native.Text, true
}

func TestClaudeProjectKeyMatchesProviderLongUnicodeEncoding(t *testing.T) {
	root := "/" + strings.Repeat("very-long-segment/", 20) + "😀"
	want := "-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-very-long-segment-v-2g816y"
	if got := claudeProjectKey(root); got != want {
		t.Fatalf("project key=%q\nwant=%q", got, want)
	}
	if got := claudeEncodeProjectRoot("/repo/😀"); got != "-repo---" {
		t.Fatalf("non-BMP encoding=%q, want one replacement per UTF-16 unit", got)
	}
}

func TestDecodeClaudeManagedDocument(t *testing.T) {
	settings, active, err := decodeClaudeManagedDocument([]byte(`{"autoMemoryDirectory":"/managed/memory","autoMemoryEnabled":false}`))
	if err != nil || !active || settings.AutoMemoryDirectory == nil || *settings.AutoMemoryDirectory != "/managed/memory" ||
		settings.AutoMemoryEnabled == nil || *settings.AutoMemoryEnabled {
		t.Fatalf("settings=%+v active=%v err=%v", settings, active, err)
	}
	if _, active, err := decodeClaudeManagedDocument([]byte(`{}`)); err != nil || active {
		t.Fatalf("empty managed document active=%v err=%v", active, err)
	}
}

func TestReadClaudeSettingsBytesStopsAtSafetyLimit(t *testing.T) {
	reader := strings.NewReader(strings.Repeat("x", maxClaudeSettingsBytes+(32<<10)))
	if _, err := readClaudeSettingsBytes(reader); !errors.Is(err, errClaudeSettingsTooLarge) {
		t.Fatalf("oversized settings error=%v, want %v", err, errClaudeSettingsTooLarge)
	}
	consumed := maxClaudeSettingsBytes + (32 << 10) - reader.Len()
	if consumed != maxClaudeSettingsBytes+1 {
		t.Fatalf("consumed=%d bytes, want exactly max+1", consumed)
	}
}

func TestClaudeReadNativeUsesRepositoryIdentityAcrossSubdirsAndWorktrees(t *testing.T) {
	home, repo, linked := newClaudeMemoryRepo(t)
	repo = canonicalPath(t, repo)
	const wanted = "SHARED REPOSITORY MEMORY"
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, repo), wanted)

	subdir := filepath.Join(repo, "nested", "package")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, cwd := range map[string]string{"subdirectory": subdir, "linked worktree": linked} {
		t.Run(name, func(t *testing.T) {
			got, found := readClaudeNative(t, cwd)
			if !found || got != wanted {
				t.Fatalf("found=%v text=%q, want repository memory", found, got)
			}
		})
	}
}

func TestClaudeReadNativeDoesNotClaimRuntimeAutoLoadWithoutAttestation(t *testing.T) {
	_, repo, _ := newClaudeMemoryRepo(t)
	const baseline = "PORTABLE CLAUDE MEMORY BASELINE"
	resolution := resolveClaudeAutoMemory(context.Background(), repo)
	writeClaudeMemory(t, resolution.memoryDir, baseline)
	t.Setenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT", ClaudeMemoryConfigFingerprint(context.Background(), repo))

	native, found, err := NewClaudeMemorySource().ReadNative(context.Background(), repo, "")
	if err != nil || !found {
		t.Fatalf("ReadNative found=%v err=%v", found, err)
	}
	if native.Text != baseline {
		t.Fatalf("native text=%q, want %q", native.Text, baseline)
	}
	if native.AutoLoadedPrefix != "" {
		t.Fatalf("unattested auto-loaded prefix=%q, want empty", native.AutoLoadedPrefix)
	}
}

func TestClaudeReadNativeRejectsStaleWorktreeSpecificMemory(t *testing.T) {
	home, repo, linked := newClaudeMemoryRepo(t)
	repo = canonicalPath(t, repo)
	linked = canonicalPath(t, linked)
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, repo), "CANONICAL MEMORY")
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, linked), "STALE WORKTREE MEMORY")

	got, found := readClaudeNative(t, linked)
	if !found || got != "CANONICAL MEMORY" {
		t.Fatalf("found=%v text=%q, want canonical repository memory", found, got)
	}
}

func TestClaudeReadNativeHonorsConfigRootAndSafeProjectName(t *testing.T) {
	_, repo, _ := newClaudeMemoryRepo(t)
	config := filepath.Join(t.TempDir(), "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "shared_project-42")
	writeClaudeMemory(t, filepath.Join(config, "projects", "shared_project-42", "memory"), "CONFIG MEMORY")

	got, found := readClaudeNative(t, repo)
	if !found || got != "CONFIG MEMORY" {
		t.Fatalf("found=%v text=%q, want config-root memory", found, got)
	}
}

func TestClaudeReadNativeHonorsTrustedUserMemoryDirectory(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	config := filepath.Join(home, ".claude")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(`{"autoMemoryDirectory":"~/custom-claude-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaudeMemory(t, filepath.Join(home, "custom-claude-memory"), "CUSTOM MEMORY")
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "STALE DEFAULT MEMORY")

	got, found := readClaudeNative(t, repo)
	if !found || got != "CUSTOM MEMORY" {
		t.Fatalf("found=%v text=%q, want trusted custom memory", found, got)
	}
}

func TestClaudeReadNativeFailsClosedForMixedRelevantSettings(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	custom := filepath.Join(home, "custom-claude-memory")
	writeClaudeMemory(t, custom, "CUSTOM MEMORY")
	config := filepath.Join(home, ".claude")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf(`{"autoMemoryDirectory":%q,"permissions":"invalid"}`, custom)
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, found := readClaudeNative(t, repo); found {
		t.Fatalf("found memory from a settings document requiring provider-wide validation: %q", got)
	}
}

func TestClaudeReadNativeDoesNotReadDisabledAutoMemory(t *testing.T) {
	t.Run("environment override", func(t *testing.T) {
		home, repo, _ := newClaudeMemoryRepo(t)
		for _, root := range []string{repo, canonicalPath(t, repo)} {
			writeClaudeMemory(t, defaultClaudeMemoryDir(home, root), "DISABLED MEMORY")
		}
		if got, found := readClaudeNative(t, repo); !found || got != "DISABLED MEMORY" {
			t.Fatalf("enabled precondition found=%v text=%q", found, got)
		}
		t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
		if got, found := readClaudeNative(t, repo); found {
			t.Fatalf("found disabled memory %q", got)
		}
	})

	t.Run("project setting", func(t *testing.T) {
		home, repo, _ := newClaudeMemoryRepo(t)
		for _, root := range []string{repo, canonicalPath(t, repo)} {
			writeClaudeMemory(t, defaultClaudeMemoryDir(home, root), "DISABLED MEMORY")
		}
		if got, found := readClaudeNative(t, repo); !found || got != "DISABLED MEMORY" {
			t.Fatalf("enabled precondition found=%v text=%q", found, got)
		}
		settingsDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(settingsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"autoMemoryEnabled":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, found := readClaudeNative(t, repo); found {
			t.Fatalf("found disabled memory %q", got)
		}
	})
}

func TestClaudeReadNativeBareModeCannotBeReenabledByManagedSettings(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "BARE MEMORY")
	config := filepath.Join(home, ".claude")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "remote-settings.json"), []byte(`{"autoMemoryEnabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_CLAUDE_BARE", "true")
	if got, found := readClaudeNative(t, repo); found {
		t.Fatalf("found managed-enabled memory in bare mode: %q", got)
	}
}

func TestClaudeReadNativeSafeModeCannotBeReenabledByManagedSettings(t *testing.T) {
	for name, env := range map[string]string{
		"provider environment": "CLAUDE_CODE_SAFE_MODE",
		"wrapper profile":      "CXT_CLAUDE_SAFE_MODE",
	} {
		t.Run(name, func(t *testing.T) {
			home, repo, _ := newClaudeMemoryRepo(t)
			writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "SAFE MODE MEMORY")
			config := filepath.Join(home, ".claude")
			if err := os.MkdirAll(config, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(config, "remote-settings.json"), []byte(`{"autoMemoryEnabled":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(env, "true")
			if got, found := readClaudeNative(t, repo); found {
				t.Fatalf("found managed-enabled memory in safe mode: %q", got)
			}
		})
	}
}

func TestClaudeReadNativeIgnoresProjectMemoryDirectoryRedirect(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "TRUSTED DEFAULT")
	untrusted := filepath.Join(t.TempDir(), "untrusted-memory")
	writeClaudeMemory(t, untrusted, "UNTRUSTED PROJECT REDIRECT")
	settingsDir := filepath.Join(repo, ".claude")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf(`{"autoMemoryDirectory":%q}`, untrusted)
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	got, found := readClaudeNative(t, repo)
	if !found || got != "TRUSTED DEFAULT" {
		t.Fatalf("found=%v text=%q, want trusted default memory", found, got)
	}
}

func TestClaudeReadNativeFailsClosedWithoutLaunchProfile(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "UNPROVEN MEMORY")
	t.Setenv("CXT_CLAUDE_MEMORY_PROFILE", "")
	if got, found := readClaudeNative(t, repo); found {
		t.Fatalf("found unproven memory %q", got)
	}
}

func TestClaudeReadNativeFailsClosedWithoutConfigRoot(t *testing.T) {
	_, repo, _ := newClaudeMemoryRepo(t)
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT", ClaudeMemoryConfigFingerprint(context.Background(), repo))
	resolution := resolveClaudeAutoMemory(context.Background(), repo)
	if resolution.proven || resolution.memoryDir != "" {
		t.Fatalf("resolution=%+v, want unproven empty path without a config root", resolution)
	}
}

func TestClaudeReadNativeFailsClosedForUnmodeledRemoteMemoryRoots(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "LOCAL MEMORY")
	for name, env := range map[string]string{
		"remote memory": "CLAUDE_CODE_REMOTE_MEMORY_DIR",
		"cowork memory": "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE",
		"memory stores": "CLAUDE_MEMORY_STORES",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env, filepath.Join(t.TempDir(), "provider-owned"))
			if got, found := readClaudeNative(t, repo); found {
				t.Fatalf("found local memory under unmodeled runtime %s: %q", env, got)
			}
		})
	}
}

func TestClaudeReadNativeFailsClosedWhenSettingsChangeAfterLaunch(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	writeClaudeMemory(t, defaultClaudeMemoryDir(home, canonicalPath(t, repo)), "LAUNCH MEMORY")
	t.Setenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT", ClaudeMemoryConfigFingerprint(context.Background(), repo))
	config := filepath.Join(home, ".claude")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(`{"autoMemoryEnabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, found := readClaudeNativeWithoutRefreshingProfile(t, repo); found {
		t.Fatalf("found memory after launch settings changed: %q", got)
	}
}

func TestClaudeReadNativeAppliesFlagAndManagedPrecedence(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	userDir := filepath.Join(home, "user-memory")
	flagDir := filepath.Join(home, "flag-memory")
	managedDir := filepath.Join(home, "managed-memory")
	writeClaudeMemory(t, userDir, "USER MEMORY")
	writeClaudeMemory(t, flagDir, "FLAG MEMORY")
	writeClaudeMemory(t, managedDir, "MANAGED MEMORY")
	config := filepath.Join(home, ".claude")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(fmt.Sprintf(`{"autoMemoryDirectory":%q}`, userDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY", flagDir)
	if got, found := readClaudeNative(t, repo); !found || got != "FLAG MEMORY" {
		t.Fatalf("flag precedence found=%v text=%q", found, got)
	}

	if err := os.WriteFile(filepath.Join(config, "remote-settings.json"), []byte(fmt.Sprintf(`{"autoMemoryDirectory":%q}`, managedDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, found := readClaudeNative(t, repo); !found || got != "MANAGED MEMORY" {
		t.Fatalf("managed precedence found=%v text=%q", found, got)
	}
}

func TestClaudeReadNativeHonorsSettingSources(t *testing.T) {
	home, repo, _ := newClaudeMemoryRepo(t)
	defaultDir := defaultClaudeMemoryDir(home, canonicalPath(t, repo))
	userDir := filepath.Join(home, "user-memory")
	writeClaudeMemory(t, defaultDir, "DEFAULT MEMORY")
	writeClaudeMemory(t, userDir, "USER MEMORY")
	config := filepath.Join(home, ".claude")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "settings.json"), []byte(fmt.Sprintf(`{"autoMemoryDirectory":%q}`, userDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_CLAUDE_SETTING_SOURCES", "project,local")
	if got, found := readClaudeNative(t, repo); !found || got != "DEFAULT MEMORY" {
		t.Fatalf("setting-sources found=%v text=%q", found, got)
	}
}
