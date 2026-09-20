import {useState} from 'react';
import {useInfiniteQuery, useQuery, useQueryClient} from '@tanstack/react-query';
import {api} from '../api';
import {useT} from '../i18n';
import type {EffectiveMemorySelection} from '../types';

// The server resolves recorded code positions; the client owns only disclosure
// and explicit comparison choices. Never substitute the current shared main.
export function EffectiveMemory({repoId, snapshotId, memoryHash, eventId}: {repoId: string; snapshotId: string; memoryHash?: string; eventId?: string}) {
 const t = useT();
 const qc = useQueryClient();
 const [open, setOpen] = useState(true);
 const [override, setOverride] = useState<string>();
 const [stored, setStored] = useState(true);
 const positions = useQuery({queryKey: ['memory-positions', repoId, snapshotId, eventId],
  queryFn: ({signal}) => api.memoryPositions(repoId, snapshotId, eventId, signal), enabled: open, retry: false});
 const code = override ?? positions.data?.code_commit;
 const selection: EffectiveMemorySelection | undefined = code ? {snapshot_id: snapshotId, code_commit: code,
  ...(stored && memoryHash ? {memory_hash: memoryHash} : {})} : undefined;
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
   {positions.isPending && !code && <p role="status">{t('effectiveMemory.resolving')}</p>}
   {positions.isError && <p role="alert">{t('effectiveMemory.positionsFailed')} <button onClick={() => void positions.refetch()}>{t('context.retryRead')}</button></p>}
   {!code && positions.data && <p role="status">{t(positions.data.reason === 'ambiguous' ? 'effectiveMemory.ambiguous' : 'effectiveMemory.unavailable')}</p>}
   <details className="effective-memory-options" open={!code && !!positions.data?.options.length ? true : undefined}>
    <summary>{t('effectiveMemory.changePosition')}</summary>
    {!!positions.data?.options.length && <label>{t('effectiveMemory.position')}
     <select value={code ?? ''} onChange={e => setOverride(e.target.value || undefined)}>
      <option value="">{t('effectiveMemory.automatic')}</option>
      {override && !positions.data.options.some(p => p.code_commit === override) && <option value={override}>{override.slice(0, 10)}</option>}
      {positions.data.options.map(p => <option key={p.event_id} value={p.code_commit}>
       {p.branch}{p.pr_number ? ` · PR #${p.pr_number}` : ''} · {p.code_commit.slice(0, 10)}
      </option>)}
     </select>
    </label>}
    {memoryHash && <label className="effective-memory-pin"><input type="checkbox" checked={stored} onChange={e => setStored(e.target.checked)} />{t('effectiveMemory.pin')}</label>}
    <details className="effective-memory-advanced">
     <summary>{t('effectiveMemory.advanced')}</summary>
     <form onSubmit={e => {e.preventDefault(); setOverride(String(new FormData(e.currentTarget).get('code')).trim());}}>
      <label>{t('codeState.code')}<input name="code" required pattern="[0-9a-f]{40}|[0-9a-f]{64}" autoComplete="off" /></label>
      <button type="submit">{t('effectiveMemory.check')}</button>
     </form>
    </details>
   </details>
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
