//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowBoundProcessesAndDrift(t *testing.T) {
	for _, mode := range []string{"window-good", "window-startup-drift", "window-config-drift", "window-foreign-provider", "window-dynamic"} {
		t.Run(mode, func(t *testing.T) {
			opts, trace := fixtureOptions(t, mode)
			path := filepath.Join(opts.Cwd, "PRIVATE_catalog.json")
			cfgPath := filepath.Join(opts.Cwd, "PRIVATE_config.json")
			catalog := []byte(`{"models":[{"slug":"fixture-resolved","context_window":272000,"max_context_window":1000000}]}`)
			config := map[string]any{"model_catalog_json": path, "model_context_window": 272000, "model_provider": "openai"}
			if mode == "window-foreign-provider" {
				config["model_provider"] = "foreign"
			}
			if mode == "window-dynamic" {
				delete(config, "model_catalog_json")
			}
			raw, _ := json.Marshal(config)
			if err := os.WriteFile(path, catalog, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfgPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			opts.Env = append(opts.Env, "CXT_NATIVE_WINDOW_CONFIG="+cfgPath, "CXT_NATIVE_WINDOW_CATALOG="+path)
			s, thread, b, err := StartWindowBound(context.Background(), opts, ThreadOptions{Model: "fixture-resolved", ModelProvider: "openai"})
			if mode == "window-good" {
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if err = b.Validate(context.Background(), thread); err != nil {
					t.Fatal(err)
				}
				if b.ModelWindow().PackingUsableWindow != 258400 {
					t.Fatal("wrong packing window")
				}
				other := thread
				other.Model = "foreign"
				if err = b.Validate(context.Background(), other); err == nil {
					t.Fatal("foreign thread accepted")
				}
				original, _ := os.ReadFile(cfgPath)
				changed := []byte(strings.ReplaceAll(string(original), "272000", "100000"))
				if err = os.WriteFile(cfgPath, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if err = b.Validate(context.Background(), thread); err == nil {
					t.Fatal("changed live config accepted")
				}
			} else if err == nil || s != nil || thread != (Thread{}) || b.RuntimeScope() != "" {
				t.Fatal("invalid binding escaped", err)
			}
			starts, _ := os.ReadFile(trace + ".starts")
			expected := 2
			if mode == "window-foreign-provider" || mode == "window-dynamic" {
				expected = 1
			}
			if len(starts) != expected {
				t.Fatalf("started %d processes; expected %d", len(starts), expected)
			}
			calls, _ := os.ReadFile(trace)
			if strings.Contains(string(calls), "turn/start") || strings.Contains(string(calls), "thread/inject_items") {
				t.Fatal("window resolution performed inference or injection")
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("private path leaked")
			}
		})
	}
}
