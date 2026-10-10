import type { SessionArchiveView } from './types';

export function sessionArchiveIndex(archives: SessionArchiveView[]) {
  const bySnapshot = new Map<string, SessionArchiveView>();
  for (const archive of archives) {
    for (const id of [archive.snapshot_id, archive.latest_snapshot_id, ...archive.snapshot_ids]) bySnapshot.set(id, archive);
  }
  return bySnapshot;
}

export function sessionArchiveMetadataIds(archives: SessionArchiveView[]) {
  const ids = new Set(sessionArchiveIndex(archives).keys());
  for (const archive of archives) {
    if (archive.origin.parent_snapshot_id) ids.add(archive.origin.parent_snapshot_id);
    if (archive.origin.main_snapshot_id) ids.add(archive.origin.main_snapshot_id);
  }
  ids.delete('');
  return ids;
}
