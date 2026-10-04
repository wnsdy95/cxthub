package nativecodex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ModelWindowNativeVersion is the native implementation this resolver follows.
const ModelWindowNativeVersion = "0.157.1"

// ErrModelWindow means the supplied metadata cannot establish a packing window.
// Errors never include catalog contents, configuration values, or credentials.
var ErrModelWindow = errors.New("native Codex 0.157.1 model window is unavailable")

// ModelWindow separates a conservative packing policy from native telemetry.
// It is a calculation from caller-supplied evidence, not provider entitlement
// or proof that the running thread used that evidence. All counts are tokens.
type ModelWindow struct {
	NativeVersion                 string
	Model                         string
	ModelProvider                 string
	BaseContextWindow             int64
	ResolvedContextWindow         int64
	EffectiveContextWindowPercent int64
	PackingUsableWindow           int64
	NativeUsableWindow            int64
	// AutoCompactTokenLimit is the unbuffered native scope-relative threshold.
	// Only scope "total" can bound full initial input. "body_after_prefix"
	// counts tokens after the prefix; neither scope overrides NativeUsableWindow.
	// Native token-budget fallback buffers and post-turn triggers are separate.
	AutoCompactTokenLimit      int64
	AutoCompactTokenLimitScope string
}

// ResolveModelWindow follows rust-v0.157.1 models-manager/src/model_info.rs,
// protocol/src/openai_models.rs and core/src/session/context_window.rs.
// catalogModel must be an exact catalog entry, before native config overrides;
// effectiveConfig must be the native startup configuration object (not its RPC
// envelope). The caller must establish their provenance, native version and
// binding to this owned thread. This function does no I/O and changes no state.
//
// Provider and slug must match exactly. No prefix, namespace or fallback lookup
// is performed. Missing/null Option<i64> fields mean absent, as in native Rust;
// an omitted effective_context_window_percent defaults to 95, but null is invalid.
// Explicit numeric values must be positive int64 integers; percentages must be
// 1..100. Overflow is rejected instead of relying on native saturation/wrapping.
//
// Packing follows native resolution when the bound catalog explicitly supplies
// a maximum. Without a catalog maximum, an override may shrink but cannot enlarge
// packing beyond the base usable window. A maximum alone does not select it. NativeUsableWindow reflects the override
// (clamped to max_context_window when present) and is the telemetry comparator.
// The resolver does not apply CXTHub's 80% input policy or hidden-input reserve.
func ResolveModelWindow(thread Thread, catalogProvider string, catalogModel, effectiveConfig json.RawMessage) (ModelWindow, error) {
	fail := func(reason string) (ModelWindow, error) {
		return ModelWindow{}, fmt.Errorf("%w: %s", ErrModelWindow, reason)
	}
	if !windowIdentity(thread.Model) || !windowIdentity(thread.ModelProvider) || catalogProvider != thread.ModelProvider {
		return fail("model/provider binding")
	}
	model, ok := windowObject(catalogModel, "slug", "context_window", "max_context_window", "effective_context_window_percent", "auto_compact_token_limit", "used_fallback_model_metadata")
	if !ok {
		return fail("invalid catalog object")
	}
	var slug string
	if json.Unmarshal(model["slug"], &slug) != nil || slug != thread.Model {
		return fail("catalog slug must match exactly")
	}
	if fallback, present := model["used_fallback_model_metadata"]; present && string(bytes.TrimSpace(fallback)) != "false" {
		return fail("fallback or ambiguous model metadata")
	}
	config, ok := windowObject(effectiveConfig, "model_context_window", "model_auto_compact_token_limit", "model_auto_compact_token_limit_scope")
	if !ok {
		return fail("invalid effective config object")
	}
	base, hasBase, err := windowOptionalCount(model, "context_window")
	if err != nil {
		return ModelWindow{}, err
	}
	maximum, hasMaximum, err := windowOptionalCount(model, "max_context_window")
	if err != nil {
		return ModelWindow{}, err
	}
	if !hasBase {
		if !hasMaximum {
			return fail("catalog has no window")
		}
		base = maximum // native context_window.or(max_context_window)
	}
	percent := int64(95)
	if raw, present := model["effective_context_window_percent"]; present {
		percent, err = windowPositiveCount(raw)
		if err != nil || percent > 100 {
			return fail("invalid effective context percentage")
		}
	}
	configured, hasConfigured, err := windowOptionalCount(config, "model_context_window")
	if err != nil {
		return ModelWindow{}, err
	}
	resolved := base
	if hasConfigured {
		resolved = configured
		if hasMaximum {
			resolved = min(resolved, maximum)
		}
	}
	baseUsable, err := windowRatio(base, percent, 100)
	if err != nil {
		return ModelWindow{}, err
	}
	nativeUsable, err := windowRatio(resolved, percent, 100)
	if err != nil {
		return ModelWindow{}, err
	}
	compact, err := windowRatio(resolved, 9, 10)
	if err != nil {
		return ModelWindow{}, err
	}
	catalogCompact, hasCatalogCompact, err := windowOptionalCount(model, "auto_compact_token_limit")
	if err != nil {
		return ModelWindow{}, err
	}
	configCompact, hasConfigCompact, err := windowOptionalCount(config, "model_auto_compact_token_limit")
	if err != nil {
		return ModelWindow{}, err
	}
	if hasConfigCompact {
		compact = min(compact, configCompact) // config replaces the catalog limit
	} else if hasCatalogCompact {
		compact = min(compact, catalogCompact)
	}
	scope := "total"
	if raw, present := config["model_auto_compact_token_limit_scope"]; present && string(bytes.TrimSpace(raw)) != "null" {
		if json.Unmarshal(raw, &scope) != nil || (scope != "total" && scope != "body_after_prefix") {
			return fail("invalid auto-compaction scope")
		}
	}
	if scope == "body_after_prefix" && hasConfigCompact {
		// Native uses config.limit.or_else(model.limit) here, bypassing the
		// model's 90% clamp for the body-relative configured threshold only.
		compact = configCompact
	}
	packing := min(baseUsable, nativeUsable)
	if hasConfigured && hasMaximum {
		packing = nativeUsable
	}
	return ModelWindow{
		NativeVersion: ModelWindowNativeVersion, Model: thread.Model, ModelProvider: thread.ModelProvider,
		BaseContextWindow: base, ResolvedContextWindow: resolved, EffectiveContextWindowPercent: percent,
		PackingUsableWindow: packing, NativeUsableWindow: nativeUsable,
		AutoCompactTokenLimit: compact, AutoCompactTokenLimitScope: scope,
	}, nil
}

func windowIdentity(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

func windowObject(raw json.RawMessage, fields ...string) (map[string]json.RawMessage, bool) {
	object, ok := rpcObject(raw)
	if !ok {
		return nil, false
	}
	for key := range object {
		for _, field := range fields {
			if key != field && strings.EqualFold(key, field) {
				return nil, false
			}
		}
	}
	return object, true
}

func windowOptionalCount(object map[string]json.RawMessage, field string) (int64, bool, error) {
	raw, present := object[field]
	if !present || string(bytes.TrimSpace(raw)) == "null" {
		return 0, false, nil
	}
	count, err := windowPositiveCount(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%w: invalid %s", ErrModelWindow, field)
	}
	return count, true, nil
}

func windowPositiveCount(raw json.RawMessage) (int64, error) {
	count, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || count <= 0 {
		return 0, ErrModelWindow
	}
	return count, nil
}

func windowRatio(count, multiplier, divisor int64) (int64, error) {
	if count > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%w: native window arithmetic overflow", ErrModelWindow)
	}
	result := count * multiplier / divisor
	if result <= 0 {
		return 0, fmt.Errorf("%w: native window rounds to zero", ErrModelWindow)
	}
	return result, nil
}
