// Package gitctx contains the concrete implementation of the GitContext outbound port.
//
// It traverses up from cwd to find the current code repo/branch by searching for the .git directory.
// Codex records do not contain a direct gitBranch field, making this port the
// authoritative branch source for Codex capture.
//
// Implementation approach (pending, backend architecture Open Questions):
//
// os/exec git call vs pure go-git. The scaffold only provides interfaces.
//
// Dependency rules (domain model):
//   - Only import domain + ports.outbound.
//   - Do not import other adapters/* packages.
package gitctx
