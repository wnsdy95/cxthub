import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { CommitGraph } from '../src/components/CommitGraph';
import { I18nProvider } from '../src/i18n';
import { graphViewRows } from '../src/graphState';
import { projectBranchGraph, visibleBranchGraph } from '../src/graphProjection';
import { completedBranchEvidence } from '../src/graphEvidence';
import { mergePendingView, pendingViewNeedsFull } from '../src/repositoryUpdates';
import { sessionArchiveIndex } from '../src/sessionArchives';
import type { HistoryEvent, SessionArchiveView, Snapshot } from '../src/types';
import { serverGraphFixture } from './serverGraphFixture';

const at = (value: number) => `2026-10-01T0${value}:00:00Z`;
const snapshot = (id: string, parents: string[], session = id, message = id): Snapshot => ({
  id, parents, session_id: session, message, provider: 'codex', branch: 'main', repo_id: 'repo', doc_hash: id, fidelity: 'full', created_at: at(1),
});
const snapshots = [snapshot('pending', ['unpushed'], 'session', 'hook: pending'), snapshot('unpushed', ['shared'], 'session'),
  snapshot('shared', ['root'], 'session'), snapshot('root', []), snapshot('other-provider', [], 'session')];
snapshots[4].provider = 'claude';
const archive: SessionArchiveView = {
  repo_id: 'repo', key: 'session', provider: 'codex', session_id: 'session', snapshot_id: 'shared', latest_snapshot_id: 'pending',
  snapshot_ids: ['shared', 'unpushed', 'pending'], archived_at: at(2), archived_by: 'owner', message: 'hook: pending', branch: 'main',
  author: undefined, updated_at: at(2), origin: {main_branch: 'main'},
};
const view = serverGraphFixture({snapshots, refs: [{kind: 'branch', name: 'main', target: 'shared', repo_id: 'repo'}],
  pending: [{repo_id: 'repo', branch: 'main', session_id: 'session', provider: 'codex', target: 'pending', updated_at: at(2)}],
  unsync: [{repo_id: 'repo', branch: 'main', user: 'owner', target: 'unpushed', updated_at: at(2)}]});
const original = JSON.stringify(view);
const normal = graphViewRows(view);
const archived = graphViewRows({...view, archived_sessions: [archive]});
assert.deepEqual(archived.snapshots, normal.snapshots);
assert.equal(archived.archiveSnapshots, view.snapshots);
assert.deepEqual(archived.graphSnapshots, normal.graphSnapshots);
assert.deepEqual(archived.committedSnapshots.map(item => item.id).sort(), ['other-provider', 'root']);
assert.equal(archived.orphans.length, 0);
assert.equal(archived.chains.length, 0);
assert.equal(archived.pendings.length, 0);
assert.equal(archived.holdCount.get('main') ?? 0, 0);
assert.ok((normal.holdCount.get('main') ?? 0) > 0);
assert.equal(JSON.stringify(view), original);
assert.deepEqual(graphViewRows({...view, archived_sessions: []}).chains, normal.chains);
const index = sessionArchiveIndex([archive]);
assert.ok(!index.has('new-capture'));
assert.ok(sessionArchiveIndex([{...archive, snapshot_ids: [...archive.snapshot_ids, 'new-capture']}]).has('new-capture'));
assert.ok(!index.has('other-provider'));
assert.ok(!sessionArchiveIndex([{...archive, session_id: ''}]).has('unknown-b'));
const patch = {graph: view.graph, revision: view.revision!, pending: view.pending, snapshots: []};
assert.deepEqual(mergePendingView(view, {...patch, archived_sessions: [archive]}).archived_sessions, [archive]);
assert.deepEqual(mergePendingView({...view, archived_sessions: [archive]}, {...patch, archived_sessions: []}).archived_sessions, []);
assert.deepEqual(mergePendingView({...view, archived_sessions: [archive]}, patch).archived_sessions, []);

const detached = ['detached-first', 'detached-latest', 'detached-member', 'detached-parent', 'detached-main'].map(name => snapshot(name, []));
const detachedArchive = {...archive, snapshot_id: 'detached-first', latest_snapshot_id: 'detached-latest', snapshot_ids: ['detached-member'],
  origin: {main_branch: 'main', parent_snapshot_id: 'detached-parent', main_snapshot_id: 'detached-main'}};
const cached = {...view, snapshots: [...view.snapshots, ...detached], archived_sessions: [detachedArchive]};
const livePatch = {...patch, archived_sessions: [detachedArchive]};
assert.ok(detached.every(item => !view.graph.snapshot_ids.includes(item.id) && !view.graph.graph_ids.includes(item.id)));
assert.equal(pendingViewNeedsFull(cached, livePatch), false);
const refreshed = mergePendingView(cached, livePatch);
for (const item of detached) assert.ok(refreshed.snapshots.some(stored => stored.id === item.id));
assert.ok(graphViewRows(refreshed).archiveSnapshots.some(item => item.id === detachedArchive.latest_snapshot_id));
assert.deepEqual(refreshed.graph, view.graph);
for (const missing of detached) {
  const incomplete = {...cached, snapshots: cached.snapshots.filter(item => item.id !== missing.id)};
  assert.equal(pendingViewNeedsFull(incomplete, livePatch), true);
  assert.equal(pendingViewNeedsFull(incomplete, {...livePatch, snapshots: [missing]}), false);
}
const latestUnknown = {...livePatch, archived_sessions: [{...detachedArchive, latest_snapshot_id: 'new-archive-latest'}]};
assert.equal(pendingViewNeedsFull(cached, latestUnknown), true);
assert.equal(pendingViewNeedsFull(cached, {...latestUnknown, snapshots: [snapshot('new-archive-latest', [])]}), false);

const branchSnapshots = [snapshot('root', []), {...snapshot('source', ['root'], 'session'), branch: 'topic', created_at: at(3)},
  {...snapshot('current', ['source']), created_at: at(5)}];
const birth: HistoryEvent = {repo_id: 'repo', id: 'birth', kind: 'birth', branch: 'topic', branch_id: 'topic-id', source: 'root', target: 'root', created_at: at(2)};
const merge: HistoryEvent = {...birth, id: 'merge', kind: 'pr-merge', branch: 'main', branch_id: 'main-id', source_branch_id: 'topic-id',
  source: 'source', target: 'source', shared_target: 'root', created_at: at(4), pr_completed: true,
  pr: {number: 42, head_branch: 'topic', base_branch: 'main', head_sha: 'a'.repeat(40), merge_sha: 'b'.repeat(40)}};
const graphView = serverGraphFixture({snapshots: branchSnapshots, history: [birth, merge], refs: [{kind: 'branch', name: 'main', branch_id: 'main-id', target: 'current', repo_id: 'repo'}]});
const sourceArchive = {...archive, snapshot_id: 'source', latest_snapshot_id: 'source', snapshot_ids: ['source']};
const full = projectBranchGraph(graphView.snapshots, graphView.refs, graphView.graph, 'current', 'main');
const visible = visibleBranchGraph(full, new Set(['current', 'root']));
assert.ok(!visible.snapshots.some(item => item.id === 'source'));
assert.equal(visible.events, full.events);
assert.ok(visible.events.has('graph:merge:merge'));
assert.deepEqual(visible.snapshots.find(item => item.id === 'graph:merge:merge')?.parents, ['root', 'source']);
assert.ok(visible.foldedParents.has('source'));
assert.equal(completedBranchEvidence(graphView.snapshots, graphView.history, undefined, graphView.semantics)[0].sourceAvailable, true);
const markup = renderToStaticMarkup(createElement(QueryClientProvider, {client: new QueryClient()},
  createElement(I18nProvider, null, createElement(CommitGraph, {
    snapshots: graphView.snapshots, archivedSessions: [sourceArchive], selectedId: 'source', onSelect: () => undefined,
    badges: new Map(), refs: graphView.refs, graphState: graphView.graph, history: graphView.history, semantics: graphView.semantics,
    pinBranch: 'main',
  }))));
assert.doesNotMatch(markup, /data-graph-id="source"/);
assert.match(markup, /data-graph-id="graph:merge:merge"/);
assert.match(markup, /Pushed 2/);
assert.match(markup, /PR #42/);
assert.match(markup, /data-branch-lineage="natural"/);
assert.ok(markup.indexOf('graph-session-archives') > markup.indexOf('graph-archive-panel'));
