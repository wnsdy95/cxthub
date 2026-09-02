// Package delivery contains the driving adapters for the cxt client.
//
// It provides three client drivers as sub-packages for calling inbound ports:
//   - cli/   : command driver (cxt init/save/list/fork/checkout/load/memorize/push/pull)
//   - mcp/   : read-only stdio server (context_list/context_fetch/memory_load/context_search)
//   - hook/  : hook event handlers (cxt hook --provider X --event Y)
//
// The REST/JSON HTTP server (serve) is not responsible for client duties → it is handled by a separate backend module.
//
// CLI and hook drivers call inbound use-cases. MCP deliberately exposes a
// narrower read-store projection so an agent cannot mutate a live session.
//
// Dependency rules (domain model):
//   - Only import domain + ports.inbound (use-case calls).
//   - Do not import ports.outbound, adapters/* packages.
package delivery
