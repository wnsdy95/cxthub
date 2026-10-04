package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowCatalogSelection(t *testing.T) {
	thread := Thread{Model: "gpt-5.4", ModelProvider: "openai"}
	good := `{"slug":"gpt-5.4","context_window":272000,"max_context_window":1000000,"effective_context_window_percent":95}`
	window, err := resolveBoundModelWindow(thread, []byte(`{"models":[`+good+`]}`), map[string]json.RawMessage{"model_context_window": json.RawMessage(`1000000`)})
	if err != nil || window.PackingUsableWindow != 950000 || window.NativeUsableWindow != 950000 {
		t.Fatal(window, err)
	}
	for name, body := range map[string]string{
		"missing": `{}`, "null": `{"models":null}`, "duplicate models": `{"models":[],"models":[` + good + `]}`,
		"alias": `{"Models":[],"models":[` + good + `]}`, "duplicate slug": `{"models":[` + good + `,` + good + `]}`,
		"prefix match": `{"models":[{"slug":"gpt-5","context_window":1000000}]}`,
		"fallback":     `{"models":[{"slug":"gpt-5.4","context_window":1000000,"used_fallback_model_metadata":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveBoundModelWindow(thread, []byte(body), nil); err == nil {
				t.Fatal("invalid model provenance accepted")
			}
		})
	}
}
func TestWindowCatalogReadAndPrivateErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE_CATALOG.json")
	if err := os.WriteFile(path, []byte(`{"models":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, first, err := readWindowCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"models":[{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, second, err := readWindowCatalog(path)
	if err != nil || first == second {
		t.Fatal("catalog content change was not fingerprinted", err)
	}
	for _, bad := range []string{path + "PRIVATE_MISSING", filepath.Dir(path)} {
		if _, _, err := readWindowCatalog(bad); err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("private path leaked or invalid file accepted")
		}
	}
	b := WindowBinding{}
	if err := b.Validate(context.Background(), Thread{}); !errors.Is(err, ErrState) {
		t.Fatal("zero binding accepted", err)
	}
}

func TestWindowStartupProviderCannotBeReplacedByThreadOverride(t *testing.T) {
	for _, raw := range []string{`{}`, `{"model_provider":null}`, `{"model_provider":"openai"}`} {
		var config map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &config) != nil || windowStartupProvider(config) != nil {
			t.Fatal("valid native startup provider rejected")
		}
	}
	for _, raw := range []string{`{"model_provider":"fixture"}`, `{"model_provider":""}`, `{"model_provider":true}`, `{"Model_Provider":"openai"}`} {
		var config map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &config) != nil {
			t.Fatal("invalid fixture")
		}
		if windowStartupProvider(config) == nil {
			t.Fatal("unsupported or ambiguous startup provider accepted")
		}
	}
}
