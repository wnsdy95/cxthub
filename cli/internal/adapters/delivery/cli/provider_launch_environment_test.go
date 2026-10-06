package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	memoryadapter "github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestProviderLaunchClaudeEnvironmentReplacesInheritedProfile(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			name := "known"
			if !known {
				name = "unknown"
			}
			if inherited {
				name += "/inherited"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "config"))
				t.Setenv("LAUNCH_UNRELATED", "retained=value")
				args := []string{"--safe-mode", "--settings={\"disableAllHooks\":true}"}
				if !known {
					args = []string{"--settings", "external-settings.json"}
				}
				want := profileEnvMap(claudeMemoryProfileEnv(root, args))
				for key := range want {
					if inherited {
						t.Setenv(key, "stale-parent-profile")
					} else {
						t.Setenv(key, "")
						if err := os.Unsetenv(key); err != nil {
							t.Fatal(err)
						}
					}
				}
				if known {
					want["CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT"] = memoryadapter.ClaudeMemoryConfigFingerprint(context.Background(), root)
				}
				env := providerLaunchEnvironment(context.Background(), root, domain.ProviderClaude, args, true)
				counts := map[string]int{}
				values := map[string]string{}
				for _, entry := range env {
					key, value, _ := strings.Cut(entry, "=")
					counts[key]++
					values[key] = value
				}
				for key, value := range want {
					if counts[key] != 1 || values[key] != value {
						t.Fatalf("managed key %s count=%d, correct value=%v", key, counts[key], values[key] == value)
					}
				}
				if values["LAUNCH_UNRELATED"] != "retained=value" {
					t.Fatal("unrelated environment changed")
				}
				// Real native validation must permit the public producer's unique
				// keys, while still rejecting an injected duplicate before spawn.
				// The only executable is an owned sentinel, never a provider.
				executable := filepath.Join(root, "sentinel")
				marker := filepath.Join(root, "started")
				if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf started > \"$HOME/started\"\nexit 99\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				options := nativeclaude.Options{Executable: executable, Cwd: root, ConfigArgs: []string{"--safe-mode"}, Env: env}
				options.Env = append(append([]string{}, env...), "CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT=duplicate")
				if _, err := nativeclaude.StartFirstExchange(ctx, options); !errors.Is(err, nativeclaude.ErrState) {
					t.Fatalf("duplicate environment accepted: %v", err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("duplicate environment reached executable")
				}
				options.Env = env
				if exchange, err := nativeclaude.StartFirstExchange(ctx, options); err == nil {
					_ = exchange.Close()
					t.Fatal("sentinel is not a valid native host")
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("valid public environment was rejected before executable", err)
				}
			})
		}
	}
}
