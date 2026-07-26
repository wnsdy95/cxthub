// Package delivery contains the driving adapters for the cxt client.
//
// It provides three client drivers as sub-packages for calling inbound ports:
//   - cli/   : cobra CLI (cxt init/save/list/fork/checkout/load/diff/memorize/memory/push/pull)
//   - mcp/   : stdio MCP server (session_save/session_list/… tools, domain model)
//   - hook/  : hook event handlers (cxt hook --provider X --event Y)
//
// The REST/JSON HTTP server (serve) is not responsible for client duties → it is handled by a separate backend module.
//
// All drivers call the same set of inbound ports (use-cases).
// They do not contaminate the core logic with transmission methods.
//
// Dependency rules (domain model):
//   - Only import domain + ports.inbound (use-case calls).
//   - Do not import ports.outbound, adapters/* packages.
package delivery
