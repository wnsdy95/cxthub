// Package memory contains adapters for native-memory ingestion and deterministic self-distillation (compatibility rules).
//
// It provides implementations for two outbound ports, and the session
// summarizer that uses them:
//   - MemorySource: reads provider-native memory (Claude MEMORY.md / Codex rollout_summary).
//   - MemoryDistiller: ingests native memory when available, otherwise derives a deterministic digest from CIR.
//   - SessionSummarizer: distills a session's conversation up to each point a summary is wanted.
package memory
