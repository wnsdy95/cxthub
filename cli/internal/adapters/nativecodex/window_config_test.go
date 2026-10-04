package nativecodex

import (
	"encoding/json"
	"strings"
	"testing"
)

func windowConfigTestHash(t *testing.T, raw string) string {
	t.Helper()
	hash, err := windowConfigFingerprint(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestWindowConfig01571SerdeDefaults(t *testing.T) {
	defaults, err := windowTUI01571().normalize(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(defaults)
	// Independently derived from the pinned core/config.schema.json Tui property
	// defaults, with absent Option<bool> defaults supplied as null from types.rs.
	const schemaDefaultHash = "sha256:0fb5d350dbba6336bd2eb4c9dd95fbd1e143e6e986deb4ffdaa0e31c9fb4110f"
	if err != nil || windowHash(raw) != schemaDefaultHash {
		t.Fatal("normalization defaults differ from the pinned native Serde schema", err)
	}
	base := windowConfigTestHash(t, `{}`)
	for _, input := range []string{
		`{"tui":null}`, `{"tui":{}}`, `{"tui":{"screen_reader_detection_done":true}}`,
		`{"tui":{"screen_reader_detection_done":false,"animations":false}}`,
		`{"tui":{"screen_reader_detection_done":null,"animations":true}}`,
		`{"tui":{"auto_recap":true,"effects":{},"rendering":{},"keymap":{"global":{}}}}`,
		`{"tui":` + string(raw) + `}`,
	} {
		if got := windowConfigTestHash(t, input); got != base {
			t.Fatal("native default/startup expansion changed fingerprint")
		}
	}
}

func TestWindowConfigKeepsSemanticAndUnknownChanges(t *testing.T) {
	base := windowConfigTestHash(t, `{}`)
	for _, tui := range []string{
		`{"auto_recap":false}`, `{"resume_cwd":"current"}`, `{"disable_paste_burst":false}`,
		`{"keymap":{"global":{"submit":"ctrl-enter"}}}`,
		`{"model_availability_nux":{"fixture":1}}`, `{"terminal_resize_reflow_max_rows":0}`,
		`{"effects":{"shimmer":false}}`, `{"rendering":{"tables":false}}`,
		`{"status_line":[]}`, `{"theme":"private-theme"}`, `{"unknown":null}`,
		`{"effects":{"unknown":null}}`, `{"rendering":{"unknown":null}}`,
		`{"keymap":{"unknown":null}}`, `{"keymap":{"global":{"unknown":null}}}`,
	} {
		if got := windowConfigTestHash(t, `{"tui":`+tui+`}`); got == base {
			t.Fatalf("configuration change ignored: %s", tui)
		}
	}
	for _, raw := range []string{`{"unknown":null}`, `{"model_context_window":1000000}`, `{"model_catalog_json":"other"}`} {
		if windowConfigTestHash(t, raw) == base {
			t.Fatal("non-TUI configuration was discarded")
		}
	}
	a := `{"tui":{"unknown":{"nested":null,"n":9007199254740992}}}`
	b := strings.ReplaceAll(a, "9007199254740992", "9007199254740993")
	if windowConfigTestHash(t, a) == windowConfigTestHash(t, b) {
		t.Fatal("unknown integer precision lost")
	}
	if windowConfigTestHash(t, `{"tui":{"unknown":{"nested":null}}}`) == windowConfigTestHash(t, `{"tui":{"unknown":{}}}`) {
		t.Fatal("unknown nested null discarded")
	}
}

func TestWindowConfigRejectsMalformedKnownTypesAndAmbiguity(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{"tui":true}`, `{"tui":[]}`, `{"tui":{},"tui":null}`,
		`{"tui":{"auto_recap":null}}`, `{"tui":{"auto_recap":0}}`,
		`{"tui":{"animations":null}}`, `{"tui":{"animations":"false"}}`,
		`{"tui":{"screen_reader_detection_done":1}}`, `{"tui":{"disable_paste_burst":[]}}`,
		`{"tui":{"effects":null}}`, `{"tui":{"effects":{"shimmer":"true"}}}`,
		`{"tui":{"rendering":[]}}`, `{"tui":{"rendering":{"math":null}}}`,
		`{"tui":{"resume_cwd":"unknown"}}`, `{"tui":{"alternate_screen":null}}`,
		`{"tui":{"notifications":null}}`, `{"tui":{"notifications":[true]}}`,
		`{"tui":{"notification_method":"unknown"}}`, `{"tui":{"theme":5}}`,
		`{"tui":{"status_line":[null]}}`, `{"tui":{"keymap":null}}`,
		`{"tui":{"keymap":{"composer":null}}}`, `{"tui":{"keymap":{"composer":{"submit":{}}}}}`,
		`{"tui":{"keymap":{"composer":{"submit":[false]}}}}`,
		`{"tui":{"model_availability_nux":null}}`, `{"tui":{"model_availability_nux":{"fixture":null}}}`,
		`{"tui":{"model_availability_nux":{"fixture":4294967296}}}`,
		`{"tui":{"terminal_resize_reflow_max_rows":-1}}`, `{"tui":{"terminal_resize_reflow_max_rows":1.0}}`,
		`{"tui":{"terminal_resize_reflow_max_rows":1e2}}`, `{"tui":{"terminal_resize_reflow_max_rows":18446744073709551616}}`,
		`{"tui":{"effects":{"shimmer":true,"shimmer":false}}}`,
		`{"tui":{"unknown":{"x":null,"x":1}}}`, `{"unknown":[{"x":null,"x":1}]}`,
	} {
		if _, err := windowConfigFingerprint(json.RawMessage(raw)); err == nil {
			t.Fatalf("malformed config accepted: %s", raw)
		}
	}
	if _, err := windowConfigFingerprint(json.RawMessage(`{"tui":{"auto_recap":"PRIVATE_VALUE"}}`)); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("invalid value leaked")
	}
}

func TestWindowConfigLeavesOriginalAndCustomSettingsUntouched(t *testing.T) {
	raw := json.RawMessage(`{"model_context_window":1000000,"tui":{"auto_recap":false,"resume_cwd":"session","keymap":{"global":{"submit":["ctrl-enter"]}},"unknown":null,"screen_reader_detection_done":true,"animations":false}}`)
	original := string(raw)
	first, err := windowConfigFingerprint(raw)
	if err != nil || string(raw) != original {
		t.Fatal("normalization mutated original effective config", err)
	}
	second := windowConfigTestHash(t, strings.ReplaceAll(strings.ReplaceAll(original, `"animations":false`, `"animations":true`), `"screen_reader_detection_done":true`, `"screen_reader_detection_done":null`))
	if first != second {
		t.Fatal("visual startup differences affected customized configuration")
	}
	for _, valid := range []string{
		`{"tui":{"notifications":[],"keymap":{"global":{"submit":null}}}}`,
		`{"tui":{"model_availability_nux":{"fixture":4294967295},"terminal_resize_reflow_max_rows":18446744073709551615}}`,
	} {
		windowConfigTestHash(t, valid)
	}
}
