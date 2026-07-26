// Package memory contains adapters for native-memory ingestion, injection, and deterministic self-distillation (compatibility rules).
//
// It provides implementations for three outbound ports:
//   - MemorySource: reads provider-native memory (Claude MEMORY.md / Codex rollout_summary).
//   - MemorySink: injects a MemoryDigest into a target-provider memory file (Claude CLAUDE.md / Codex AGENTS.md).
//   - MemoryDistiller: ingests native memory when available, otherwise derives a deterministic digest from CIR.
package memory
