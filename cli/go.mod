// Module github.com/wnsdy95/cxthub-cli: CLI for "Git + GitHub" coding agent sessions.
// Single Go binary (cxt) provides CLI, local MCP, and hook entry points.
// Hexagonal architecture (ports & adapters); adapters use at-rest compression and native WebSocket transport.
module github.com/wnsdy95/cxthub-cli

go 1.26.11

require (
	github.com/klauspost/compress v1.20.5
	github.com/tiktoken-go/tokenizer v0.10.0
)

require github.com/dlclark/regexp2/v2 v2.13.2
