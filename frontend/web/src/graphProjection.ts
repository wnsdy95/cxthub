import type { HistoryEvent, Ref, RefLogEntry, Snapshot } from './types';
import { parseBranchLifecycleRef } from './branchLifecycle';
import { reachableSnapshotIds } from './onhold';
import { conversationParents } from './graphEvidence';

export interface GraphEvent {
  id: string;
  kind: 'birth' | 'merge';
  branch: string;
  sourceBranch?: string;
  snapshot: string;
  orphan?: boolean;
  evidence: string;
  prNumber?: number;
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
  const merges: Array<{ id: string; before: string; after: string; source: string; branch: string; from: string; at: string; identity?: string }> = [];
  const completed = history.filter(h => h.kind === 'pr-merge' && h.pr_completed && h.pr && h.source && h.target && h.shared_target
    && byId.has(h.source) && byId.has(h.target) && byId.has(h.shared_target)
    && reachableSnapshotIds([h.target], snapshots).has(h.source)
    && reachableSnapshotIds([h.target], snapshots).has(h.shared_target));
  // A normal capture need not move a rewound ref, so it may never emit advance.
  // Publication and verified completion attest the exact source identity even
  // after its ref is archived. A worktree position is only a selection and must
  // not turn somebody else's history into this branch's work.
  const branchRoots = new Map<string, Set<string>>();
  const addRoot = (identity: string | undefined, target: string | undefined) => {
    if (!identity || !target || !byId.has(target)) return;
    const roots = branchRoots.get(identity) ?? new Set<string>();
    roots.add(target);
    branchRoots.set(identity, roots);
  };
  for (const h of history) {
    if (h.kind === 'advance' || h.kind === 'publish') addRoot(h.branch_id, h.target);
  }
  for (const h of completed) addRoot(h.source_branch_id, h.source);
  // Content-addressed captures may be shared by multiple identities. Gather
  // claims against the original edges first; input order must not pick an owner.
  const birthClaims = new Map<string, Map<string, Set<string>>>();
  const claimBirth = (child: string, parent: string, birth: string) => {
    const parents = birthClaims.get(child) ?? new Map<string, Set<string>>();
    const claims = parents.get(parent) ?? new Set<string>();
    claims.add(birth);
    parents.set(parent, claims);
    birthClaims.set(child, parents);
  };
  for (const h of completed) {
    merges.push({id: `graph:merge:${h.id}`, before: h.shared_target!, after: h.target!, source: h.source!, branch: h.branch,
      from: h.pr!.head_branch, at: h.created_at, identity: h.source_branch_id});
  }
  // Chronological ref order is evidence order. Reject ambiguous names, backward
  // moves and missing endpoints instead of inferring them from snapshot.branch.
  for (const move of reflog) {
    if (move.kind !== 'branch' || !move.old || !move.new || move.old === move.new || !byId.has(move.old) || !byId.has(move.new)) continue;
    if (!reachableSnapshotIds([move.new], snapshots).has(move.old)) continue;
    const receipts = history.filter(h => h.kind === 'pr-merge' && !h.pr_completed && h.branch === move.name && h.source === move.new && h.shared_target === move.old && h.pr && time(h.created_at) <= time(move.created_at));
    if (completed.some(h => h.branch === move.name && h.shared_target === move.old && h.target === move.new)) continue;
    const candidates = [...(branchTips.get(move.new) ?? [])].filter(name => name !== move.name);
    const incoming = reachableSnapshotIds([move.new], snapshots);
    const previous = reachableSnapshotIds([move.old], snapshots);
    const hasAppendEdge = [...incoming].some(id => !previous.has(id) && byId.get(id)?.graft_parents?.includes(move.old));
    // A current shared hash can also be a later fork point. Without a bound PR
    // receipt, only a stored append edge proves this was a join.
    if (receipts.length !== 1 && !hasAppendEdge) continue;
    const from = receipts.length === 1 ? receipts[0].pr!.head_branch : candidates.length === 1 ? candidates[0] : undefined;
    if (!from) continue;
    merges.push({ id: `graph:merge:${move.created_at}:${move.name}:${move.old}:${move.new}`, before: move.old, after: move.new, source: move.new, branch: move.name, from, at: move.created_at, identity: receipts[0]?.source_branch_id });
  }
  merges.sort((a,b) => time(a.at) - time(b.at) || a.id.localeCompare(b.id));
  const mergeForTip = new Map<string, string>();
  for (const merge of merges) {
    if (events.has(merge.id)) continue;
    const source = byId.get(merge.source)!;
    const previous = mergeForTip.get(`${merge.branch}:${merge.before}`) ?? merge.before;
    const replacedTip = mergeForTip.get(`${merge.branch}:${merge.after}`);
    const node: Snapshot = { ...source, id: merge.id, branch: merge.branch, parents: [previous, merge.source], graft_parents: [], grafted: false,
      created_at: merge.at, message: `${merge.from} → ${merge.branch}`, memory_hash: undefined, session_id: undefined };
    nodes.set(merge.id, { ...node, parents: node.parents ?? [], graft_parents: [] });
    const receipt = completed.find(h => `graph:merge:${h.id}` === merge.id);
    events.set(merge.id, { id: merge.id, kind: 'merge', branch: merge.branch, sourceBranch: merge.from, snapshot: merge.source, evidence: receipt?.id ?? 'ref-move', prNumber: receipt?.pr?.number });
    mergeForTip.set(`${merge.branch}:${merge.after}`, merge.id);
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
      // Retained progress from before a later completion must not move behind
      // it when the destination has since rewound to this same stored tip.
      if (!(time(n.created_at) >= time(merge.at))) continue;
      // Several completed PRs can leave the stored ref unchanged. Children
      // already redirected to its earlier projection must follow the latest
      // operation, or later joins become detached tips beside main.
      n.parents = n.parents.map(p => p === merge.after || p === replacedTip ? merge.id : p);
    }
    if (pinBranch === merge.branch && pinHead === merge.after) projectedHead = merge.id;
  }
  for (const birth of births.values()) {
    const source = byId.get(birth.source!)!;
    const id = `graph:birth:${birth.id}`;
    nodes.set(id, { ...source, id, branch: birth.branch, parents: [source.id], graft_parents: [], grafted: false, message: birth.branch,
      created_at: birth.created_at, memory_hash: undefined, session_id: undefined });
    events.set(id, { id, kind: 'birth', branch: birth.branch, snapshot: source.id, evidence: birth.id });
    const own = new Set<string>();
    const stack = [...(branchRoots.get(birth.branch_id) ?? [])];
    while (stack.length) {
      const target = stack.pop()!;
      if (target === source.id || own.has(target)) continue;
      own.add(target);
      const snapshot = byId.get(target);
      if (snapshot) stack.push(...conversationParents(snapshot));
    }
    for (const target of own) {
      const snapshot = byId.get(target);
      if (snapshot && conversationParents(snapshot).includes(source.id)) claimBirth(target, source.id, id);
    }
    for (const merge of merges) {
      if (merge.source === source.id && (merge.identity === birth.branch_id || (!merge.identity && merge.from === birth.branch && [...births.values()].filter(b => b.branch === birth.branch && time(b.created_at) <= time(merge.at)).length === 1)) && time(merge.at) >= time(birth.created_at)) {
        const n = nodes.get(merge.id)!;
        n.parents = [n.parents[0], id];
      }
    }
  }
  for (const birth of history.filter(h => h.kind === 'orphan')) {
    // Only a directly evidenced root can start this orphan. A later publication
    // may follow an explicitly selected old path; its ancestors are not births.
    const roots = [...(branchRoots.get(birth.branch_id) ?? [])]
      .map(id => byId.get(id)!).filter(s => !(s.parents?.length));
    const template = roots[0];
    if (!template) continue; // an unborn branch with no capture stays in the operations list
    const id = `graph:birth:${birth.id}`;
    nodes.set(id, { ...template, id, branch: birth.branch, parents: [], graft_parents: [], grafted: false,
      created_at: birth.created_at, message: birth.branch, memory_hash: undefined, session_id: undefined });
    for (const root of roots) claimBirth(root.id, '', id);
    events.set(id, { id, kind: 'birth', branch: birth.branch, snapshot: template.id, orphan: true, evidence: birth.id });
  }
  for (const [child, parents] of birthClaims) {
    const node = nodes.get(child)!;
    for (const [parent, claims] of parents) {
      if (claims.size !== 1) continue;
      const birth = [...claims][0];
      if (parent) node.parents = node.parents.map(p => p === parent ? birth : p);
      else if (!node.parents.length) node.parents = [birth];
    }
  }
  const projectedRefs = refs.map(ref => ref.kind === 'branch' && mergeForTip.has(`${ref.name}:${ref.target}`)
    ? { ...ref, target: mergeForTip.get(`${ref.name}:${ref.target}`)! } : ref);
  return { snapshots: [...nodes.values()], events, pinHead: projectedHead, refs: projectedRefs };
}
