import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { api } from '../api';
import { useSessionArchive } from '../hooks';
import { useT } from '../i18n';
import { atLeast, type Role } from '../roles';
import { short, when } from '../snapshotFormat';
import type { SessionArchiveView, Snapshot } from '../types';
import { saveBlob } from '../zip';

export function SessionArchiveActions({ repoId, snapshotId, docHash, archive, role }: {
  repoId: string; snapshotId: string; docHash: string; archive?: SessionArchiveView; role: Role | null;
}) {
  const t = useT();
  const mutation = useSessionArchive();
  const [confirming, setConfirming] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState('');
  const trigger = useRef<HTMLButtonElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!confirming) return;
    dialog.current?.querySelector<HTMLButtonElement>('button')?.focus();
    return () => trigger.current?.focus();
  }, [confirming]);
  useEffect(() => { if (confirming && mutation.isPending) dialog.current?.focus(); }, [confirming, mutation.isPending]);
  async function download() {
    setDownloading(true);
    setDownloadError('');
    try {
      const doc = await api.getDoc(repoId, docHash);
      saveBlob(new Blob([JSON.stringify(doc.cir, null, 2)], {type: 'application/json'}), `${short(snapshotId)}-context.json`);
    } catch (error) { setDownloadError((error as Error).message); }
    finally { setDownloading(false); }
  }
  const action = t(archive ? 'sessionArchive.restore' : 'sessionArchive.archive');
  return <>
    <button type="button" className="dl-btn" title={t('context.rawTitle')} disabled={downloading} onClick={() => void download()}>↓ raw</button>
    {atLeast(role, 'maintainer') && <button ref={trigger} type="button" className="dl-btn" disabled={mutation.isPending}
      onClick={() => { mutation.reset(); setConfirming(true); }}>{action}</button>}
    {downloadError && <span role="alert" className="err">{downloadError}</span>}
    {confirming && createPortal(<div className="modal-back">
      <div ref={dialog} tabIndex={-1} className="modal" role="dialog" aria-modal="true" aria-label={action} onKeyDown={event => {
        if (event.key === 'Escape' && !mutation.isPending) { event.preventDefault(); setConfirming(false); }
        if (event.key === 'Tab') {
          const buttons = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('button:not(:disabled)'));
          const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
          event.preventDefault();
          if (!buttons.length) event.currentTarget.focus();
          else buttons[(current + (event.shiftKey ? buttons.length - 1 : 1)) % buttons.length]?.focus();
        }
      }}>
        <h3>{action}</h3>
        <p>{t(archive ? 'sessionArchive.restoreWarning' : 'sessionArchive.warning')}</p>
        {mutation.isError && <p role="alert" className="err">{mutation.error.message}</p>}
        {mutation.isPending && <p role="status">{t('sessionArchive.saving')}</p>}
        <div className="modal-actions">
          <button type="button" disabled={mutation.isPending} onClick={() => setConfirming(false)}>{t('common.cancel')}</button>
          <button type="button" disabled={mutation.isPending} onClick={() => mutation.mutate({
            repoId, snapshotId: archive?.snapshot_id ?? snapshotId, archived: !archive,
          }, {onSuccess: () => setConfirming(false)})}>{action}</button>
        </div>
      </div>
    </div>, document.body)}
  </>;
}

export function SessionArchiveDetails({ archive, snapshots, onSelect }: {
  archive: SessionArchiveView; snapshots: Snapshot[]; onSelect: (id: string) => void;
}) {
  const t = useT();
  const origin = archive.origin;
  const link = (id: string | undefined, label?: string) => id && snapshots.some(snapshot => snapshot.id === id)
    ? <button type="button" role="link" className="session-archive-link" title={id} onClick={() => onSelect(id)}>{label || short(id)}</button>
    : <code title={id}>{label || (id ? short(id) : t('sessionArchive.unknown'))}</code>;
  return <dl className="session-archive-details">
    <dt>{t('sessionArchive.parentSession')}</dt><dd>{origin.parent_session_id ? link(origin.parent_snapshot_id, `${origin.parent_provider || t('sessionArchive.unknown')} / ${origin.parent_session_id}`) : t('sessionArchive.unknown')}</dd>
    <dt>{t('sessionArchive.parentSnapshot')}</dt><dd>{link(origin.parent_snapshot_id)}</dd>
    <dt>{t('sessionArchive.mainSnapshot', {branch: origin.main_branch})}</dt><dd>{link(origin.main_snapshot_id)}</dd>
    <dt>{t('sessionArchive.mainCommit')}</dt><dd><code>{origin.main_git_commit || t('sessionArchive.unknown')}</code></dd>
  </dl>;
}

export function ArchivedSessionNotice({ archive, snapshots, onSelect }: {
  archive?: SessionArchiveView; snapshots: Snapshot[]; onSelect: (id: string) => void;
}) {
  const t = useT();
  if (!archive) return null;
  return <section className="session-archive-notice" aria-label={t('sessionArchive.archived')}>
    <strong>{t('sessionArchive.archived')} · {archive.provider} / {archive.session_id || t('sessionArchive.unknown')}</strong>
    <p>{t('sessionArchive.warning')}</p>
    <SessionArchiveDetails archive={archive} snapshots={snapshots} onSelect={onSelect} />
  </section>;
}

export function ArchivedSessions({ archives, snapshots, onSelect }: {
  archives: SessionArchiveView[]; snapshots: Snapshot[]; onSelect: (id: string) => void;
}) {
  const t = useT();
  return <details className="graph-archive-panel graph-session-archives">
    <summary>{t('sessionArchive.list', {count: archives.length})}</summary>
    <ul className="graph-archive-list">
      {archives.map(archive => <li key={archive.key}>
        <button type="button" className="graph-archive-entry" onClick={() => onSelect(archive.latest_snapshot_id)}
          disabled={!snapshots.some(snapshot => snapshot.id === archive.latest_snapshot_id)}
          aria-label={t('sessionArchive.open', {provider: archive.provider, session: archive.session_id || t('sessionArchive.unknown')})}>
          <span title={`${archive.provider} / ${archive.session_id}`}>{archive.provider} / {archive.session_id || t('sessionArchive.unknown')}</span>
          <code title={archive.latest_snapshot_id}>{short(archive.latest_snapshot_id)}</code>
          <em><time dateTime={archive.updated_at} title={archive.updated_at}>{when(archive.updated_at)}</time> · {archive.message || t('common.noMessage')}</em>
        </button>
        <SessionArchiveDetails archive={archive} snapshots={snapshots} onSelect={onSelect} />
      </li>)}
    </ul>
    {!archives.length && <p className="repository-empty">{t('sessionArchive.empty')}</p>}
  </details>;
}
