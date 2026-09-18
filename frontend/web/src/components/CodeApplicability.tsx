import {useState} from 'react';
import {useQuery} from '@tanstack/react-query';
import {api} from '../api';
import {useT} from '../i18n';
import type {CodeSelection} from '../types';

// Input, visibility and request state live here. Only the application query
// interprets ancestry and file applicability; graph folding is unrelated.
export function CodeApplicability({repoId}: {repoId: string}) {
 const t = useT();
 const [open, setOpen] = useState(false);
 const [selection, setSelection] = useState<CodeSelection>();
 const result = useQuery({queryKey: ['code-applicability', repoId, selection],
  queryFn: ({signal}) => api.codeApplicability(repoId, selection!, signal), enabled: open && !!selection, retry: false});
 const states = {applied: t('codeState.applied'), before: t('codeState.before'), changed: t('codeState.changed'), equivalent: t('codeState.equivalent'), not_in_history: t('codeState.notInHistory'), unknown: t('codeState.unknown')};
 const relations = {ancestor: t('codeState.ancestor'), not_ancestor: t('codeState.notAncestor'), unknown: t('codeState.ancestryUnknown')};
 const reason = (code: string) => code === 'comparison_parent_required' ? t('codeState.parentRequired') : code === 'path_not_changed_by_source' ? t('codeState.pathUnchanged') : t('codeState.pending');
 return <details className="code-applicability" open={open} onToggle={e => setOpen(e.currentTarget.open)}>
  <summary>{t('codeState.title')}</summary>
  {open && <>
   <p>{t('codeState.scope')}</p>
   <form onSubmit={e => {e.preventDefault(); const data = new FormData(e.currentTarget); setSelection({code_commit: String(data.get('code')), source_commit: String(data.get('source')), source_parent: String(data.get('parent') || ''), paths: [String(data.get('path'))]});}}>
    <label>{t('codeState.code')}<input name="code" required pattern="[0-9a-f]{40}|[0-9a-f]{64}" autoComplete="off" /></label>
    <label>{t('codeState.source')}<input name="source" required pattern="[0-9a-f]{40}|[0-9a-f]{64}" autoComplete="off" /></label>
    <label>{t('codeState.parent')}<input name="parent" pattern="[0-9a-f]{40}|[0-9a-f]{64}" autoComplete="off" /></label>
    <label>{t('codeState.path')}<input name="path" required maxLength={4096} autoComplete="off" /></label>
    <button type="submit">{t('codeState.check')}</button>
   </form>
   {selection && result.isPending && <p role="status">{t('codeState.loading')}</p>}
   {result.isError && <p role="alert">{t('codeState.failed')} <button onClick={() => void result.refetch()}>{t('context.retryRead')}</button></p>}
   {result.data && <div aria-live="polite">
    <p className="code-selection"><code>{result.data.selection.code_commit.slice(0, 10)}</code> ← <code>{result.data.selection.source_commit.slice(0, 10)}</code></p>
    <p>{relations[result.data.relation]}</p>
    {result.data.reason && <p>{reason(result.data.reason)}</p>}
    <ul>{result.data.paths.map(p => <li key={p.path}><code>{p.path}</code> · <strong>{states[p.state]}</strong>{p.reason && <p>{reason(p.reason)}</p>}</li>)}</ul>
   </div>}
  </>}
 </details>;
}
