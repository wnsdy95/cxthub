package nativeclaude

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const thinkingProgressFixture = `{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50,"session_id":"owned-session","uuid":"progress-frame","user_message_uuid":"owned-question"}`

func thinkingProgressSession() *Session {
	return &Session{id: "owned-session", version: "2.1.287",
		firstQuestion: &firstQuestionState{id: "owned-question", phase: 2},
		pending:       &pendingCall{id: "owned-question", kind: "first_question", result: make(chan json.RawMessage, 1)},
	}
}

func TestRateLimitNotificationIsNotAdmissionOrCompletion(t *testing.T) {
	for _, status := range []string{"allowed", "allowed_warning", "rejected"} {
		s := thinkingProgressSession()
		raw := []byte(`{"type":"rate_limit_event","session_id":"owned-session","uuid":"rate-frame","rate_limit_info":{"status":"` + status + `","overageStatus":"rejected","unifiedWindows":{"five_hour":{"utilization":1.1}}}}`)
		if err := s.frame(raw); err != nil {
			t.Fatal("account notification rejected", err)
		}
		if s.firstQuestion.completed || s.firstQuestion.resultReceived || s.pending.delivered || len(s.pending.result) != 0 {
			t.Fatal("usage notification advanced completion")
		}
		// A delayed account notification can arrive while closing a completed
		// exchange; it remains informational and cannot start another question.
		s.firstQuestion.phase, s.firstQuestion.completed = 3, true
		if err := s.frame(raw); err != nil {
			t.Fatal("late account notification rejected", err)
		}
		s.firstQuestion = nil
		if err := s.frame(raw); !errors.Is(err, ErrProtocol) {
			t.Fatal("no-query session accepted model usage notification")
		}
	}
}

func TestRateLimitNotificationRejectsForeignMalformedAndOversizedData(t *testing.T) {
	base := `{"type":"rate_limit_event","session_id":"owned-session","uuid":"rate-frame","rate_limit_info":{"status":"allowed"}}`
	for _, alter := range []func(*Session){
		func(s *Session) { s.version = "2.1.285" },
		func(s *Session) { s.firstQuestion.phase = 0 },
		func(s *Session) { s.firstQuestion.phase = 1 },
	} {
		s := thinkingProgressSession()
		alter(s)
		if err := s.frame([]byte(base)); !errors.Is(err, ErrProtocol) {
			t.Fatal("notification before a supported query was accepted", err)
		}
	}
	for _, tc := range []struct{ key, raw string }{
		{"session_id", `"other"`}, {"session_id", `null`}, {"uuid", `null`},
		{"rate_limit_info", `null`}, {"rate_limit_info", `[]`},
		{"rate_limit_info", `{"status":"other"}`},
		{"rate_limit_info", `{"status":"allowed","status":"rejected"}`},
		{"rate_limit_info", `{"status":"allowed","padding":"` + strings.Repeat("x", 16<<10) + `"}`},
		{"request", `{}`},
	} {
		m, _ := object([]byte(base))
		m[tc.key] = json.RawMessage(tc.raw)
		raw, _ := json.Marshal(m)
		if err := thinkingProgressSession().frame(raw); err == nil {
			t.Fatal("invalid notification accepted", tc.key)
		}
	}
}

func TestThinkingProgressDoesNotCompleteOrMutateQuestion(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		s := thinkingProgressSession()
		if ordinary {
			s.firstQuestion.ordinary = &ordinaryState{}
		}
		if err := s.frame([]byte(thinkingProgressFixture)); err != nil {
			t.Fatal("valid first-question progress rejected", err)
		}
		// A later thinking block resets its estimate. Numerical progress is
		// not accumulated as final usage and repeated frames cannot complete it.
		m, _ := object([]byte(thinkingProgressFixture))
		m["estimated_tokens"], m["estimated_tokens_delta"] = json.RawMessage("10"), json.RawMessage("10")
		raw, _ := json.Marshal(m)
		for i := 0; i < 2; i++ {
			if err := s.frame(raw); err != nil {
				t.Fatal("block reset rejected", err)
			}
		}
		q := s.firstQuestion
		if q.phase != 2 || q.completed || q.resultReceived || q.replayed || q.answer != "" || s.pending.delivered || len(s.pending.result) != 0 || len(q.assistantIDs) != 0 {
			t.Fatal("progress became response or completion evidence")
		}
	}
}

func TestThinkingProgressRejectsForeignMalformedAndInactiveFrames(t *testing.T) {
	for _, tc := range []struct{ key, raw string }{
		{"session_id", `"foreign"`}, {"session_id", `null`},
		{"user_message_uuid", `"foreign"`}, {"user_message_uuid", `null`},
		{"uuid", `""`}, {"uuid", `null`},
		{"estimated_tokens", `-1`}, {"estimated_tokens", `null`},
		{"estimated_tokens", `1.5`}, {"estimated_tokens", `"50"`},
		{"estimated_tokens", `1000000000000001`},
		{"estimated_tokens_delta", `-1`}, {"estimated_tokens_delta", `51`},
		{"estimated_tokens_delta", `null`}, {"estimated_tokens_delta", `0.5`},
		{"content", `"not a numeric progress notification"`},
	} {
		t.Run(tc.key+"/"+tc.raw, func(t *testing.T) {
			m, _ := object([]byte(thinkingProgressFixture))
			m[tc.key] = json.RawMessage(tc.raw)
			raw, _ := json.Marshal(m)
			if err := thinkingProgressSession().frame(raw); !errors.Is(err, ErrProtocol) {
				t.Fatal("invalid progress accepted", err)
			}
		})
	}
	for name, alter := range map[string]func(*Session){
		"no_query":        func(s *Session) { s.firstQuestion = nil },
		"legacy":          func(s *Session) { s.version = "2.1.285" },
		"queued":          func(s *Session) { s.firstQuestion.phase = 1 },
		"completed":       func(s *Session) { s.firstQuestion.completed = true },
		"result_received": func(s *Session) { s.firstQuestion.resultReceived = true },
		"no_pending":      func(s *Session) { s.pending = nil },
		"foreign_pending": func(s *Session) { s.pending.id = "foreign" },
		"append_pending":  func(s *Session) { s.pending.kind = "append" },
		"delivered":       func(s *Session) { s.pending.delivered = true },
	} {
		t.Run(name, func(t *testing.T) {
			s := thinkingProgressSession()
			alter(s)
			if err := s.frame([]byte(thinkingProgressFixture)); !errors.Is(err, ErrProtocol) {
				t.Fatal("inactive question accepted progress", err)
			}
		})
	}
}
