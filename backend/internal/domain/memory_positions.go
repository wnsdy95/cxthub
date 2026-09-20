package domain

import (
	"sort"
	"time"
)

// MemoryPositions resolves a browsing position, never a worker's current HEAD.
// Options retain their event identity even when multiple events use one SHA.
type MemoryPositions struct {
	SnapshotID ContentHash          `json:"snapshot_id"`
	EventID    string               `json:"event_id,omitempty"`
	CodeCommit string               `json:"code_commit,omitempty"`
	Reason     string               `json:"reason"` // selected_event, unique, ambiguous, unavailable
	Options    []MemoryCodePosition `json:"options"`
}

type MemoryCodePosition struct {
	EventID    string    `json:"event_id"`
	CodeCommit string    `json:"code_commit"`
	Branch     string    `json:"branch"`
	Kind       string    `json:"kind"`
	PRNumber   int       `json:"pr_number,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ResolveMemoryPositions uses exact recorded associations, not reachability,
// snapshot timestamps, shared branch heads or another worker's newest position.
// A selected event without code evidence must not fall back to a different event.
func ResolveMemoryPositions(snapshot ContentHash, event string, history []HistoryEvent) MemoryPositions {
	out := MemoryPositions{SnapshotID: snapshot, EventID: event, Reason: "unavailable", Options: []MemoryCodePosition{}}
	codes := map[string]bool{}
	for _, h := range history {
		code, pr := "", 0
		switch h.Kind {
		case "publish", "birth":
			if h.Target == snapshot && h.Source == snapshot {
				code = h.GitAfter
			}
		case "position", "attach":
			if h.Target == snapshot {
				code = h.GitAfter
			}
		case "pr-merge":
			if h.PRCompleted && h.PR != nil && (h.Source == snapshot || h.Target == snapshot) {
				code, pr = h.PR.MergeSHA, h.PR.Number
			}
		}
		if ValidateGitOID(code) != nil {
			continue
		}
		out.Options = append(out.Options, MemoryCodePosition{EventID: h.ID, CodeCommit: code, Branch: h.Branch, Kind: h.Kind, PRNumber: pr, CreatedAt: h.CreatedAt})
		codes[code] = true
		if event != "" && h.ID == event {
			out.CodeCommit, out.Reason = code, "selected_event"
		}
	}
	sort.Slice(out.Options, func(i, j int) bool {
		a, b := out.Options[i], out.Options[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.EventID < b.EventID
	})
	if event == "" {
		if len(codes) == 1 {
			out.CodeCommit, out.Reason = out.Options[0].CodeCommit, "unique"
		} else if len(codes) > 1 {
			out.Reason = "ambiguous"
		}
	}
	return out
}
