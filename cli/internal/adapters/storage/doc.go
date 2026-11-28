// Package storage keeps the files cxt writes in the repository's local .cxt/
// directory (repoRoot = repo working tree root). It is client-specific and uses
// no SQL. Talking to the server is handled by adapters/backendclient.
//
// Implementations:
//   - FileStore: content-addressed settings bundles and the secrets editing lock.
//   - SessionUploadFiles: what was uploaded for each session (.cxt/sessions).
//
// Dependency rules (domain model):
//   - Imports domain and the .cxt path checks of adapters/providerfs.
package storage
