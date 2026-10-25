// Module github.com/wnsdy95/cxthub-cli: CLI for "Git + GitHub" coding agent sessions.
// Single Go binary (cxt) provides CLI, local MCP, and hook entry points.
// Hexagonal architecture (ports & adapters); adapters use at-rest compression and native WebSocket transport.
module github.com/wnsdy95/cxthub-cli

go 1.26.9

require (
	github.com/coder/websocket v1.8.15
	github.com/klauspost/compress v1.20.3
	github.com/tiktoken-go/tokenizer v0.10.0
)

require github.com/dlclark/regexp2/v2 v2.9.1

require golang.org/x/sys v0.49.0
