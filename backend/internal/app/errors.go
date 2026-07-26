// Package app implements server use-case services (inbound port implementation).
//
// The service receives outbound ports (MetadataStore/BlobStore/AuthProvider/GitEngine) via constructor injection.
// (All initial scaffold errNotImplemented stubs have been replaced with actual implementations —
// Audit 2026-07-09: removed the dead CreateBranch stub; no placeholders remain.
package app
