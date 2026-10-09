//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func estimateFixture(t *testing.T, mode string) (Options, string, string, string) {
	t.Helper()
	opts, trace := fixtureOptions(t, mode)
	state := filepath.Join(opts.Cwd, "PRIVATE_native_home")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(opts.Cwd, "PRIVATE_config.json")
	cachePath := filepath.Join(state, "models_cache.json")
	writeEstimateFile(t, configPath, []byte(`{"model_provider":"openai","model_context_window":272000}`))
	writeEstimateFile(t, cachePath, estimateCache(t, time.Now().Add(-time.Minute), "["+estimateModel+"]"))
	opts.Env = append(opts.Env, "CODEX_HOME="+state, "CXT_NATIVE_WINDOW_CONFIG="+configPath)
	t.Cleanup(func() { assertEstimateNoInference(t, trace) })
	return opts, trace, configPath, cachePath
}

func assertEstimateNoInference(t *testing.T, trace string) {
	t.Helper()
	raw, err := os.ReadFile(trace)
	if os.IsNotExist(err) {
		return // rejected before launching native
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range strings.Fields(string(raw)) {
		switch method {
		case "initialize", "initialized", "config/read", "thread/start":
		default:
			t.Fatalf("unexpected inference, injection, mutation or discovery RPC: %q", method)
		}
	}
}

func TestWindowEstimatedProcesses(t *testing.T) {
	opts, trace, configPath, cachePath := estimateFixture(t, "window-dynamic")
	configBefore, _ := os.ReadFile(configPath)
	cacheBefore, _ := os.ReadFile(cachePath)
	authPath := filepath.Join(filepath.Dir(cachePath), "auth.json")
	writeEstimateFile(t, authPath, []byte("PRIVATE_AUTH_SENTINEL_NOT_JSON"))
	callerEnv := append([]string(nil), opts.Env...)
	s, thread, e, err := StartWindowEstimated(context.Background(), opts, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, binding := any(e).(WindowBinding); binding {
		t.Fatal("estimate became a binding")
	}
	if thread.Model != "fixture-resolved" || thread.ModelProvider != "openai" || e.ModelWindow().PackingUsableWindow != 258400 || e.RuntimeScope() == "" || e.CatalogHash() == "" {
		t.Fatal("native acknowledged model was not resolved", e.ModelWindow())
	}
	if _, err := time.Parse(time.RFC3339Nano, e.ObservedAt()); err != nil {
		t.Fatal("missing cache observation time", err)
	}
	if e.CatalogClientVersion() != ModelWindowNativeVersion {
		t.Fatal("missing cache writer version")
	}
	if err := e.Validate(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*Thread){
		func(th *Thread) { th.ID = "other" },
		func(th *Thread) { th.Model = "other" },
		func(th *Thread) { th.ModelProvider = "other" },
		func(th *Thread) { th.SettingsHash = "other" },
	} {
		other := thread
		alter(&other)
		if err := e.Validate(context.Background(), other); err == nil {
			t.Fatal("mismatched thread accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Validate(ctx, thread); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	configAfter, _ := os.ReadFile(configPath)
	cacheAfter, _ := os.ReadFile(cachePath)
	authAfter, _ := os.ReadFile(authPath)
	if string(configBefore) != string(configAfter) || string(cacheBefore) != string(cacheAfter) || string(authAfter) != "PRIVATE_AUTH_SENTINEL_NOT_JSON" || !reflect.DeepEqual(callerEnv, opts.Env) {
		t.Fatal("startup mutated configuration, catalog, auth or caller options")
	}
	starts, _ := os.ReadFile(trace + ".starts")
	if len(starts) != 2 {
		t.Fatalf("expected discovery plus owned runtime, got %d", len(starts))
	}
	calls, _ := os.ReadFile(trace)
	if strings.Count(string(calls), "thread/start") != 1 {
		t.Fatal("startup did not create exactly one fresh thread")
	}
	// The fixture deliberately reuses its acknowledged thread ID. Runtime scopes
	// must still distinguish two invocations with identical catalog evidence.
	s2, _, e2, err := StartWindowEstimated(context.Background(), opts, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if e.RuntimeScope() == e2.RuntimeScope() || e.CatalogHash() != e2.CatalogHash() {
		t.Fatal("scope did not partition invocation from stable evidence")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(context.Background(), thread); !errors.Is(err, ErrClosed) {
		t.Fatal("closed runtime accepted", err)
	}
}

func TestWindowEstimatedUnknownAndStartupDrift(t *testing.T) {
	for _, name := range []string{"missing cache", "malformed cache", "model mismatch", "no window", "expired", "version mismatch", "startup provider", "thread provider", "static config", "config drift", "host version"} {
		t.Run(name, func(t *testing.T) {
			mode := "window-dynamic"
			if name == "config drift" {
				mode = "window-config-drift"
			}
			if name == "host version" {
				mode = ""
			}
			opts, _, configPath, cachePath := estimateFixture(t, mode)
			threadOpts := ThreadOptions{}
			switch name {
			case "missing cache":
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
			case "malformed cache":
				writeEstimateFile(t, cachePath, []byte(`{"models":`))
			case "model mismatch":
				threadOpts.Model = "fixture-resolved-alias"
			case "no window":
				writeEstimateFile(t, cachePath, estimateCache(t, time.Now(), `[{"slug":"fixture-resolved"}]`))
			case "expired":
				writeEstimateFile(t, cachePath, estimateCache(t, time.Now().Add(-time.Hour), "["+estimateModel+"]"))
			case "version mismatch":
				raw, _ := os.ReadFile(cachePath)
				writeEstimateFile(t, cachePath, []byte(strings.Replace(string(raw), ModelWindowNativeVersion, "0.156.0", 1)))
			case "startup provider":
				writeEstimateFile(t, configPath, []byte(`{"model_provider":"foreign"}`))
				threadOpts.ModelProvider = "openai"
			case "thread provider":
				threadOpts.ModelProvider = "foreign"
			case "static config":
				raw, _ := json.Marshal(map[string]string{"model_catalog_json": cachePath})
				writeEstimateFile(t, configPath, raw)
			}
			s, thread, e, err := StartWindowEstimated(context.Background(), opts, threadOpts)
			if s != nil {
				_ = s.Close()
			}
			if err == nil || s != nil || thread != (Thread{}) || e.ModelWindow() != (ModelWindow{}) || e.RuntimeScope() != "" || e.CatalogHash() != "" {
				t.Fatal("unknown metadata yielded an estimate", err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("private path or metadata leaked")
			}
		})
	}
	// Simulate a relevant edit after discovery but before execution startup.
	opts, trace, configPath, _ := estimateFixture(t, "window-dynamic")
	opts, _, hash, err := discoverWindowConfig(context.Background(), opts, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writeEstimateFile(t, configPath, []byte(`{"model_provider":"openai","model_context_window":100000}`))
	s, _, _, err := startWindowEstimated(context.Background(), opts, ThreadOptions{}, hash)
	if s != nil {
		_ = s.Close()
	}
	if err == nil || s != nil {
		t.Fatal("startup config drift accepted")
	}
	calls, _ := os.ReadFile(trace)
	if strings.Contains(string(calls), "thread/start") {
		t.Fatal("thread created after startup config drift")
	}
}

func TestWindowEstimateValidateDriftAndRefresh(t *testing.T) {
	opts, _, configPath, cachePath := estimateFixture(t, "window-dynamic")
	s, thread, e, err := StartWindowEstimated(context.Background(), opts, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	config, _ := os.ReadFile(configPath)
	cache, _ := os.ReadFile(cachePath)
	refresh := estimateCache(t, time.Now(), `[`+estimateModel+`,{"slug":"unrelated","context_window":1000}]`)
	writeEstimateFile(t, cachePath, refresh)
	if err := e.Validate(context.Background(), thread); err != nil {
		t.Fatal("normal catalog refresh invalidated estimate", err)
	}
	for name, mutate := range map[string]func(){
		"effective config": func() {
			writeEstimateFile(t, configPath, []byte(strings.Replace(string(config), "272000", "100000", 1)))
		},
		"unknown config": func() {
			writeEstimateFile(t, configPath, []byte(`{"model_provider":"openai","model_context_window":272000,"new_setting":true}`))
		},
		"provider":          func() { writeEstimateFile(t, configPath, []byte(`{"model_provider":"other"}`)) },
		"catalog route":     func() { writeEstimateFile(t, configPath, []byte(`{"model_catalog_json":"/PRIVATE_STATIC.json"}`)) },
		"selected metadata": func() { writeEstimateFile(t, cachePath, []byte(strings.Replace(string(cache), "272000", "100000", 1))) },
		"opaque identity": func() {
			writeEstimateFile(t, cachePath, []byte(strings.Replace(string(cache), "PRIVATE_OPAQUE_CACHE_IDENTITY", "OTHER_IDENTITY", 1)))
		},
		"expired metadata": func() {
			writeEstimateFile(t, cachePath, estimateCache(t, time.Now().Add(-time.Hour), "["+estimateModel+"]"))
		},
		"missing metadata": func() {
			if err := os.Remove(cachePath); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			writeEstimateFile(t, configPath, config)
			writeEstimateFile(t, cachePath, cache)
			mutate()
			if err := e.Validate(context.Background(), thread); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("drift accepted or private data leaked", err)
			}
		})
	}
}

func TestWindowPolicyDispatchDoesNotFallback(t *testing.T) {
	for _, name := range []string{"dynamic", "null dynamic", "static", "missing static", "malformed static", "invalid static path", "invalid static type", "aliased catalog config", "missing dynamic", "static drift"} {
		t.Run(name, func(t *testing.T) {
			mode := "window-good"
			if name == "static drift" {
				mode = "window-config-drift"
			}
			opts, trace, configPath, cachePath := estimateFixture(t, mode)
			staticPath := filepath.Join(opts.Cwd, "PRIVATE_static.json")
			writeEstimateFile(t, staticPath, []byte(`{"models":[`+estimateModel+`]}`))
			config := map[string]any{"model_provider": "openai", "model_context_window": 272000}
			switch name {
			case "static", "missing static", "malformed static", "static drift":
				config["model_catalog_json"] = staticPath
			case "null dynamic":
				config["model_catalog_json"] = nil
			case "invalid static path":
				config["model_catalog_json"] = "relative.json"
			case "invalid static type":
				config["model_catalog_json"] = false
			case "aliased catalog config":
				config["Model_Catalog_Json"] = nil
			}
			if name == "missing static" {
				if err := os.Remove(staticPath); err != nil {
					t.Fatal(err)
				}
			}
			if name == "malformed static" {
				writeEstimateFile(t, staticPath, []byte(`{"models":[]}`))
			}
			if name == "missing dynamic" {
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(config)
			writeEstimateFile(t, configPath, raw)
			s, thread, policy, err := StartWindowPolicy(context.Background(), opts, ThreadOptions{})
			if s != nil {
				t.Cleanup(func() { _ = s.Close() })
			}
			switch name {
			case "dynamic", "null dynamic":
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := policy.(WindowEstimate); !ok {
					t.Fatal("dynamic route not an estimate")
				}
			case "static":
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := policy.(WindowBinding); !ok {
					t.Fatal("static route not a binding")
				}
			default:
				if err == nil || s != nil || policy != nil || thread != (Thread{}) {
					t.Fatal("invalid route fell back", err)
				}
			}
			if err == nil {
				if err := policy.Validate(context.Background(), thread); err != nil {
					t.Fatal(err)
				}
				if policy.ModelWindow().PackingUsableWindow != 258400 {
					t.Fatal("wrong resolved policy")
				}
			}
			starts, _ := os.ReadFile(trace + ".starts")
			if len(starts) > 2 {
				t.Fatal("dispatcher retried on arbitrary failure")
			}
		})
	}
}

func TestWindowEstimateConcurrentCacheWriterVersions(t *testing.T) {
	opts, _, _, cachePath := estimateFixture(t, "window-dynamic")
	raw, _ := os.ReadFile(cachePath)
	desktopCache := []byte(strings.Replace(string(raw), ModelWindowNativeVersion, "0.162.0", 1))
	writeEstimateFile(t, cachePath, desktopCache)
	s, thread, e, err := StartWindowEstimated(context.Background(), opts, ThreadOptions{})
	if err != nil {
		t.Fatal("reviewed desktop cache rejected by pinned host", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if e.CatalogClientVersion() != "0.162.0" || e.ModelWindow().NativeVersion != "0.157.1" {
		t.Fatal("cache writer confused with executing native version")
	}
	if err := e.Validate(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	// A reviewed writer can refresh identical model evidence without closing
	// the idle CLI. The receipt retains the original observation's provenance.
	writeEstimateFile(t, cachePath, raw)
	if err := e.Validate(context.Background(), thread); err != nil {
		t.Fatal("compatible concurrent cache writer blocked first question", err)
	}
	if e.CatalogClientVersion() != "0.162.0" {
		t.Fatal("original provenance silently rewritten")
	}
	writeEstimateFile(t, cachePath, []byte(strings.Replace(string(raw), "272000", "100000", 1)))
	if err := e.Validate(context.Background(), thread); err == nil {
		t.Fatal("substantive metadata drift accepted")
	}
	writeEstimateFile(t, cachePath, raw)
	s2, _, e2, err := StartWindowEstimated(context.Background(), opts, ThreadOptions{})
	if err != nil {
		t.Fatal("fresh native cache did not permit re-preparation", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if e2.CatalogClientVersion() != "0.157.1" || e.CatalogHash() != e2.CatalogHash() {
		t.Fatal("compatible writer changed selected evidence")
	}
	// Truncated concurrent writes and unreviewed writers remain unknown.
	for _, changed := range []string{
		`{"client_version":"0.162.0",`,
		strings.Replace(string(raw), ModelWindowNativeVersion, "9.999.0", 1),
		strings.Replace(string(desktopCache), `"identity":"PRIVATE_OPAQUE_CACHE_IDENTITY"`, `"identity":{"provider":"openai"}`, 1),
	} {
		writeEstimateFile(t, cachePath, []byte(changed))
		if err := e.Validate(context.Background(), thread); err == nil {
			t.Fatal("partial write or incompatible writer accepted")
		}
	}
}
