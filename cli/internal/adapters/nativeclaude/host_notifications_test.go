package nativeclaude

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func hostNotification(kind string) map[string]any {
	m := map[string]any{"type": "system", "subtype": kind, "session_id": "owned", "uuid": "notification"}
	if kind == "ui_invalidate" {
		m["event"] = "ui.render"
	} else {
		m["content"], m["isMeta"], m["level"] = "PRIVATE_NATIVE_NOTICE", false, "warning"
	}
	return m
}

func TestHostNotificationsDoNotAdvanceProtocol(t *testing.T) {
	for _, phase := range []string{"initialize", "append", "first_question", "closing"} {
		for _, kind := range []string{"ui_invalidate", "informational"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				pending := &pendingCall{id: "command", kind: phase, result: make(chan json.RawMessage, 1)}
				s := &Session{id: "owned", version: "2.1.287", pending: pending, appendPhase: 2, appended: true}
				if phase == "first_question" || phase == "closing" {
					s.firstQuestion = &firstQuestionState{id: "command", phase: 2}
				}
				if phase == "closing" {
					s.closing = true
					s.firstQuestion.completed = true
				}
				beforePhase, beforeResult, beforeReceipt, beforeClosing := s.appendPhase, s.appendResult, s.receipt, s.closing
				var question firstQuestionState
				if s.firstQuestion != nil {
					question = *s.firstQuestion
				}
				raw, _ := json.Marshal(hostNotification(kind))
				if err := s.frame(raw); err != nil {
					t.Fatal(err)
				}
				if s.pending != pending || pending.delivered || len(pending.result) != 0 || s.appendPhase != beforePhase || s.appendResult != beforeResult || s.receipt != beforeReceipt || s.closing != beforeClosing {
					t.Fatal("notification changed command or acknowledgment state")
				}
				if s.firstQuestion != nil && !reflect.DeepEqual(question, *s.firstQuestion) {
					t.Fatal("notification changed first-question state")
				}
			})
		}
	}
}

func TestHostNotificationsRejectMalformedOrForeignData(t *testing.T) {
	for _, kind := range []string{"ui_invalidate", "informational"} {
		cases := map[string]func(map[string]any){
			"missing session": func(m map[string]any) { delete(m, "session_id") },
			"foreign session": func(m map[string]any) { m["session_id"] = "foreign" },
			"null session":    func(m map[string]any) { m["session_id"] = nil },
			"missing uuid":    func(m map[string]any) { delete(m, "uuid") },
			"null uuid":       func(m map[string]any) { m["uuid"] = nil },
			"unknown subtype": func(m map[string]any) { m["subtype"] = "future_notification" },
		}
		if kind == "ui_invalidate" {
			cases["missing event"] = func(m map[string]any) { delete(m, "event") }
			cases["action event"] = func(m map[string]any) { m["event"] = "ui.press" }
			cases["null instances"] = func(m map[string]any) { m["instances"] = nil }
			cases["unsupported instances"] = func(m map[string]any) { m["instances"] = []any{map[string]any{"component": "AskUserQuestion"}} }
		} else {
			cases["missing content"] = func(m map[string]any) { delete(m, "content") }
			cases["null content"] = func(m map[string]any) { m["content"] = nil }
			cases["oversized content"] = func(m map[string]any) { m["content"] = strings.Repeat("x", 16385) }
			cases["missing meta"] = func(m map[string]any) { delete(m, "isMeta") }
			cases["null meta"] = func(m map[string]any) { m["isMeta"] = nil }
			cases["unknown level"] = func(m map[string]any) { m["level"] = "grant" }
		}
		for name, mutate := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				m := hostNotification(kind)
				mutate(m)
				raw, _ := json.Marshal(m)
				s := &Session{id: "owned", version: "2.1.287"}
				if err := s.frame(raw); !errors.Is(err, ErrProtocol) {
					t.Fatalf("got %v", err)
				}
			})
		}
		t.Run(kind+"/old version", func(t *testing.T) {
			raw, _ := json.Marshal(hostNotification(kind))
			s := &Session{id: "owned", version: "2.1.285"}
			if err := s.frame(raw); !errors.Is(err, ErrProtocol) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
