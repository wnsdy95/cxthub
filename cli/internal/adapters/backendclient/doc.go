// Package backendclient implements the REST client for the RemoteSync outbound port.
//
// When cxt operates as a client, synchronization with the central backend server is
// entirely handled by this package using only net/http (stdlib). The server-side role (receiving/storing/validating) is
// the responsibility of the backend module, and this package acts as a negotiator for calling the REST surface (SYNC-PROTOCOL §2/§3).
//
// No external dependencies: uses only net/http + encoding/json (stdlib). No imports of CDN or external modules.
//
// Implementations:
//   - BackendClient: REST client for the central server implementing RemoteSync.
//
// Authentication (SYNC-PROTOCOL §4):
//
//	Authorization: Bearer cxt_team_<opaque>
//	X-Cxt-Identity: name="..."; email="..."; team="..."
//
// Dependency rules (SPINE §3.2):
//   - Only imports domain + ports.outbound.
//   - Does not import any adapters/* packages.
package backendclient
