package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const estimateModel = `{"slug":"fixture-resolved","context_window":272000,"max_context_window":1000000,"effective_context_window_percent":95}`

func estimateCache(t *testing.T, at time.Time, models string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"fetched_at": at.UTC().Format(time.RFC3339Nano), "client_version": ModelWindowNativeVersion,
		"identity": "PRIVATE_OPAQUE_CACHE_IDENTITY", "etag": "PRIVATE_ETAG", "models": json.RawMessage(models),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeEstimateFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWindowEstimateCacheValidation(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	thread := Thread{Model: "fixture-resolved", ModelProvider: "openai"}
	good := string(estimateCache(t, now, "["+estimateModel+"]"))
	for name, body := range map[string]string{
		"valid":                    good,
		"reviewed desktop version": strings.Replace(good, ModelWindowNativeVersion, "0.162.0", 1),
		"missing":                  "", "malformed": "{", "null root": "null", "array root": "[]",
		"wrong version":              strings.Replace(good, ModelWindowNativeVersion, "0.158.0", 1),
		"nonwhole version":           strings.Replace(good, ModelWindowNativeVersion, "0.157.1-alpha", 1),
		"missing version":            strings.Replace(good, `"client_version":"0.157.1",`, "", 1),
		"null version":               strings.Replace(good, `"0.157.1"`, "null", 1),
		"unscoped legacy":            strings.Replace(good, `"identity":"PRIVATE_OPAQUE_CACHE_IDENTITY",`, "", 1),
		"empty identity":             strings.Replace(good, "PRIVATE_OPAQUE_CACHE_IDENTITY", "", 1),
		"null identity":              strings.Replace(good, `"PRIVATE_OPAQUE_CACHE_IDENTITY"`, "null", 1),
		"bad etag":                   strings.Replace(good, `"PRIVATE_ETAG"`, "23", 1),
		"no timestamp":               strings.Replace(good, `"fetched_at":"2026-10-09T12:00:00Z",`, "", 1),
		"bad timestamp":              strings.Replace(good, "2026-10-09T12:00:00Z", "PRIVATE_BAD_DATE", 1),
		"expired":                    string(estimateCache(t, now.Add(-windowEstimateCacheTTL-time.Nanosecond), "["+estimateModel+"]")),
		"future":                     string(estimateCache(t, now.Add(time.Nanosecond), "["+estimateModel+"]")),
		"missing models":             strings.Replace(good, `,"models":[`+estimateModel+`]`, "", 1),
		"null models":                strings.Replace(good, "["+estimateModel+"]", "null", 1),
		"empty models":               strings.Replace(good, "["+estimateModel+"]", "[]", 1),
		"duplicate envelope":         `{"client_version":"wrong",` + good[1:],
		"aliased envelope":           `{"Client_Version":"0.157.1",` + good[1:],
		"duplicate slug":             string(estimateCache(t, now, "["+estimateModel+","+estimateModel+"]")),
		"unknown model":              strings.Replace(good, "fixture-resolved", "another-model", 1),
		"prefix only":                strings.Replace(good, "fixture-resolved", "fixture", 1),
		"namespace only":             strings.Replace(good, "fixture-resolved", "custom/fixture-resolved", 1),
		"missing window":             strings.Replace(good, estimateModel, `{"slug":"fixture-resolved"}`, 1),
		"bad window":                 strings.Replace(good, `"context_window":272000`, `"context_window":"272000"`, 1),
		"zero window":                strings.Replace(good, `"context_window":272000`, `"context_window":0`, 1),
		"fallback metadata":          strings.Replace(good, `"context_window":272000`, `"context_window":272000,"used_fallback_model_metadata":true`, 1),
		"duplicate selected field":   strings.Replace(good, `"context_window":272000`, `"context_window":272000,"context_window":1000000`, 1),
		"aliased selected field":     strings.Replace(good, `"context_window":272000`, `"Context_Window":272000`, 1),
		"duplicate nested extension": strings.Replace(good, `"context_window":272000`, `"context_window":272000,"extension":{"x":1,"x":2}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "PRIVATE_models_cache.json")
			if name != "missing" {
				writeEstimateFile(t, path, []byte(body))
			}
			raw, hash, observed, err := readWindowEstimateCache(path, thread, now)
			var window ModelWindow
			if err == nil {
				window, err = resolveBoundModelWindow(thread, raw, map[string]json.RawMessage{})
			}
			if name == "valid" || name == "reviewed desktop version" {
				if err != nil || window.PackingUsableWindow != 258400 || observed != "2026-10-09T12:00:00Z" || !strings.HasPrefix(hash, "sha256:") {
					t.Fatal(window, observed, hash, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("invalid metadata accepted or private metadata leaked", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "models_cache.json")
	writeEstimateFile(t, path, []byte(good))
	if _, _, _, err := readWindowEstimateCache(path, thread, now.Add(windowEstimateCacheTTL)); err != nil {
		t.Fatal("native TTL boundary rejected", err)
	}
	if _, _, _, err := readWindowEstimateCache(path, thread, now.Add(windowEstimateCacheTTL+time.Nanosecond)); err == nil {
		t.Fatal("expired evidence accepted")
	}
	if _, _, _, err := readWindowEstimateCache(filepath.Dir(path), thread, now); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestWindowEstimateSelectedEvidenceFingerprint(t *testing.T) {
	now := time.Now().UTC()
	thread := Thread{Model: "fixture-resolved", ModelProvider: "openai"}
	path := filepath.Join(t.TempDir(), "models_cache.json")
	good := estimateCache(t, now.Add(-time.Minute), "["+estimateModel+"]")
	writeEstimateFile(t, path, good)
	_, hash, observed, err := readWindowEstimateCache(path, thread, now)
	if err != nil {
		t.Fatal(err)
	}
	refresh := estimateCache(t, now, `[{"slug":"another-model","context_window":4096},`+estimateModel+`]`)
	refresh = []byte(strings.Replace(string(refresh), "PRIVATE_ETAG", "NEW_ETAG", 1))
	refresh = []byte(strings.Replace(string(refresh), ModelWindowNativeVersion, "0.162.0", 1))
	writeEstimateFile(t, path, refresh)
	_, newHash, newObserved, err := readWindowEstimateCache(path, thread, now)
	if err != nil || hash != newHash || observed == newObserved {
		t.Fatal("timestamp/ETag/unrelated model refresh changed selected evidence", err)
	}
	for _, changed := range []string{
		strings.Replace(string(good), "PRIVATE_OPAQUE_CACHE_IDENTITY", "CHANGED_IDENTITY", 1),
		strings.Replace(string(good), "272000", "100000", 1),
		strings.Replace(string(good), `"context_window":272000`, `"context_window":272000,"unknown_setting":true`, 1),
	} {
		writeEstimateFile(t, path, []byte(changed))
		_, newHash, _, err := readWindowEstimateCache(path, thread, now)
		if err != nil || hash == newHash {
			t.Fatal("selected metadata/provenance drift not fingerprinted", err)
		}
	}
	// Object reordering is not evidence drift.
	var decoded any
	if err := json.Unmarshal(good, &decoded); err != nil {
		t.Fatal(err)
	}
	reformatted, _ := json.MarshalIndent(decoded, "", "  ")
	writeEstimateFile(t, path, reformatted)
	_, newHash, _, err = readWindowEstimateCache(path, thread, now)
	if err != nil || hash != newHash {
		t.Fatal("formatting changed selected evidence", err)
	}
}

func TestWindowEstimateNativeHomeSelection(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{filepath.Join(root, "state"), filepath.Join(root, ".codex")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", filepath.Join(root, "PRIVATE_WRONG_AMBIENT_HOME"))
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"explicit", []string{"CODEX_HOME=" + filepath.Join(root, "state")}, "state"},
		{"relative", []string{"CODEX_HOME=state"}, "state"},
		{"last environment entry wins", []string{"CODEX_HOME=bad", "CODEX_HOME=state"}, "state"},
		{"default in child home", []string{"HOME=" + root}, ".codex"},
		{"empty override", []string{"CODEX_HOME=", "HOME=" + root}, ".codex"},
		{"missing home", nil, ""},
		{"empty environment", []string{}, ""},
		{"relative HOME", []string{"HOME=relative"}, ""},
		{"absent explicit home", []string{"CODEX_HOME=PRIVATE_ABSENT", "HOME=" + root}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := windowEstimateCachePath(tc.env, root)
			if tc.want == "" {
				if err == nil || path != "" || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatal("unproven home accepted or private path leaked", err)
				}
				return
			}
			canonical, _ := filepath.EvalSymlinks(filepath.Join(root, tc.want))
			if err != nil || path != filepath.Join(canonical, "models_cache.json") {
				t.Fatal("wrong native home selected", err)
			}
		})
	}
}

func TestWindowEstimateZeroAndPrivacy(t *testing.T) {
	e := WindowEstimate{}
	if !errors.Is(e.Validate(context.Background(), Thread{}), ErrModelWindow) || e.RuntimeScope() != "" || e.ModelWindow() != (ModelWindow{}) {
		t.Fatal("zero estimate usable")
	}
	e.catalogPath = "PRIVATE_PATH"
	e.configHash = "PRIVATE_CONFIG"
	e.catalogHash = "PRIVATE_HASH"
	e.observedAt = "PRIVATE_TIMESTAMP"
	raw, err := json.Marshal(e)
	if err != nil || string(raw) != "{}" || strings.Contains(fmt.Sprintf("%v %+v %#v %s", e, e, e, e.Evidence()), "PRIVATE") {
		t.Fatal("estimate leaked private state")
	}
	if e.Evidence() != (WindowEstimate{}).Evidence() || e.Source() != "native_model_cache" || !e.Estimated() {
		t.Fatal("unstable estimate disclosure")
	}
}

func TestWindowEstimateRelativeHomeCanonicalization(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{filepath.Join(root, "target", "child"), filepath.Join(root, "target", "state"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "target", "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	path, err := windowEstimateCachePath([]string{"CODEX_HOME=link/../state"}, root)
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "target", "state"))
	if err != nil || path != filepath.Join(want, "models_cache.json") {
		t.Fatal("relative home resolved differently from native", err)
	}
	if _, err := windowEstimateCachePath([]string{"CODEX_HOME=" + string([]byte{0xff})}, root); err == nil {
		t.Fatal("non-Unicode native environment accepted")
	}
}
