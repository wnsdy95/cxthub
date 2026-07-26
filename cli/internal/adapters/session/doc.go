// Package session materializes encoded CIR output as native target-provider session files (compatibility rules).
//
// Full-context recovery supports native resume:
//   - claude: ~/.claude/projects/<cwd-encoded>/<newid>.jsonl synthesis + resumeCmd="claude --resume <id>".
//   - codex : ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl synthesis + resumeCmd="codex resume <id>".
package session
