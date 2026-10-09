package nativecodex

import (
	"encoding/json"
	"testing"
)

func TestHandoffSearchSetting(t *testing.T) {
	for _, raw := range []string{"", "null", `"cached"`, `"live"`, `"disabled"`, `"indexed"`} {
		if _, ok := handoffSearchSetting(json.RawMessage(raw)); !ok {
			t.Fatalf("valid search setting rejected: %s", raw)
		}
	}
	for _, raw := range []string{`""`, `"unknown"`, `false`, `1`, `{}`, `[]`} {
		if _, ok := handoffSearchSetting(json.RawMessage(raw)); ok {
			t.Fatalf("invalid search setting accepted: %s", raw)
		}
	}
}

func TestHandoffInheritedSearchNeverForwardsDefaultOverride(t *testing.T) {
	for _, mode := range []string{"cached", "live", "disabled", "indexed"} {
		t.Run(mode, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.p.search = ""
			f.initialize(t)
			params := map[string]any{"threadId": f.p.thread.ID, "model": f.p.thread.Model, "excludeTurns": true, "config": map[string]string{"web_search": mode}}
			frame := handoffUnitRequest(t, "resume", "thread/resume", params)
			handoffUnitObserve(t, f.p, frame, true, false, false)
			forwarded, err := f.p.forwardResume(frame)
			if err != nil {
				t.Fatal(err)
			}
			delete(params, "config")
			want := handoffUnitRequest(t, "resume", "thread/resume", params)
			if !sameJSON(forwarded, want) {
				t.Fatal("resume changed fields other than the inherited search override")
			}
			handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, true, false)
		})
	}
}

func TestHandoffInheritedSearchStillRejectsConfigurationChanges(t *testing.T) {
	for _, config := range []string{
		`{"web_search":null}`, `{"web_search":"unknown"}`, `{"Web_Search":"live"}`,
		`{"web_search":"live","web_search":"cached"}`, `{"web_search":"live","model":"other"}`,
		`{"developer_instructions":"PRIVATE"}`, `{"sandbox_mode":"danger-full-access"}`,
	} {
		t.Run(config, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.p.search = ""
			f.initialize(t)
			frame := handoffUnitRequest(t, "resume", "thread/resume", map[string]any{"threadId": f.p.thread.ID, "config": json.RawMessage(config)})
			handoffUnitObserve(t, f.p, frame, true, false, true)
		})
	}
}

func TestHandoffExplicitSearchRemainsExactAndUnmodified(t *testing.T) {
	f := newHandoffUnitFixture(t)
	f.initialize(t)
	frame := handoffUnitRequest(t, "resume", "thread/resume", map[string]any{"threadId": f.p.thread.ID, "config": map[string]string{"web_search": "cached"}})
	handoffUnitObserve(t, f.p, frame, true, false, false)
	forwarded, err := f.p.forwardResume(frame)
	if err != nil || string(forwarded) != string(frame) {
		t.Fatal("explicit search frame was modified")
	}
	if f.p.preservesSearch(json.RawMessage(`"live"`)) {
		t.Fatal("explicit search mode changed")
	}
}
