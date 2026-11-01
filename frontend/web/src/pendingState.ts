import type { PendingProjectionStatus, PendingStateView, RepositoryView, Snapshot } from './types';
import { parseRevision, revisionCovers } from './repositoryUpdates';
import { sessionArchiveIndex } from './sessionArchives';

export function validatePendingState(view: PendingStateView) {
  if (!view || view.projection !== 'on-hold-v1' || 'graph' in view || !parseRevision(view.revision) ||
      typeof view.default_branch !== 'string' || !view.hold_counts || typeof view.hold_counts !== 'object' || Array.isArray(view.hold_counts)) {
    throw new Error('Invalid On Hold projection');
  }
  if (view.read_generation !== undefined) {
    const generation = view.read_generation;
    const epoch = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:(0|[1-9][0-9]*)$/;
    if (!generation || generation.rules !== 1 || generation.complete !== true ||
        typeof generation.generation !== 'string' || !/^sha256:[0-9a-f]{64}$/.test(generation.generation) ||
        typeof generation.source_epoch !== 'string' || generation.source_epoch.length > 56 || !epoch.test(generation.source_epoch) ||
        generation.source_epoch !== generation.applied_epoch || BigInt(generation.source_epoch.slice(37)) > 9223372036854775807n) {
      throw new Error('Invalid On Hold read generation');
    }
  }
  for (const key of ['refs', 'pending', 'snapshots', 'archived_sessions', 'hold', 'orphan_sessions'] as const) {
    if (!Array.isArray(view[key])) throw new Error('Incomplete On Hold projection');
  }
  for (const cluster of view.hold) {
    if (!Array.isArray(cluster.ids) || !Array.isArray(cluster.tips)) throw new Error('Invalid On Hold cluster');
  }
  return view;
}

export function pendingStateEpochChanged(previous: PendingStateView | undefined, next: PendingStateView) {
  const before = previous?.read_generation?.source_epoch;
  const after = next.read_generation?.source_epoch;
  return Boolean(before && after && before.slice(0, 36) !== after.slice(0, 36));
}

export function pendingProjectionMatches(pending: PendingStateView | undefined, status: PendingProjectionStatus | undefined) {
  if (!status || status.state === 'disabled') return true;
  const generation = pending?.read_generation;
  return status.state === 'ready' && generation?.complete === true && generation.rules === status.rules &&
    generation.generation === status.generation && generation.source_epoch === status.source_epoch &&
    generation.applied_epoch === status.source_epoch;
}

type PendingReadState = {
  fetching?: boolean;
  error?: Error | null;
  failureReason?: Error | null;
  fullError?: Error | null;
  noticeError?: Error | null;
  projection?: PendingProjectionStatus;
};

export function pendingStateRows(full?: RepositoryView, pending?: PendingStateView, read: PendingReadState = {}) {
  const accessError = [read.error, read.failureReason, read.fullError, read.noticeError].find(error =>
    error && 'status' in error && typeof error.status === 'number' && [401, 403, 404].includes(error.status));
  const inaccessible = Boolean(accessError);
  const needsAttention = !inaccessible && read.projection?.state === 'needs_attention';
  const incarnation = read.projection?.source_epoch.slice(0, 36);
  const pendingIncarnation = pending?.read_generation?.source_epoch.slice(0, 36);
  const fullIncarnation = full?.read_generation?.source_epoch.slice(0, 36);
  const pendingReset = Boolean(incarnation && pendingIncarnation && incarnation !== pendingIncarnation);
  const fullReset = Boolean(incarnation && fullIncarnation && incarnation !== fullIncarnation) || (pendingReset && !fullIncarnation);
  const currentPending = pendingReset ? undefined : pending;
  const currentFull = fullReset ? undefined : full;
  const sourceReset = pendingReset || (!pending && fullReset);
  const refreshing = !inaccessible && !needsAttention && Boolean(read.error || read.failureReason || (read.fetching && pending) ||
    sourceReset || !pendingProjectionMatches(pending, read.projection));
  const source = inaccessible ? undefined
    : currentPending && (currentPending.read_generation || !currentFull?.revision || revisionCovers(currentPending.revision, currentFull.revision)) ? currentPending
    : currentFull ? { ...currentFull, hold: currentFull.graph.hold, orphan_sessions: currentFull.graph.orphan_sessions } : undefined;
  const archiveSnapshots = source?.snapshots ?? [];
  const archiveBySnapshot = sessionArchiveIndex(source?.archived_sessions ?? []);
  const byId = new Map(archiveSnapshots.map(snapshot => [snapshot.id, snapshot]));
  const pendings = (source?.pending ?? []).filter(item => !archiveBySnapshot.has(item.target));
  const bySession = new Map(pendings.map(item => [item.session_id, item]));
  const orphans = (source?.orphan_sessions ?? []).flatMap(id => bySession.has(id) ? [bySession.get(id)!] : []);
  const chains = (source?.hold ?? []).map(cluster => ({
    tips: cluster.tips,
    chain: cluster.ids.map(id => byId.get(id)).filter((snapshot): snapshot is Snapshot => snapshot !== undefined && !archiveBySnapshot.has(snapshot.id)),
  })).filter(cluster => cluster.chain.length);
  return {
    ready: Boolean(source) && !refreshing && !needsAttention, refreshing, needsAttention, inaccessible, sourceReset, error: accessError?.message ?? read.error?.message,
    refs: source?.refs ?? [], archiveSnapshots, archiveBySnapshot, pendings, orphans, chains,
  };
}
