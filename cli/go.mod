// Module github.com/wnsdy95/cxthub/cli: Backend for "Git + GitHub" coding agent sessions.
// Single Go binary (cxt) provides multiple entry points for serve, mcp, hook, and CLI.
// Hexagonal architecture (ports & adapters); adapters use at-rest compression and native WebSocket transport.
module github.com/wnsdy95/cxthub/cli

go 1.26.6

require (
	github.com/coder/websocket v1.8.15
	github.com/klauspost/compress v1.20.0
	github.com/tiktoken-go/tokenizer v0.8.1
)

require github.com/dlclark/regexp2/v2 v2.5.1
