package nativecodex

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// Numeric fixtures follow rust-v0.157.1, commit
// 36650394c5b38c2990ccf2a3457165ca3e9d9726:
// models-manager/src/model_info.rs:19, protocol/src/openai_models.rs:513,
// core/src/session/context_window.rs:60. No fixture asserts account entitlement.
const windowCatalogFixture = `{"slug":"fixture-model","context_window":272000,"max_context_window":872000}`

func windowThreadFixture() Thread {
	return Thread{Model: "fixture-model", ModelProvider: "fixture-provider"}
}

func TestResolveModelWindowNative01571Semantics(t *testing.T) {
	for _, tc := range []struct {
		name, model, config      string
		base, resolved, percent  int64
		packing, native, compact int64
		scope                    string
	}{
		{"defaults", windowCatalogFixture, `{}`, 272000, 272000, 95, 258400, 258400, 244800, "total"},
		{"null optional config", windowCatalogFixture, `{"model_context_window":null,"model_auto_compact_token_limit":null,"model_auto_compact_token_limit_scope":null}`, 272000, 272000, 95, 258400, 258400, 244800, "total"},
		{"smaller config", windowCatalogFixture, `{"model_context_window":128000}`, 272000, 128000, 95, 121600, 121600, 115200, "total"},
		{"larger config clamped by maximum", windowCatalogFixture, `{"model_context_window":1000000}`, 272000, 872000, 95, 828400, 828400, 784800, "total"},
		{"supported increase below maximum", windowCatalogFixture, `{"model_context_window":400000}`, 272000, 400000, 95, 380000, 380000, 360000, "total"},
		{"larger config without maximum", `{"slug":"fixture-model","context_window":272000}`, `{"model_context_window":1000000}`, 272000, 1000000, 95, 258400, 950000, 900000, "total"},
		{"maximum supplies missing base", `{"slug":"fixture-model","max_context_window":872000}`, `{}`, 872000, 872000, 95, 828400, 828400, 784800, "total"},
		{"maximum supplies null base", `{"slug":"fixture-model","context_window":null,"max_context_window":872000}`, `{}`, 872000, 872000, 95, 828400, 828400, 784800, "total"},
		{"null maximum and compact", `{"slug":"fixture-model","context_window":272000,"max_context_window":null,"auto_compact_token_limit":null}`, `{}`, 272000, 272000, 95, 258400, 258400, 244800, "total"},
		// Native applies max_context_window to overrides, not to the default.
		{"base precedence over maximum", `{"slug":"fixture-model","context_window":400000,"max_context_window":272000}`, `{}`, 400000, 400000, 95, 380000, 380000, 360000, "total"},
		{"override still obeys maximum", `{"slug":"fixture-model","context_window":400000,"max_context_window":272000}`, `{"model_context_window":400000}`, 400000, 272000, 95, 258400, 258400, 244800, "total"},
		{"integer rounding", `{"slug":"fixture-model","context_window":1001,"effective_context_window_percent":80}`, `{}`, 1001, 1001, 80, 800, 800, 900, "total"},
		{"full percentage", `{"slug":"fixture-model","context_window":1001,"effective_context_window_percent":100}`, `{}`, 1001, 1001, 100, 1001, 1001, 900, "total"},
		{"catalog compact lower", `{"slug":"fixture-model","context_window":272000,"auto_compact_token_limit":100000}`, `{}`, 272000, 272000, 95, 258400, 258400, 100000, "total"},
		{"catalog compact clamped", `{"slug":"fixture-model","context_window":272000,"auto_compact_token_limit":300000}`, `{}`, 272000, 272000, 95, 258400, 258400, 244800, "total"},
		{"config replaces catalog compact", `{"slug":"fixture-model","context_window":272000,"auto_compact_token_limit":100000}`, `{"model_auto_compact_token_limit":150000}`, 272000, 272000, 95, 258400, 258400, 150000, "total"},
		{"config compact clamped against resolved raw window", windowCatalogFixture, `{"model_context_window":128000,"model_auto_compact_token_limit":200000}`, 272000, 128000, 95, 121600, 121600, 115200, "total"},
		{"body config bypasses ninety percent clamp", windowCatalogFixture, `{"model_auto_compact_token_limit":1000000,"model_auto_compact_token_limit_scope":"body_after_prefix"}`, 272000, 272000, 95, 258400, 258400, 1000000, "body_after_prefix"},
		{"body without config uses catalog clamp", `{"slug":"fixture-model","context_window":272000,"auto_compact_token_limit":300000}`, `{"model_auto_compact_token_limit_scope":"body_after_prefix"}`, 272000, 272000, 95, 258400, 258400, 244800, "body_after_prefix"},
		{"int64 precision beyond float64", `{"slug":"fixture-model","context_window":9007199254740993}`, `{}`, 9007199254740993, 9007199254740993, 95, 8556839292003943, 8556839292003943, 8106479329266893, "total"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", json.RawMessage(tc.model), json.RawMessage(tc.config))
			want := ModelWindow{
				NativeVersion: "0.157.1", Model: "fixture-model", ModelProvider: "fixture-provider",
				BaseContextWindow: tc.base, ResolvedContextWindow: tc.resolved, EffectiveContextWindowPercent: tc.percent,
				PackingUsableWindow: tc.packing, NativeUsableWindow: tc.native,
				AutoCompactTokenLimit: tc.compact, AutoCompactTokenLimitScope: tc.scope,
			}
			if err != nil || got != want {
				t.Fatalf("got %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestResolveModelWindowRejectsUnboundOrAmbiguousEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, model, provider, catalog, config string
	}{
		{"missing selected model", "", "fixture-provider", windowCatalogFixture, `{}`},
		{"missing selected provider", "fixture-model", "", windowCatalogFixture, `{}`},
		{"provider mismatch", "fixture-model", "other-provider", windowCatalogFixture, `{}`},
		{"model mismatch", "other-model", "fixture-provider", windowCatalogFixture, `{}`},
		{"prefix alias", "fixture-model-2026-10-04", "fixture-provider", windowCatalogFixture, `{}`},
		{"namespace alias", "custom/fixture-model", "fixture-provider", windowCatalogFixture, `{}`},
		{"surrounding whitespace", " fixture-model", "fixture-provider", windowCatalogFixture, `{}`},
		{"control character", "fixture-model\n", "fixture-provider", windowCatalogFixture, `{}`},
		{"empty catalog", "fixture-model", "fixture-provider", ``, `{}`},
		{"null catalog", "fixture-model", "fixture-provider", `null`, `{}`},
		{"catalog array", "fixture-model", "fixture-provider", `[]`, `{}`},
		{"missing slug", "fixture-model", "fixture-provider", `{"context_window":272000}`, `{}`},
		{"null slug", "fixture-model", "fixture-provider", `{"slug":null,"context_window":272000}`, `{}`},
		{"missing windows not rescued by config", "fixture-model", "fixture-provider", `{"slug":"fixture-model"}`, `{"model_context_window":1000000}`},
		{"null windows", "fixture-model", "fixture-provider", `{"slug":"fixture-model","context_window":null,"max_context_window":null}`, `{}`},
		{"fallback", "fixture-model", "fixture-provider", `{"slug":"fixture-model","context_window":272000,"used_fallback_model_metadata":true}`, `{}`},
		{"null fallback marker", "fixture-model", "fixture-provider", `{"slug":"fixture-model","context_window":272000,"used_fallback_model_metadata":null}`, `{}`},
		{"string fallback marker", "fixture-model", "fixture-provider", `{"slug":"fixture-model","context_window":272000,"used_fallback_model_metadata":"false"}`, `{}`},
		{"duplicate window", "fixture-model", "fixture-provider", `{"slug":"fixture-model","context_window":272000,"context_window":1000000}`, `{}`},
		{"duplicate slug", "fixture-model", "fixture-provider", `{"slug":"other","slug":"fixture-model","context_window":272000}`, `{}`},
		{"case alias", "fixture-model", "fixture-provider", `{"slug":"fixture-model","context_window":272000,"Context_Window":1000000}`, `{}`},
		{"trailing catalog", "fixture-model", "fixture-provider", windowCatalogFixture + `{}`, `{}`},
		{"missing config", "fixture-model", "fixture-provider", windowCatalogFixture, ``},
		{"null config", "fixture-model", "fixture-provider", windowCatalogFixture, `null`},
		{"config array", "fixture-model", "fixture-provider", windowCatalogFixture, `[]`},
		{"trailing config", "fixture-model", "fixture-provider", windowCatalogFixture, `{} {}`},
		{"duplicate config", "fixture-model", "fixture-provider", windowCatalogFixture, `{"model_context_window":100000,"model_context_window":200000}`},
		{"config case alias", "fixture-model", "fixture-provider", windowCatalogFixture, `{"Model_Context_Window":100000}`},
		{"unknown scope", "fixture-model", "fixture-provider", windowCatalogFixture, `{"model_auto_compact_token_limit_scope":"future"}`},
		{"nonstring scope", "fixture-model", "fixture-provider", windowCatalogFixture, `{"model_auto_compact_token_limit_scope":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := Thread{Model: tc.model, ModelProvider: tc.provider}
			got, err := ResolveModelWindow(thread, "fixture-provider", json.RawMessage(tc.catalog), json.RawMessage(tc.config))
			if !errors.Is(err, ErrModelWindow) || got != (ModelWindow{}) {
				t.Fatalf("unbound/ambiguous evidence admitted: %+v, %v", got, err)
			}
		})
	}
}

func TestResolveModelWindowRejectsInvalidNumbers(t *testing.T) {
	fields := []struct {
		key    string
		config bool
	}{
		{"context_window", false}, {"max_context_window", false},
		{"effective_context_window_percent", false}, {"auto_compact_token_limit", false},
		{"model_context_window", true}, {"model_auto_compact_token_limit", true},
	}
	for _, field := range fields {
		for _, raw := range []string{`0`, `-1`, `-0`, `1.5`, `1.0`, `1e5`, `"100000"`, `true`, `[]`, `{}`, `9223372036854775808`, `-9223372036854775809`} {
			t.Run(field.key+"/"+raw, func(t *testing.T) {
				model := map[string]json.RawMessage{"slug": json.RawMessage(`"fixture-model"`), "context_window": json.RawMessage(`272000`)}
				config := map[string]json.RawMessage{}
				if field.config {
					config[field.key] = json.RawMessage(raw)
				} else {
					model[field.key] = json.RawMessage(raw)
				}
				modelJSON, _ := json.Marshal(model)
				configJSON, _ := json.Marshal(config)
				got, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", modelJSON, configJSON)
				if !errors.Is(err, ErrModelWindow) || got != (ModelWindow{}) {
					t.Fatalf("invalid number admitted: %+v, %v", got, err)
				}
			})
		}
	}
	for _, percent := range []string{`null`, `101`, `9223372036854775807`} {
		_, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", json.RawMessage(fmt.Sprintf(`{"slug":"fixture-model","context_window":272000,"effective_context_window_percent":%s}`, percent)), json.RawMessage(`{}`))
		if !errors.Is(err, ErrModelWindow) {
			t.Fatalf("invalid percentage %s admitted", percent)
		}
	}
}

func TestResolveModelWindowRejectsNativeArithmeticOverflowAndZeroRounding(t *testing.T) {
	for _, tc := range []struct{ name, model, config string }{
		{"base usable overflow", fmt.Sprintf(`{"slug":"fixture-model","context_window":%d}`, int64(math.MaxInt64/95+1)), `{}`},
		{"resolved usable overflow", `{"slug":"fixture-model","context_window":272000}`, fmt.Sprintf(`{"model_context_window":%d}`, int64(math.MaxInt64/95+1))},
		{"compact overflow", fmt.Sprintf(`{"slug":"fixture-model","context_window":%d,"effective_context_window_percent":1}`, int64(math.MaxInt64/9+1)), `{}`},
		{"usable rounds to zero", `{"slug":"fixture-model","context_window":1}`, `{}`},
		{"compact rounds to zero", `{"slug":"fixture-model","context_window":1,"effective_context_window_percent":100}`, `{}`},
		{"small override rounds to zero", windowCatalogFixture, `{"model_context_window":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", json.RawMessage(tc.model), json.RawMessage(tc.config))
			if !errors.Is(err, ErrModelWindow) || got != (ModelWindow{}) {
				t.Fatalf("unsafe arithmetic admitted: %+v, %v", got, err)
			}
		})
	}
	// A huge override is safe when native first clamps it to a valid maximum.
	got, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", json.RawMessage(windowCatalogFixture), json.RawMessage(fmt.Sprintf(`{"model_context_window":%d}`, int64(math.MaxInt64))))
	if err != nil || got.NativeUsableWindow != 828400 || got.PackingUsableWindow != 828400 {
		t.Fatalf("maximum was not applied before multiplication: %+v, %v", got, err)
	}
}

func TestResolveModelWindowLeavesInputsPrivateAndUnchanged(t *testing.T) {
	model := json.RawMessage(`{"slug":"fixture-model","context_window":272000,"used_fallback_model_metadata":false,"model_messages":{"instructions_template":"PRIVATE_NATIVE_INSTRUCTIONS"}}`)
	config := json.RawMessage(`{"model_context_window":128000,"private_unrelated_config":"PRIVATE_SECRET"}`)
	beforeModel, beforeConfig := string(model), string(config)
	got, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", model, config)
	if err != nil || string(model) != beforeModel || string(config) != beforeConfig || strings.Contains(fmt.Sprint(got), "PRIVATE") {
		t.Fatalf("input mutation/disclosure: %+v, %v", got, err)
	}
	_, err = ResolveModelWindow(windowThreadFixture(), "fixture-provider", json.RawMessage(`{"slug":"fixture-model","context_window":"PRIVATE_SECRET"}`), config)
	if !errors.Is(err, ErrModelWindow) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("error disclosed input: %v", err)
	}
	invalidUTF8 := append([]byte(`{"slug":"fixture-model","context_window":272000,"extra":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	if _, err := ResolveModelWindow(windowThreadFixture(), "fixture-provider", invalidUTF8, config); !errors.Is(err, ErrModelWindow) {
		t.Fatal("invalid UTF-8 admitted")
	}
}
