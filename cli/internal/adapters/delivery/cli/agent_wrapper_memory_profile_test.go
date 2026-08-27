package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func profileEnvMap(values []string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok {
			out[key] = val
		}
	}
	return out
}

func TestClaudeMemoryProfileEnvProjectsOnlyMemorySettings(t *testing.T) {
	env := profileEnvMap(claudeMemoryProfileEnv(t.TempDir(), []string{
		"--settings", `{"autoMemoryDirectory":"/tmp/claude-memory","autoMemoryEnabled":false}`,
		"--setting-sources", "local,user",
	}))
	if env["CXT_CLAUDE_MEMORY_PROFILE"] != "v1" {
		t.Fatalf("profile=%q", env["CXT_CLAUDE_MEMORY_PROFILE"])
	}
	if env["CXT_CLAUDE_SETTING_SOURCES"] != "local,user" {
		t.Fatalf("sources=%q", env["CXT_CLAUDE_SETTING_SOURCES"])
	}
	if env["CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY"] != "/tmp/claude-memory" ||
		env["CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED"] != "false" {
		t.Fatalf("projected profile=%v", env)
	}
	if strings.Contains(strings.Join(claudeMemoryProfileEnv(t.TempDir(), []string{
		"--settings", `{"apiKey":"DO-NOT-LEAK"}`,
	}), "\n"), "DO-NOT-LEAK") {
		t.Fatal("unrelated settings secret leaked into child environment")
	}
}

func TestClaudeMemoryProfileEnvProjectsInlineSettingsAndRuntimeModes(t *testing.T) {
	dir := t.TempDir()
	env := profileEnvMap(claudeMemoryProfileEnv(dir, []string{
		"--bare", "--safe-mode", "--settings", `{"autoMemoryDirectory":"~/memory-inline","autoMemoryEnabled":true}`,
	}))
	if env["CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY"] != "~/memory-inline" {
		t.Fatalf("directory=%q", env["CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY"])
	}
	if env["CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED"] != "true" {
		t.Fatalf("settings enabled=%q, want the projected flag value", env["CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED"])
	}
	if env["CXT_CLAUDE_BARE"] != "true" {
		t.Fatalf("bare=%q, want an independent runtime disable", env["CXT_CLAUDE_BARE"])
	}
	if env["CXT_CLAUDE_SAFE_MODE"] != "true" {
		t.Fatalf("safe mode=%q, want an independent runtime disable", env["CXT_CLAUDE_SAFE_MODE"])
	}
}

func TestClaudeMemoryProfileEnvFailsClosedForUnprovableSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch-settings.json")
	if err := os.WriteFile(path, []byte(`{"autoMemoryDirectory":"~/memory-from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"external file can race provider read": {"--settings", path},
		"mixed document needs provider schema": {"--settings", `{"autoMemoryDirectory":"~/memory-inline","permissions":"invalid"}`},
	} {
		t.Run(name, func(t *testing.T) {
			env := profileEnvMap(claudeMemoryProfileEnv(dir, args))
			if env["CXT_CLAUDE_MEMORY_PROFILE"] != "unknown" {
				t.Fatalf("profile=%v, want fail-closed unknown", env)
			}
		})
	}
}

func TestClaudeMemoryProfileEnvClearsInheritedOverrides(t *testing.T) {
	env := profileEnvMap(claudeMemoryProfileEnv(t.TempDir(), nil))
	for _, key := range []string{
		"CXT_CLAUDE_BARE",
		"CXT_CLAUDE_SAFE_MODE",
		"CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY",
		"CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED",
		"CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT",
	} {
		if value, ok := env[key]; !ok || value != "" {
			t.Fatalf("%s=%q present=%v, want an explicit empty override", key, value, ok)
		}
	}

	unknown := profileEnvMap(claudeMemoryProfileEnv(t.TempDir(), []string{"--settings"}))
	if unknown["CXT_CLAUDE_MEMORY_PROFILE"] != "unknown" {
		t.Fatalf("unknown profile=%v", unknown)
	}
	for _, key := range []string{
		"CXT_CLAUDE_SETTING_SOURCES",
		"CXT_CLAUDE_BARE",
		"CXT_CLAUDE_SAFE_MODE",
		"CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY",
		"CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED",
		"CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT",
	} {
		if value, ok := unknown[key]; !ok || value != "" {
			t.Fatalf("unknown %s=%q present=%v, want an explicit empty override", key, value, ok)
		}
	}
}

func TestClaudeMemoryProfileEnvFailsClosed(t *testing.T) {
	for name, args := range map[string][]string{
		"missing settings value": {"--settings"},
		"invalid settings JSON":  {"--settings", "{"},
		"invalid setting source": {"--setting-sources", "user,repo"},
	} {
		t.Run(name, func(t *testing.T) {
			env := profileEnvMap(claudeMemoryProfileEnv(t.TempDir(), args))
			if env["CXT_CLAUDE_MEMORY_PROFILE"] != "unknown" {
				t.Fatalf("profile=%v", env)
			}
		})
	}
}

func TestNormalizeClaudeSettingSourcesSupportsExplicitNone(t *testing.T) {
	if got, ok := normalizeClaudeSettingSources(""); !ok || got != "none" {
		t.Fatalf("empty sources=%q ok=%v", got, ok)
	}
}
