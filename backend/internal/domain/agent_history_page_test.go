package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentHistoryProjectionWireKeepsLegacyShape(t *testing.T) {
	if AgentHistoryPageVersion != 1 || AgentHistoryProjectionVersion != 2 {
		t.Fatal("history projection must not replace the strict version")
	}
	request, err := json.Marshal(AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: MaxAgentHistoryPageBytes})
	if err != nil || strings.Contains(string(request), "incomplete_tail") {
		t.Fatal("legacy request gained an opt-in", err)
	}
	page := AgentHistoryPage{Version: AgentHistoryPageVersion, Total: 2, Before: 2, NextBefore: -1, Turns: []AgentHistoryTurn{}}
	raw, err := json.Marshal(page)
	if err != nil || strings.Contains(string(raw), "omitted_tail") {
		t.Fatal("legacy page gained omission metadata", err)
	}
	page.Version, page.OmittedTail = AgentHistoryProjectionVersion, &AgentHistoryTail{Start: 0, End: 2, Reason: "incomplete_tool_pair"}
	raw, err = json.Marshal(page)
	if err != nil || !strings.Contains(string(raw), `"omitted_tail":{"start":0,"end":2,"reason":"incomplete_tool_pair"}`) {
		t.Fatal("incorrect v2 omission shape", err)
	}
}
