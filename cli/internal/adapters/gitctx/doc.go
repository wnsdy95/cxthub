// Package gitctx contains the concrete implementation of the GitContext outbound port.
//
// It traverses up from cwd to find the current code repo/branch by searching for the .git directory.
// Codex does not have a direct gitBranch field in records (RESEARCH-FINDINGS §39), making this port the unique branch source.
//
// Implementation approach (pending, BACKEND-ARCHITECTURE Open Questions §3):
//
// os/exec git call vs pure go-git. The scaffold only provides interfaces.
//
// Dependency rules (SPINE §3.2):
//   - Only import domain + ports.outbound.
//   - Do not import other adapters/* packages.
package gitctx
