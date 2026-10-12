// Module github.com/wnsdy95/cxthub-cli: CLI for "Git + GitHub" coding agent sessions.
// Single Go binary (cxt) provides CLI, local MCP, and hook entry points.
// Hexagonal architecture (ports & adapters); adapters use at-rest compression and native WebSocket transport.
module github.com/wnsdy95/cxthub-cli

go 1.26.9

require (
	github.com/coder/websocket v1.8.15
	github.com/klauspost/compress v1.20.1
	github.com/tiktoken-go/tokenizer v0.8.1
)

require github.com/dlclark/regexp2/v2 v2.8.3

require golang.org/x/sys v0.48.0
