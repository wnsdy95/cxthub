import { useEffect, useRef, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { SyncAuditCheck, SyncAuditPage } from '../types';

const codes = ['origin_identity_mismatch','github_receipt_present', 'github_pr_not_recorded','command_recorded', 'command_unavailable', 'missing_parent', 'invalid_history', 'birth_source_missing', 'birth_projection_mismatch', 'merge_objects_missing', 'merge_projection_missing', 'graph_invalid', 'graph_structure_valid', 'github_unavailable', 'github_pr_mismatch', 'github_pr_matches', 'github_commit_mismatch', 'github_commit_matches', 'code_position_missing', 'github_lookup_failed', 'included_context_missing', 'included_merge_path_missing'] as const;

export function GitHubSyncCheck({ repo }: { repo: string }) {
 const t = useT();
 const controller = useRef<AbortController | null>(null);
 const [report, setReport] = useState<SyncAuditPage | null>(null);
 const [issuesOnly, setIssuesOnly] = useState(true);
 useEffect(() => () => controller.current?.abort(), [repo]);
 const run = useMutation({ mutationFn: async () => {
  controller.current?.abort();
  const active = new AbortController(); controller.current = active; setReport(null);
  let cursor = '', checks: SyncAuditCheck[] = [];
  const seen = new Set<string>(); let revision = '';
  do {
   const page = await api.checkGitHubSync(repo, cursor, active.signal);
   if (active.signal.aborted) throw new DOMException('Aborted', 'AbortError');
   if (revision && revision !== page.revision) throw new Error(t('syncAudit.changed'));
   revision = page.revision;
   checks = [...checks, ...page.checks];
   setReport({ ...page, checks });
   cursor = page.next_cursor ?? '';
   if (cursor && seen.has(cursor)) throw new Error(t('syncAudit.failed'));
   seen.add(cursor);
  } while (cursor);
 }});
 const checks = report?.checks ?? [];
 const visible = issuesOnly ? checks.filter(c => c.state !== 'verified') : checks;
 const counts = { verified: 0, mismatch: 0, incomplete: 0, unavailable: 0 };
 for (const c of checks) counts[c.state]++;
 return <section className="settings-upload sync-audit" aria-label={t('syncAudit.title')}>
  <h3>{t('syncAudit.title')}</h3>
  <p className="hint">{t('syncAudit.hint')}</p>
  <p className="hint">{t('syncAudit.provenanceHint')}</p>
  <button type="button" className="ghost" disabled={run.isPending} onClick={() => run.mutate()}>{run.isPending ? t('syncAudit.running') : t('syncAudit.run')}</button>
  {run.isPending && <button type="button" className="ghost" onClick={() => controller.current?.abort()}>{t('syncAudit.stop')}</button>}
  {run.isError && <p role="alert" className="err">{t('syncAudit.failed')} {run.error.message}</p>}
  {report && <>
   <p role="status">{run.isSuccess ? t('syncAudit.finished') : t('syncAudit.partial')} · {report.phase === 'github' ? t('syncAudit.remoteScan') : `${report.processed}/${report.total}`} </p>
   <p>{t('syncAudit.verified')}: {counts.verified} · {t('syncAudit.mismatch')}: {counts.mismatch} · {t('syncAudit.incomplete')}: {counts.incomplete} · {t('syncAudit.unavailable')}: {counts.unavailable}</p>
   <small>{new Date(report.checked_at).toLocaleString()} · {report.revision.slice(0, 12)}</small>
   <label><input type="checkbox" checked={issuesOnly} onChange={e => setIssuesOnly(e.target.checked)} />{t('syncAudit.issuesOnly')}</label>
   <div className="sync-audit-results">{visible.map(c => <article key={c.id} className={`sync-audit-entry ${c.state}`}>
    <strong>{t(`syncAudit.${c.state}`)} · {c.branch}</strong>
    <p>{codes.includes(c.code as typeof codes[number]) ? t(`syncAudit.${c.code as typeof codes[number]}`) : c.code}</p>
    {c.snapshot && <code>{c.snapshot}</code>}
    {c.creation?.command && <p><code>{c.creation.command.join(' ')}</code></p>}
    {c.creation?.origin_branch && <p>{t('syncAudit.origin')}: {c.creation.origin_branch} · <code>{c.creation.origin_branch_id}</code></p>}
    {c.expected && <p>{t('syncAudit.expected')}: <code>{c.expected}</code></p>}
    {c.actual && <p>{t('syncAudit.actual')}: <code>{c.actual}</code></p>}
    {c.event_id && <small>{t('syncAudit.event')}: {c.event_id}</small>}
   </article>)}</div>
  </>}
 </section>;
}
