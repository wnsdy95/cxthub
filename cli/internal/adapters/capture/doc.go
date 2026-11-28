// Package capture reads and masks what cxt takes from the agent tools' files.
//
// Session content is masked here before it is uploaded: the .cxtsecrets
// values, pattern scrubbing and the repository's masking policy. The package
// also finds Codex rollout files for the memory readers and snapshots the
// settings folders (.claude, .agents, .codex).
//
// Implementations:
//   - CodexCaptureSource: finds rollout-*.jsonl files in ~/.codex/sessions/YYYY/MM/DD/
//     and checks that a file is the current generation of its session.
//
// Dependency rules (domain model):
//   - Imports domain and the providerfs and gitctx adapters.
package capture
