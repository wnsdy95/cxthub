package cli

import "testing"

func TestSessionIDFromAgentArgs(test *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	for name, sample := range map[string]struct {
		agent string
		args  []string
		want  string
	}{
		"codex resume":          {agent: "codex", args: []string{"resume", id, "--yolo"}, want: id},
		"claude resume":         {agent: "claude", args: []string{"--resume", id}, want: id},
		"claude inline resume":  {agent: "claude", args: []string{"--resume=" + id}, want: id},
		"fresh session unknown": {agent: "codex", args: []string{"--yolo"}},
		"invalid id rejected":   {agent: "codex", args: []string{"resume", "../../other"}},
	} {
		test.Run(name, func(test *testing.T) {
			if got := sessionIDFromAgentArgs(sample.agent, sample.args); got != sample.want {
				test.Fatalf("session id = %q, want %q", got, sample.want)
			}
		})
	}
}
