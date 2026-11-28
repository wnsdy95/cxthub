package sessionrecords

import (
	"encoding/json"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func claudeContentLine(t *testing.T, kind, uuid string, content any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": kind, "uuid": uuid, "sessionId": "session-1", "timestamp": "2031-01-02T03:00:00.000Z",
		"cwd": "/work/app", "gitBranch": "main", "message": map[string]any{"role": kind, "content": content}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func claudeContentKinds(events []domain.Event) []string {
	out := []string{}
	for _, event := range events {
		out = append(out, string(event.Kind))
	}
	return out
}
