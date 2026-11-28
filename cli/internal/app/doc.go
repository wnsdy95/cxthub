// Package app contains the implementation of use-case services.
//
// This package implements inbound ports and calls outbound ports.
// It orchestrates domain rules without knowing the specific provider/storage/transport method.
//
// Dependency rules (domain model):
//   - Only import domain + ports (inbound implementation, outbound calls).
//   - Do not import adapters/* packages.
//
// Provider registries:
//
// ProviderCodec, SessionMaterializer and SessionContentSource have one implementation per provider, so they are passed as maps keyed by domain.ProviderKind
// (for example map[domain.ProviderKind]outbound.ProviderCodec) to the constructors in cmd/cxt (composition root) via dependency injection.
package app
