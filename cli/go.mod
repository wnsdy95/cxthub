// Module github.com/wnsdy95/cxthub/cli: Backend for "Git + GitHub" coding agent sessions.
// Single Go binary (cxt) provides multiple entry points for serve, mcp, hook, and CLI.
// Hexagonal architecture (ports & adapters); external dependencies are limited to at-rest compression.
module github.com/wnsdy95/cxthub/cli

go 1.26.6

require github.com/klauspost/compress v1.19.2
