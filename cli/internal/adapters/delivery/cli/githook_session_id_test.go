package cli

import (
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
)

func TestRestoredSessionID(t *testing.T) {
	const sessionID = "12345678-1234-4abc-8def-1234567890ab"
	tests := []struct {
		name      string
		path      string
		resumeCmd string
		want      string
	}{
		{
			name:      "claude materialization",
			path:      "/tmp/.claude/projects/repo/" + sessionID + ".jsonl",
			resumeCmd: "claude --resume " + sessionID,
			want:      sessionID,
		},
		{
			name:      "codex rollout filename",
			path:      "/tmp/.codex/sessions/2026/07/30/rollout-2026-07-30T12-00-00-" + sessionID + ".jsonl",
			resumeCmd: "codex resume " + sessionID,
			want:      sessionID,
		},
		{
			// The materialized file name is the independent cross-check (#34):
			// a resume command pointing at a session the materializer did not
			// write must never become the wrapper restart target.
			name:      "resume target absent from materialized file name",
			path:      "/tmp/materialized.jsonl",
			resumeCmd: "codex resume " + sessionID,
		},
		{
			name:      "materialized file name carries a different session",
			path:      "/tmp/.claude/projects/repo/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl",
			resumeCmd: "claude --resume " + sessionID,
		},
		{
			name:      "missing materialized path",
			resumeCmd: "codex resume " + sessionID,
		},
		{
			name: "missing resume command",
			path: "/tmp/" + sessionID + ".jsonl",
		},
		{
			name:      "invalid resume target",
			path:      "/tmp/" + sessionID + ".jsonl",
			resumeCmd: "codex resume ../../outside",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := restoredSessionID(tt.path, tt.resumeCmd); got != tt.want {
				t.Fatalf("restoredSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBoundaryTransitionSafe(t *testing.T) {
	const sessionID = "12345678-1234-4abc-8def-1234567890ab"
	restartable := boundary.Boundary{
		SeedPath:  "/tmp/materialized.jsonl",
		SeedID:    sessionID,
		ResumeCmd: "codex resume " + sessionID,
	}
	if !boundaryTransitionSafe(restartable, true) {
		t.Fatal("valid wrapper-managed transition was rejected")
	}
	if boundaryTransitionSafe(boundary.Boundary{}, true) {
		t.Fatal("wrapper-managed transition without a restart target was accepted")
	}
	if !boundaryTransitionSafe(boundary.Boundary{}, false) {
		t.Fatal("unmanaged superseded-only boundary was rejected")
	}
	if boundaryTransitionSafe(boundary.Boundary{
		SeedPath:  restartable.SeedPath,
		ResumeCmd: restartable.ResumeCmd,
	}, false) {
		t.Fatal("prepared transition without a valid seed ID was accepted")
	}
}
