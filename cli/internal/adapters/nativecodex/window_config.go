package nativecodex

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// windowConfigFingerprint normalizes only the 0.157.1 TUI defaults and its two
// screen-reader startup fields, on a detached copy. Runtime resolution still
// receives the original effective configuration. Unknown keys, including null
// values and nested keys, remain in the hash; no observed config supplies defaults.
//
// Source: openai/codex commit 36650394c5b38c2990ccf2a3457165ca3e9d9726:
// config/src/types.rs:746-879; config/src/tui_{effects,rendering,keymap}.rs;
// core/config.schema.json definitions.Tui; tui/src/screen_reader.rs:75-97;
// app-server/src/config_manager_service.rs:137-149. Defaults here are Serde's
// deserialization defaults, not derived Rust Tui::default() (whose bool fields
// differ). The API Config schema permits TUI through additionalProperties.
func windowConfigFingerprint(raw json.RawMessage) (string, error) {
	canonical, err := windowConfigValue(raw, 0)
	config, ok := canonical.(map[string]any)
	if err != nil || !ok {
		return "", windowConfigError()
	}
	tui, present := config["tui"]
	if !present || tui == nil {
		tui = map[string]any{}
	}
	tui, err = windowTUI01571().normalize(tui, true)
	if err != nil {
		return "", err
	}
	settings := tui.(map[string]any)
	// Only these fields can be written by the one-time screen-reader probe.
	// Validate their types above before excluding their visual-only differences.
	delete(settings, "screen_reader_detection_done")
	delete(settings, "animations")
	config["tui"] = settings
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", windowConfigError()
	}
	return windowHash(encoded), nil
}

func windowConfigError() error { return windowBindingError("invalid effective configuration") }

// Keep integer precision and reject duplicate keys at every depth. Native RPC
// input is already size bounded; the depth bound also applies to unknown data.
func windowConfigValue(raw json.RawMessage, depth int) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || depth > 64 {
		return nil, windowConfigError()
	}
	switch raw[0] {
	case '{':
		fields, ok := rpcObject(raw)
		if !ok {
			return nil, windowConfigError()
		}
		out := make(map[string]any, len(fields))
		for key, value := range fields {
			var err error
			out[key], err = windowConfigValue(value, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil, windowConfigError()
		}
		out := make([]any, len(items))
		for i, value := range items {
			var err error
			out[i], err = windowConfigValue(value, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			return nil, windowConfigError()
		}
		if _, err := decoder.Token(); err != io.EOF {
			return nil, windowConfigError()
		}
		return value, nil
	}
}

type windowTUIRule struct {
	kind     string
	value    any
	nullable bool
	enum     string
	fields   map[string]windowTUIRule
}

func (r windowTUIRule) normalize(value any, present bool) (any, error) {
	if !present {
		value = r.value
		if r.kind == "object" || r.kind == "counts" {
			value = map[string]any{}
		}
	}
	if value == nil && r.nullable {
		return nil, nil
	}
	valid := false
	switch r.kind {
	case "bool":
		_, valid = value.(bool)
	case "string", "enum":
		var text string
		text, valid = value.(string)
		if valid && r.kind == "enum" {
			valid = false
			for _, allowed := range strings.Fields(r.enum) {
				if text == allowed {
					valid = true
				}
			}
		}
	case "strings", "bindings", "notifications":
		if r.kind == "bindings" {
			_, valid = value.(string)
		}
		if r.kind == "notifications" {
			_, valid = value.(bool)
		}
		if items, ok := value.([]any); ok {
			valid = true
			for _, item := range items {
				if _, ok := item.(string); !ok {
					valid = false
				}
			}
		}
	case "uint":
		valid = windowConfigUint(value, 64)
	case "object", "counts":
		fields, ok := value.(map[string]any)
		if !ok {
			return nil, windowConfigError()
		}
		out := make(map[string]any, len(fields)+len(r.fields))
		for key, field := range fields {
			if r.kind == "counts" && !windowConfigUint(field, 32) {
				return nil, windowConfigError()
			}
			out[key] = field // unknown names and nulls are not discarded
		}
		for key, rule := range r.fields {
			field, exists := fields[key]
			var err error
			out[key], err = rule.normalize(field, exists)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if !valid {
		return nil, windowConfigError()
	}
	return value, nil
}

func windowConfigUint(value any, bits int) bool {
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	_, err := strconv.ParseUint(string(n), 10, bits)
	return err == nil
}

func windowTUI01571() windowTUIRule {
	boolean := func(v bool) windowTUIRule { return windowTUIRule{kind: "bool", value: v} }
	optional := func(kind string) windowTUIRule { return windowTUIRule{kind: kind, nullable: true} }
	enum := func(value, choices string) windowTUIRule {
		return windowTUIRule{kind: "enum", value: value, enum: choices}
	}
	optionalEnum := func(choices string) windowTUIRule { return windowTUIRule{kind: "enum", nullable: true, enum: choices} }
	flags := func(names string) windowTUIRule {
		fields := map[string]windowTUIRule{}
		for _, key := range strings.Fields(names) {
			fields[key] = boolean(true)
		}
		return windowTUIRule{kind: "object", fields: fields}
	}
	keymaps := map[string]windowTUIRule{}
	for context, names := range windowTUIKeymaps01571 {
		fields := map[string]windowTUIRule{}
		for _, key := range strings.Fields(names) {
			fields[key] = optional("bindings")
		}
		keymaps[context] = windowTUIRule{kind: "object", fields: fields}
	}
	return windowTUIRule{kind: "object", fields: map[string]windowTUIRule{
		"notifications":                   {kind: "notifications", value: true},
		"notification_method":             enum("auto", "auto osc9 bel"),
		"notification_condition":          enum("unfocused", "unfocused always"),
		"animations":                      boolean(true),
		"screen_reader_detection_done":    optional("bool"),
		"effects":                         flags("effort progress shimmer starfield title welcome"),
		"rendering":                       flags("lists math mermaid tables"),
		"show_tooltips":                   boolean(true),
		"show_server_version_notice":      boolean(true),
		"auto_recap":                      boolean(true),
		"disable_paste_burst":             optional("bool"),
		"vim_mode_default":                boolean(false),
		"question_esc_back":               boolean(true),
		"raw_output_mode":                 boolean(false),
		"fullscreen_transcript":           boolean(true),
		"alternate_screen":                enum("auto", "auto always never"),
		"status_line":                     optional("strings"),
		"status_line_use_colors":          boolean(true),
		"terminal_title":                  optional("strings"),
		"theme":                           optional("string"),
		"pet":                             optional("string"),
		"pet_anchor":                      enum("composer", "composer screen-bottom"),
		"session_picker_view":             optionalEnum("comfortable dense"),
		"resume_cwd":                      optionalEnum("current session"),
		"keymap":                          {kind: "object", fields: keymaps},
		"model_availability_nux":          {kind: "counts"},
		"terminal_resize_reflow_max_rows": optional("uint"),
	}}
}

// Exact optional action names from the pinned TuiKeymap schema. Missing actions
// become null; unknown actions/contexts remain distinct, even when null-valued.
var windowTUIKeymaps01571 = map[string]string{
	"agents":          "archive delete hide new_task new_worktree rename resume search stop toggle_grouping",
	"approval":        "approve approve_for_prefix approve_for_session cancel decline deny open_fullscreen open_thread",
	"chat":            "decrease_reasoning_effort edit_queued_message increase_reasoning_effort interrupt_turn next_permission_mode previous_permission_mode prompt_stack_back skip_question toggle_voice toggle_voice_mute",
	"composer":        "history_search_next history_search_previous queue submit toggle_shortcuts",
	"editor":          "delete_backward delete_backward_word delete_forward delete_forward_word insert_newline kill_line_end kill_line_start kill_whole_line move_down move_left move_line_end move_line_start move_right move_up move_word_left move_word_right yank",
	"global":          "clear_terminal copy find_transcript focus_activity open_agents open_external_editor open_transcript queue submit toggle_fast_mode toggle_raw_output toggle_shortcuts toggle_side_conversation toggle_vim_mode",
	"list":            "accept cancel jump_bottom jump_top move_down move_left move_right move_up page_down page_up",
	"pager":           "close close_transcript find half_page_down half_page_up jump_bottom jump_top page_down page_up scroll_down scroll_up",
	"vim_normal":      "append_after_cursor append_line_end cancel_operator change_to_line_end delete_char delete_to_line_end enter_insert enter_replace_mode find_backward find_forward insert_line_start jump_bottom jump_top move_down move_left move_line_end move_line_start move_right move_up move_word_backward move_word_end move_word_forward open_line_above open_line_below paste_after redo repeat_last_change replace_char start_change_operator start_delete_operator start_yank_operator substitute_char till_backward till_forward undo yank_line",
	"vim_operator":    "cancel delete_line motion_down motion_find_backward motion_find_forward motion_jump_bottom motion_jump_top motion_left motion_line_end motion_line_start motion_right motion_till_backward motion_till_forward motion_up motion_word_backward motion_word_end motion_word_forward select_around_text_object select_inner_text_object yank_line",
	"vim_search":      "backward forward next previous",
	"vim_text_object": "backtick big_word braces brackets cancel double_quote parentheses single_quote word",
}
