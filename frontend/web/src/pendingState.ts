import type { PendingStateView, RepositoryView, Snapshot } from './types';
import { parseRevision, revisionCovers } from './repositoryUpdates';
import { sessionArchiveIndex } from './sessionArchives';

export function validatePendingState(view: PendingStateView) {
  if (!view || view.projection !== 'on-hold-v1' || 'graph' in view || !parseRevision(view.revision) ||
      typeof view.default_branch !== 'string' || !view.hold_counts || typeof view.hold_counts !== 'object' || Array.isArray(view.hold_counts)) {
    throw new Error('Invalid On Hold projection');
  }
  for (const key of ['refs', 'pending', 'snapshots', 'archived_sessions', 'hold', 'orphan_sessions'] as const) {
    if (!Array.isArray(view[key])) throw new Error('Incomplete On Hold projection');
  }
  for (const cluster of view.hold) {
    if (!Array.isArray(cluster.ids) || !Array.isArray(cluster.tips)) throw new Error('Invalid On Hold cluster');
  }
  return view;
}

export function pendingStateRows(full?: RepositoryView, pending?: PendingStateView) {
  const source = pending && (!full?.revision || revisionCovers(pending.revision, full.revision)) ? pending
    : full ? { ...full, hold: full.graph.hold, orphan_sessions: full.graph.orphan_sessions } : undefined;
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
    ready: Boolean(source), refs: source?.refs ?? [], archiveSnapshots, archiveBySnapshot, pendings, orphans, chains,
  };
}
