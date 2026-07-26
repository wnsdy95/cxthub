// Package app contains the implementation of use-case services.
//
// This package implements inbound ports and calls outbound ports.
// It orchestrates domain rules without knowing the specific provider/storage/transport method.
//
// Dependency rules (domain model):
//   - Only import domain + ports (inbound implementation, outbound calls).
//   - Do not import adapters/* packages.
//
// Codec/Capture Registry:
//
// ProviderCodec and CaptureSource have N implementations per provider, so they are stored in a registry of type map[domain.ProviderKind]outbound.ProviderCodec.
// The registry is passed to the constructor in cmd/cxt (composition root) via dependency injection.
package app
