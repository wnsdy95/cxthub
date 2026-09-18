import {useState} from 'react';
import {useInfiniteQuery, useQueryClient} from '@tanstack/react-query';
import {api} from '../api';
import {useT} from '../i18n';
import type {EffectiveMemorySelection} from '../types';

// The client owns form and disclosure state. All applicability decisions and
// continuation generations come from the shared backend query.
export function EffectiveMemory({repoId, snapshotId, memoryHash}: {repoId: string; snapshotId: string; memoryHash?: string}) {
 const t = useT();
 const qc = useQueryClient();
 const [open, setOpen] = useState(false);
 const [selection, setSelection] = useState<EffectiveMemorySelection>();
 const queryKey = ['effective-memory', repoId, selection];
 const result = useInfiniteQuery({queryKey, initialPageParam: '',
  queryFn: ({pageParam, signal}) => api.effectiveMemory(repoId, selection!, pageParam, signal),
  getNextPageParam: page => page.next_cursor || undefined,
  enabled: open && !!selection, retry: false});
 const states = {applied: t('effectiveMemory.applied'), inactive: t('effectiveMemory.inactive'), retained: t('effectiveMemory.retained'), review: t('effectiveMemory.review')};
 const reasons: Record<string, string> = {
  declared_scope_matches: t('effectiveMemory.matches'), declared_scope_before: t('effectiveMemory.before'),
  source_not_selected: t('effectiveMemory.notSelected'), scope_changed: t('effectiveMemory.changed'),
  equivalent_without_integration: t('effectiveMemory.equivalent'), untyped_historical_text: t('effectiveMemory.legacy'),
  historical_knowledge: t('effectiveMemory.historical'), source_publication_missing: t('effectiveMemory.publicationMissing'),
  declared_scope_invalid: t('effectiveMemory.scopeInvalid'), evidence_budget_exhausted: t('effectiveMemory.bounded'),
 };
 const restart = () => void qc.resetQueries({queryKey, exact: true});
 return <details className="effective-memory" open={open} onToggle={e => setOpen(e.currentTarget.open)}>
  <summary className="label">{t('effectiveMemory.title')}</summary>
  {open && <>
   <p className="memory-note">{t('effectiveMemory.scope')}</p>
   <form onSubmit={e => {e.preventDefault(); const data = new FormData(e.currentTarget); const next = {snapshot_id: snapshotId, code_commit: String(data.get('code')), ...(data.get('stored') && memoryHash ? {memory_hash: memoryHash} : {})}; if (JSON.stringify(next) === JSON.stringify(selection)) restart(); else setSelection(next);}}>
    <label>{t('codeState.code')}<input name="code" required pattern="[0-9a-f]{40}|[0-9a-f]{64}" autoComplete="off" /></label>
    {memoryHash && <label className="effective-memory-pin"><input type="checkbox" name="stored" />{t('effectiveMemory.pin')}</label>}
    <button type="submit">{t('effectiveMemory.check')}</button>
   </form>
   {selection && result.isPending && <p role="status">{t('codeState.loading')}</p>}
   {result.isError && <p role="alert">{t('effectiveMemory.failed')} <button onClick={restart}>{t('effectiveMemory.restart')}</button></p>}
   {!result.isError && result.data && <div aria-live="polite">
    <p>{t('effectiveMemory.atCode')} <code>{result.data.pages[0].selection.code_commit.slice(0, 10)}</code></p>
    {result.data.pages[0].total === 0 && <p>{t('effectiveMemory.empty')}</p>}
    <ul className="effective-memory-items">{result.data.pages.flatMap(page => page.items).map(item => <li key={item.id} data-memory-state={item.state}>
     <strong>{states[item.state]}</strong> · <code>{item.source_snapshot.replace('sha256:', '').slice(0, 10)}</code>
     <p className="memory-note">{reasons[item.reason] ?? t('effectiveMemory.pending')}</p>
     <p className="effective-memory-text">{item.text}</p>
     {item.code && <small><code>{item.code.commit.slice(0, 10)}</code> · {item.code.paths.join(', ')}</small>}
    </li>)}</ul>
    {result.hasNextPage && <button disabled={result.isFetching} onClick={() => void result.fetchNextPage()}>{t('effectiveMemory.more')}</button>}
   </div>}
  </>}
 </details>;
}
