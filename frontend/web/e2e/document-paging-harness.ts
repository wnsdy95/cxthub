import { createElement, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { DocEvents } from '../src/components/DocEvents';
import { OnHoldView } from '../src/components/OnHoldView';
import { CommitGraph } from '../src/components/CommitGraph';
import { I18nProvider } from '../src/i18n';
import type { DocViewMode } from '../src/types';
import { useRepoView } from '../src/hooks';
import '../src/styles.css';

const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
Object.assign(window, { documentQueryClient: client });
const params = new URLSearchParams(location.search);

function RepositoryHarness() {
  const view = useRepoView('repo');
  return createElement('main', null,
    createElement('pre', { 'data-testid': 'full-graph' }, JSON.stringify(view.graphState?.operations)),
    createElement('pre', { 'data-testid': 'pending-state' }, JSON.stringify(view.pendingRows.pendings)));
}

function GraphIdentityHarness() {
  const [published, setPublished] = useState(false);
  return createElement('main', null,
    createElement('button', { onClick: () => setPublished(true) }, 'Publish first branch'),
    createElement(CommitGraph, {
      readRepoId: 'repo', snapshots: [], selectedId: null, onSelect: () => undefined, badges: new Map(),
      refs: published ? [{ repo_id: 'repo', kind: 'branch', name: 'main', target: 'head' }] : [],
    }));
}

function Harness() {
  const [mode, setMode] = useState<DocViewMode>((params.get('mode') ?? 'chat') as DocViewMode);
  if (params.has('repository')) return createElement(RepositoryHarness);
  if (params.has('graphIdentity')) return createElement(GraphIdentityHarness);
  if (params.has('onhold')) return createElement(OnHoldView, { repo: { id: 'repo', remote_url: '', default_branch: 'main' }, role: null });
  return createElement('main', null,
    createElement('select', { 'aria-label': 'View mode', value: mode, onChange: (event: React.ChangeEvent<HTMLSelectElement>) => setMode(event.target.value as DocViewMode) },
      ...(['all', 'chat', 'prompts'] as const).map(value => createElement('option', { key: value, value }, value))),
    createElement('div', { 'data-testid': 'document-scroll', className: params.has('responsive') ? 'ctx-main' : undefined,
      style: params.has('responsive') ? undefined : { height: 500, width: '100%', maxWidth: 900, overflowY: 'auto' } },
      createElement(DocEvents, { repoId: 'repo', hash: 'document', base: 'base', start: Number(params.get('start') ?? -1), mode },
        params.has('tail') ? createElement('section', { 'data-testid': 'continuation' },
          createElement(DocEvents, { repoId: 'repo', hash: 'tail', base: 'document', mode })) : undefined)));
}

createRoot(document.getElementById('root')!).render(
  createElement(QueryClientProvider, { client }, createElement(I18nProvider, null, createElement(Harness))));
