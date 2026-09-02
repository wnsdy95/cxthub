import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { CommitGraph } from '../src/components/CommitGraph.tsx';
import { I18nProvider } from '../src/i18n/index.tsx';
import type { Ref, Snapshot } from '../src/types.ts';

const childId = `sha256:${'a'.repeat(64)}`;
const parentId = `sha256:${'b'.repeat(64)}`;
const snapshots: Snapshot[] = [
  {
    id: childId,
    repo_id: 'repo',
    branch: 'main',
    parents: [],
    graft_parents: [parentId],
    grafted: true,
    doc_hash: childId,
    provider: 'codex',
    fidelity: 'full',
    message: 'appended session root',
    created_at: '2026-08-31T02:00:00Z',
  },
  {
    id: parentId,
    repo_id: 'repo',
    branch: 'main',
    parents: [],
    doc_hash: parentId,
    provider: 'codex',
    fidelity: 'full',
    message: 'previous main head',
    created_at: '2026-08-31T01:00:00Z',
  },
];
const refs: Ref[] = [{ kind: 'branch', name: 'main', repo_id: 'repo', target: childId }];
const client = new QueryClient();
const markup = renderToStaticMarkup(
  createElement(
    QueryClientProvider,
    { client },
    createElement(
      I18nProvider,
      null,
      createElement(CommitGraph, {
        snapshots,
        selectedId: null,
        onSelect: () => undefined,
        badges: new Map(),
        refs,
        uncommitted: new Set(),
        pinBranch: 'main',
      }),
    ),
  ),
);

assert.match(markup, /id="seam-out-aaaaaaaaaa"[^>]*gradientUnits="userSpaceOnUse"/);
assert.match(markup, /id="seam-in-bbbbbbbbbb"[^>]*gradientUnits="userSpaceOnUse"/);
assert.match(markup, /stroke="url\(#seam-out-aaaaaaaaaa\)"/);
assert.match(markup, /stroke="url\(#seam-in-bbbbbbbbbb\)"/);
assert.match(markup, /class="graph-viewport"/);
assert.match(markup, /class="graph-canvas"/);
assert.match(markup, /data-graph-lane="0"/);
assert.match(markup, /data-graph-row-index="0"/);
