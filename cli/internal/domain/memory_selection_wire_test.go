package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// JSON makes this regression executable unchanged against the old wire type.
func TestMemorySelectionParentWireDependency(t *testing.T) {
	before := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(HashContent([]byte("repo"))), BranchID: "identity", Branch: "main", LocalBranch: "main", Kind: "position", Source: HashContent([]byte("b")), Target: HashContent([]byte("b")), MemoryPinned: true, GitAfter: strings.Repeat("c", 40), WorktreeID: strings.Repeat("d", 32), CreatedAt: time.Unix(20, 0).UTC()}
	after := before
	after.ID = strings.Repeat("2", 32)
	after.MemoryHash = HashContent([]byte("e"))
	after.CreatedAt = time.Unix(10, 0).UTC()
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["memory_selection_parent"] = before.ID
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	t.Run("roundtrip", func(t *testing.T) {
		encoded, err := json.Marshal(after)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if got["memory_selection_parent"] != before.ID {
			t.Fatal("dedicated memory dependency lost in wire roundtrip")
		}
	})
	t.Run("reverse-clock", func(t *testing.T) {
		got, err := OrderHistoryEvents([]HistoryEvent{after, before})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != before.ID {
			t.Fatalf("memory dependency ordered by clock: %v", got)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := OrderHistoryEvents([]HistoryEvent{after}); err == nil {
			t.Fatal("missing memory predecessor accepted")
		}
	})
	t.Run("collision", func(t *testing.T) {
		different := after
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"memory_selection_parent":%q}`, strings.Repeat("3", 32))), &different); err != nil {
			t.Fatal(err)
		}
		if _, err := OrderHistoryEvents([]HistoryEvent{before, after, different}); err == nil {
			t.Fatal("different immutable memory predecessor accepted with same ID")
		}
	})
}
