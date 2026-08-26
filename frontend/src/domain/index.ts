/**
 * domain/index: Public barrel re-export for the domain layer.
 *
 * Importing this file grants access to all publicly exposed types in the domain.
 * application, infrastructure, and presentation layers reference the domain through this barrel.
 *
 * Dependency direction: domain should never import from other layers (sync).
 */

export type {
  ContentHash,
  ProviderKind,
  FidelityTier,
  RefKind,
} from './value-objects.js';

export type {
  Role,
  Envelope,
  ContentBlock,
  LockedBlob,
  ProviderMetadata,
  TurnEvent,
  MessageEvent,
  ToolCallEvent,
  ToolResultEvent,
  ReasoningEvent,
  CompactionBoundaryEvent,
  CompactionLockedEvent,
  CompactionEvent,
  CIREvent,
  CIRDocument,
} from './cir.js';

export type {
  TeamIdentity,
  Repo,
  Branch,
  Snapshot,
  Ref,
  SessionDoc,
  MemoryDigest,
  MemoryFragment,
  MemoryGraftCoverage,
  Manifest,
} from './entities.js';

export {
  isContentHash,
  verifyContentAddress,
} from './entities.js';
