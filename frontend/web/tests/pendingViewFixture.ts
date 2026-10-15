import type { GraphState, PendingStateView, PendingView, RepositoryRevision, RepositoryView } from '../src/types';
import { graphIndexFields } from '../src/graphWire';

export function pendingFixture(target = 'pending-document', revision: RepositoryRevision = { graph: '1', pending: '2', evidence: '0' }): PendingView {
  const graph: GraphState = {
    version: 1, revision, primary_branch: 'main', snapshot_ids: [target], graph_ids: [target],
    committed_ids: [], historical_ids: [], shared_ids: [], pushed_ids: [], unpushed_ids: [], uncommitted_ids: [target],
    tagged_ids: [], archived_only_ids: [], ahead_ids: [], ahead_tips: [], markers: [],
    branch_heads: {}, ref_scopes: {}, snapshot_scopes: {}, scope_labels: {}, hold: [],
    orphan_sessions: ['live-session'], hold_counts: { main: 1 }, positions: [], previous: [],
    branch_snapshots: {}, continuations: {}, operations: { births: [], merges: [] },
  };
  return {
    graph, revision,
    pending: [{ repo_id: 'repo', session_id: 'live-session', target, branch: 'main', provider: 'codex', updated_at: '2026-10-14T00:00:00Z' }],
    snapshots: [{ id: target, doc_hash: target, repo_id: 'repo', branch: 'main', provider: 'codex', fidelity: 'full', parents: [], message: 'Live session', created_at: '2026-10-14T00:00:00Z' }],
  };
}

export function fullFixture(pending = pendingFixture()): RepositoryView {
  return { ...pending, default_branch: 'main', refs: [], reflog: [], history: [], unsync: [] };
}

export function pendingStateFixture(pending = pendingFixture()): PendingStateView {
  return {
    projection: 'on-hold-v1', revision: pending.revision, default_branch: 'main', refs: [],
    pending: pending.pending, snapshots: pending.snapshots, archived_sessions: pending.archived_sessions ?? [],
    hold: pending.graph.hold, orphan_sessions: pending.graph.orphan_sessions, hold_counts: pending.graph.hold_counts,
  };
}

export function wireFixture(view: PendingView | RepositoryView) {
  const dictionary = view.graph.snapshot_ids;
  return { ...view, graph: {
    ...view.graph, encoding: 'indexed-v2', dictionary, branch_contexts: {},
    ...Object.fromEntries(graphIndexFields.map(field => [field, view.graph[field].map(id => dictionary.indexOf(id))])),
  } };
}
