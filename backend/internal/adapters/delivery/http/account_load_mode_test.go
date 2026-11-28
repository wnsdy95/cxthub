package http

import (
	"net/http"
	"testing"
)

// An older web client still sends load_mode, the removed load preference.
// The account update ignores it instead of refusing the request, and the
// account no longer carries it.
func TestUpdateMeIgnoresTheRemovedLoadMode(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var updated map[string]any
	body := map[string]any{"nickname": "Older client alias", "load_mode": "not a mode"}
	if code := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/me", body, &updated); code != http.StatusOK {
		t.Fatalf("account update that carries load_mode answered %d", code)
	}
	var me map[string]any
	if code := doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me); code != http.StatusOK {
		t.Fatalf("account read answered %d", code)
	}
	for label, user := range map[string]map[string]any{"update": updated, "read": me} {
		if user["nickname"] != "Older client alias" {
			t.Fatalf("%s lost the nickname sent with load_mode: %+v", label, user)
		}
		if _, present := user["load_mode"]; present {
			t.Fatalf("%s still carries load_mode: %+v", label, user)
		}
	}
}
