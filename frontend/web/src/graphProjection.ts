import type { HistoryEvent, Ref, RefLogEntry, Snapshot } from './types';
import { parseBranchLifecycleRef } from './branchLifecycle';
import { reachableSnapshotIds } from './onhold';

export interface GraphEvent {
  kind: 'birth' | 'merge';
  branch: string;
  sourceBranch?: string;
  snapshot: string;
  orphan?: boolean;
  evidence: string;
}

/** Graph event rows are a view projection, never stored snapshots. A branch
 * ref move plus verified ancestry proves a join; a pending PR receipt does not.
 * Natural parents and the original source branch remain in their own lane. */
export function projectBranchGraph(snapshots: Snapshot[], refs: Ref[], history: HistoryEvent[], reflog: RefLogEntry[], pinHead?: string | null, pinBranch?: string) {
  const time = (value: string) => Date.parse(value);
  const byId = new Map(snapshots.map(s => [s.id, s]));
  const nodes = new Map(snapshots.map(s => [s.id, { ...s, parents: [...(s.parents ?? [])], graft_parents: [...(s.graft_parents ?? [])] }]));
  const events = new Map<string, GraphEvent>();
  const births = new Map<string, HistoryEvent>();
  const branchTips = new Map<string, Set<string>>();
  const addTip = (id: string, name: string) => { const names = branchTips.get(id) ?? new Set<string>(); names.add(name); branchTips.set(id, names); };
  for (const ref of refs) {
    const lifecycle = parseBranchLifecycleRef(ref);
    if (lifecycle) addTip(ref.target, lifecycle.branch);
    else if (ref.kind === 'branch') addTip(ref.target, ref.name);
  }
  for (const h of history) {
    if (h.kind === 'birth' && h.source && h.source === h.target && byId.has(h.source)) births.set(h.branch_id, h);
    if (h.kind !== 'pr-merge' && h.target) addTip(h.target, h.branch);
  }
  let projectedHead = pinHead;
  const merges: Array<{ id: string; before: string; source: string; branch: string; from: string; at: string; identity?: string }> = [];
  // Chronological ref order is evidence order. Reject ambiguous names, backward
  // moves and missing endpoints instead of inferring them from snapshot.branch.
  for (const move of reflog) {
    if (move.kind !== 'branch' || !move.old || !move.new || move.old === move.new || !byId.has(move.old) || !byId.has(move.new)) continue;
    if (!reachableSnapshotIds([move.new], snapshots).has(move.old)) continue;
    const receipts = history.filter(h => h.kind === 'pr-merge' && h.branch === move.name && h.source === move.new && h.shared_target === move.old && h.pr && time(h.created_at) <= time(move.created_at));
    const candidates = [...(branchTips.get(move.new) ?? [])].filter(name => name !== move.name);
    const incoming = reachableSnapshotIds([move.new], snapshots);
    const previous = reachableSnapshotIds([move.old], snapshots);
    const hasAppendEdge = [...incoming].some(id => !previous.has(id) && byId.get(id)?.graft_parents?.includes(move.old));
    // A current shared hash can also be a later fork point. Without a bound PR
    // receipt, only a stored append edge proves this was a join.
    if (receipts.length !== 1 && !hasAppendEdge) continue;
    const from = receipts.length === 1 ? receipts[0].pr!.head_branch : candidates.length === 1 ? candidates[0] : undefined;
    if (!from) continue;
    merges.push({ id: `graph:merge:${move.created_at}:${move.name}:${move.old}:${move.new}`, before: move.old, source: move.new, branch: move.name, from, at: move.created_at, identity: receipts[0]?.source_branch_id });
  }
  merges.sort((a,b) => time(a.at) - time(b.at) || a.id.localeCompare(b.id));
  const mergeForTip = new Map<string, string>();
  for (const merge of merges) {
    if (events.has(merge.id)) continue;
    const source = byId.get(merge.source)!;
    const previous = mergeForTip.get(`${merge.branch}:${merge.before}`) ?? merge.before;
    const node: Snapshot = { ...source, id: merge.id, branch: merge.branch, parents: [previous, merge.source], graft_parents: [], grafted: false,
      created_at: merge.at, message: `${merge.from} → ${merge.branch}`, memory_hash: undefined, session_id: undefined };
    nodes.set(merge.id, { ...node, parents: node.parents ?? [], graft_parents: [] });
    events.set(merge.id, { kind: 'merge', branch: merge.branch, sourceBranch: merge.from, snapshot: merge.source, evidence: 'ref-move' });
    mergeForTip.set(`${merge.branch}:${merge.source}`, merge.id);
    // The verified ref move already represents this append edge. Drawing its
    // storage graft too would invert main and feature paths a second time.
    const segment = reachableSnapshotIds([merge.source], snapshots);
    const previousIDs = reachableSnapshotIds([merge.before], snapshots);
    for (const id of segment) {
      if (previousIDs.has(id)) continue;
      const n = nodes.get(id);
      if (n) n.graft_parents = n.graft_parents.filter(p => p !== merge.before);
    }
    for (const n of nodes.values()) {
      const membership = history.some(h => h.kind === 'advance' && h.branch === merge.branch && h.target === n.id);
      if (n.id === merge.id || events.has(n.id) || segment.has(n.id) || (!membership && n.branch !== merge.branch)) continue;
      n.parents = n.parents.map(p => p === merge.source ? merge.id : p);
    }
    if (pinBranch === merge.branch && pinHead === merge.source) projectedHead = merge.id;
  }
  for (const birth of births.values()) {
    const source = byId.get(birth.source!)!;
    const id = `graph:birth:${birth.id}`;
    nodes.set(id, { ...source, id, branch: birth.branch, parents: [source.id], graft_parents: [], grafted: false, message: birth.branch,
      created_at: birth.created_at, memory_hash: undefined, session_id: undefined });
    events.set(id, { kind: 'birth', branch: birth.branch, snapshot: source.id, evidence: birth.id });
    const own = new Set<string>();
    const stack = history.filter(h => h.branch_id === birth.branch_id && h.kind === 'advance').flatMap(h => h.target ? [h.target] : []);
    while (stack.length) {
      const target = stack.pop()!;
      if (target === source.id || own.has(target)) continue;
      own.add(target);
      stack.push(...(byId.get(target)?.parents ?? []));
    }
    for (const n of nodes.values()) {
      if (n.id === id || events.has(n.id)) continue;
      if (own.has(n.id) && n.id !== source.id) n.parents = n.parents.map(p => p === source.id ? id : p);
    }
    for (const merge of merges) {
      if (merge.source === source.id && (merge.identity === birth.branch_id || (!merge.identity && merge.from === birth.branch && [...births.values()].filter(b => b.branch === birth.branch && time(b.created_at) <= time(merge.at)).length === 1)) && time(merge.at) >= time(birth.created_at)) {
        const n = nodes.get(merge.id)!;
        n.parents = [n.parents[0], id];
      }
    }
  }
  for (const birth of history.filter(h => h.kind === 'orphan')) {
    const own = history.filter(h => h.branch_id === birth.branch_id && h.kind === 'advance').flatMap(h => h.target && byId.has(h.target) ? [h.target] : []);
    const template = own.map(id => byId.get(id)!).find(s => !(s.parents?.length));
    if (!template) continue; // an unborn branch with no capture stays in the operations list
    const id = `graph:birth:${birth.id}`;
    nodes.set(id, { ...template, id, branch: birth.branch, parents: [], graft_parents: [], grafted: false,
      created_at: birth.created_at, message: birth.branch, memory_hash: undefined, session_id: undefined });
    nodes.get(template.id)!.parents = [id];
    events.set(id, { kind: 'birth', branch: birth.branch, snapshot: template.id, orphan: true, evidence: birth.id });
  }
  const projectedRefs = refs.map(ref => ref.kind === 'branch' && mergeForTip.has(`${ref.name}:${ref.target}`)
    ? { ...ref, target: mergeForTip.get(`${ref.name}:${ref.target}`)! } : ref);
  return { snapshots: [...nodes.values()], events, pinHead: projectedHead, refs: projectedRefs };
}
