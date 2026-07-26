// Package capture contains the concrete implementation of the CaptureSource outbound port.
//
// It detects active session files relative to the current working directory and receives hook events.
// Incremental capture, debouncing, and secret scrubbing are also internal logic of this package.
//
// Implementations:
//   - ClaudeCaptureSource: Detects *.jsonl files in ~/.claude/projects/<cwd-encoded>/
//   - CodexCaptureSource: Detects rollout-*.jsonl files in ~/.codex/sessions/YYYY/MM/DD/
//   - CaptureCoordinator: Common adjuster for automatic/manual capture (debouncing/incremental/lock gate)
//
// Dependency rules (domain model):
//   - Only imports domain + ports.outbound + ports.inbound (SaveSession consumer).
//   - Does not import other adapters/* packages.
package capture
