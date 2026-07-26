// Package codec contains the concrete implementation of the ProviderCodec outbound port.
//
// claude/codex handles the bidirectional conversion between raw JSONL and CIR v1.
// This package also handles tool name mapping (claude Bash/Edit ↔ codex shell/apply_patch …).
//
// Implementations:
//   - ClaudeCodec: Converts Claude Code JSONL (.claude/projects/…/*.jsonl) ↔ CIR
//   - CodexCodec: Converts Codex rollout JSONL (.codex/sessions/…/rollout-*.jsonl) ↔ CIR
//
// CIR v1 schema: schemas/cir.schema.json
// Mapping contract: the provider compatibility rules
//
// Dependency rules (domain model):
//   - Only import domain + ports.outbound.
//   - Do not import other adapters/* packages.
package codec
