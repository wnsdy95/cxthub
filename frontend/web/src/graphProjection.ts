import type { HistoryEvent, Ref, RefLogEntry, Snapshot } from './types';
import { parseBranchLifecycleRef } from './branchLifecycle';
import { GraphIndex, reachesSnapshot } from './graphIndex';
import { completedBranchEvidence, conversationParents } from './graphEvidence';
import { graphBranchBindings } from './contextHistory';

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
export function projectBranchGraph(snapshots: Snapshot[], refs: Ref[], history: HistoryEvent[], reflog: RefLogEntry[], pinHead?: string | null, pinBranch?: string, index = new GraphIndex(snapshots), evidence = completedBranchEvidence(snapshots, history, index)) {
  if (index.issues.length) return { snapshots: [] as Snapshot[], events: new Map<string, GraphEvent>(), lifecycleEdges: new Map<string, Set<string>>(), pinHead, refs };
  const time = (value: string) => Date.parse(value);
  const { byId } = index;
  const bindings = graphBranchBindings(refs, history);
  const refsByName = new Map(refs.filter(r => r.kind === 'branch').map(r => [r.name, r]));
  const scopeOf = (name: string) => bindings.refKey(refsByName.get(name)
    ?? { kind: 'branch', name, target: '', repo_id: '' });
  const branchTargets = new Map(refs.filter(r => r.kind === 'branch').map(r => [bindings.refKey(r), r.target]));
  const nodes = new Map(snapshots.map(s => [s.id, { ...s, parents: [...(s.parents ?? [])], graft_parents: [...(s.graft_parents ?? [])] }]));
  const children = new Map<string, Set<string>>();
  const addChild = (parent: string, child: string) => {
    const ids = children.get(parent) ?? new Set<string>(); ids.add(child); children.set(parent, ids);
  };
  for (const s of nodes.values()) for (const p of s.parents) addChild(p, s.id);
  const events = new Map<string, GraphEvent>();
  // Separately typed display edges: same branch identity from birth to a
  // verified PR completion. They never attest conversation ancestry.
  const lifecycleEdges = new Map<string, Set<string>>();
  const births = new Map<string, HistoryEvent>();
  const birthCounts = new Map<string, number>();
  for (const h of history) {
    if (h.kind === 'birth' || h.kind === 'orphan') birthCounts.set(h.branch_id, (birthCounts.get(h.branch_id) ?? 0) + 1);
  }
  const branchTips = new Map<string, Set<string>>();
  const addTip = (id: string, name: string) => { const names = branchTips.get(id) ?? new Set<string>(); names.add(name); branchTips.set(id, names); };
  for (const ref of refs) {
    const lifecycle = parseBranchLifecycleRef(ref);
    if (lifecycle) addTip(ref.target, lifecycle.branch);
    else if (ref.kind === 'branch') addTip(ref.target, ref.name);
  }
  for (const h of history) {
    if (h.kind === 'birth' && birthCounts.get(h.branch_id) === 1 && h.source && h.source === h.target && byId.has(h.source)) births.set(h.branch_id, h);
    if (h.kind !== 'pr-merge' && h.target) addTip(h.target, h.branch);
  }
  let projectedHead = pinHead;
  const merges: Array<{ id: string; before: string; after: string; source: string; branch: string; scope: string; from: string; at: string; identity?: string }> = [];
  const completed = evidence.filter(e => e.merged).map(e => e.merge);
  const completedById = new Map(completed.map(h => [`graph:merge:${h.id}`, h]));
  const completedMoves = new Set(completed.map(h => JSON.stringify([h.branch_id, h.shared_target, h.target])));
  const publications = new Map<string, Set<string>>();
  const publicationTimes = new Map<string, number>();
  const receiptsByMove = new Map<string, HistoryEvent[]>();
  const movementsByScope = new Map<string, Array<{old?: string; next?: string; at: string}>>();
  const addMovement = (scope: string, move: {old?: string; next?: string; at: string}) => {
    const list = movementsByScope.get(scope) ?? []; list.push(move); movementsByScope.set(scope, list);
  };
  for (const h of history) {
    if (h.kind === 'advance' || h.kind === 'publish') {
      const ids = publications.get(h.branch_id) ?? new Set<string>();
      if (h.target) ids.add(h.target);
      publications.set(h.branch_id, ids);
      const key = JSON.stringify([h.branch, h.target]);
      publicationTimes.set(key, Math.min(publicationTimes.get(key) ?? Infinity, time(h.created_at)));
    }
    if (h.kind === 'advance') addMovement(h.branch_id, {old:h.source, next:h.target, at:h.created_at});
    if (h.kind === 'pr-merge' && !h.pr_completed && h.pr) {
      const key = JSON.stringify([h.branch, h.source, h.shared_target]);
      const list = receiptsByMove.get(key) ?? []; list.push(h); receiptsByMove.set(key, list);
    }
  }
  for (const r of reflog) if (r.kind === 'branch') addMovement(scopeOf(r.name), {old:r.old, next:r.new, at:r.created_at});
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
    merges.push({id: `graph:merge:${h.id}`, before: h.shared_target!, after: h.target!, source: h.source!, branch: bindings.label(h.branch_id, h.branch), scope: h.branch_id,
      from: h.pr!.head_branch, at: h.created_at, identity: h.source_branch_id});
  }
  // Chronological ref order is evidence order. Reject ambiguous names, backward
  // moves and missing endpoints instead of inferring them from snapshot.branch.
  for (const move of reflog) {
    if (bindings.snapshotKey(move.name) === undefined) continue; // legacy name cannot identify a reused generation
    if (move.kind !== 'branch' || !move.old || !move.new || move.old === move.new || !byId.has(move.old) || !byId.has(move.new)) continue;
    if (!index.reaches(move.new, move.old)) continue;
    const receipts = (receiptsByMove.get(JSON.stringify([move.name, move.new, move.old])) ?? []).filter(h => time(h.created_at) <= time(move.created_at));
    if (receipts.length > 1) continue;
    if (completedMoves.has(JSON.stringify([scopeOf(move.name), move.old, move.new]))) continue;
    const candidates = [...(branchTips.get(move.new) ?? [])].filter(name => name !== move.name);
    const incoming = index.closure(move.new).ids;
    const previous = index.closure(move.old).ids;
    const hasAppendEdge = [...incoming].some(id => !previous.has(id) && byId.get(id)?.graft_parents?.includes(move.old));
    // A current shared hash can also be a later fork point. Without a bound PR
    // receipt, only a stored append edge proves this was a join.
    // Publishing a branch's own capture can carry an append of earlier main.
    // A later main ref/position at that hash does not prove a reverse PR into
    // the source branch. Legacy inference needs a distinct originating branch.
    const ownPublication = (publicationTimes.get(JSON.stringify([move.name, move.new])) ?? Infinity) <= time(move.created_at);
    if (receipts.length !== 1 && (!hasAppendEdge || byId.get(move.new)?.branch === move.name || ownPublication)) continue;
    const from = receipts.length === 1 ? receipts[0].pr!.head_branch : candidates.length === 1 ? candidates[0] : undefined;
    if (!from) continue;
    merges.push({ id: `graph:merge:${move.created_at}:${move.name}:${move.old}:${move.new}`, before: move.old, after: move.new, source: move.new, branch: move.name, scope: scopeOf(move.name), from, at: move.created_at, identity: receipts[0]?.source_branch_id });
  }
  merges.sort((a,b) => time(a.at) - time(b.at) || a.id.localeCompare(b.id));
  const mergeForTip = new Map<string, string>();
  const inactiveMerges = new Set<string>();
  for (const merge of merges) {
    if (events.has(merge.id)) continue;
    const source = byId.get(merge.source)!;
    const previousCandidate = mergeForTip.get(`${merge.scope}:${merge.before}`);
    const previous = previousCandidate && !inactiveMerges.has(previousCandidate) ? previousCandidate : merge.before;
    const replacedTip = mergeForTip.get(`${merge.scope}:${merge.after}`);
    const node: Snapshot = { ...source, id: merge.id, branch: merge.branch, parents: [previous, merge.source], graft_parents: [], grafted: false,
      created_at: merge.at, message: `${merge.from} → ${merge.branch}`, memory_hash: undefined, session_id: undefined };
    nodes.set(merge.id, { ...node, parents: node.parents ?? [], graft_parents: [] });
    for (const p of node.parents ?? []) addChild(p, merge.id);
    const receipt = completedById.get(merge.id);
    events.set(merge.id, { id: merge.id, kind: 'merge', branch: merge.branch, sourceBranch: merge.from, snapshot: merge.source, evidence: receipt?.id ?? 'ref-move', prNumber: receipt?.pr?.number });
    mergeForTip.set(`${merge.scope}:${merge.after}`, merge.id);
    // Explicit movements away from this lineage supersede its placement.
    // Compare server operation times here, never provider snapshot timestamps.
    const movements = movementsByScope.get(merge.scope) ?? [];
    const withdrawn = movements.some(m => m.old && m.next && time(m.at) > time(merge.at)
      && index.reaches(m.old, merge.after)
      && !index.reaches(m.next, m.old));
    if (withdrawn) inactiveMerges.add(merge.id);
    // The verified ref move already represents this append edge. Drawing its
    // storage graft too would invert main and feature paths a second time.
    const segment = index.closure(merge.source).ids;
    const previousIDs = index.closure(merge.before).ids;
    for (const id of segment) {
      if (previousIDs.has(id)) continue;
      const n = nodes.get(id);
      if (n?.graft_parents.length) {
        n.graft_parents = n.graft_parents.filter(p => p !== merge.before);
        // Removing the represented overlay must not reclassify the remaining
        // natural parent as a legacy destructive-graft seam in the renderer.
        if (!n.graft_parents.length) n.grafted = false;
      }
    }
    const affected = new Set([...(children.get(merge.after) ?? []), ...(replacedTip ? children.get(replacedTip) ?? [] : [])]);
    for (const child of affected) {
      const n = nodes.get(child)!;
      const membership = publications.get(merge.scope)?.has(n.id);
      if (n.id === merge.id || events.has(n.id) || segment.has(n.id)
        || (!membership && bindings.snapshotKey(n.branch ?? '') !== merge.scope)) continue;
      // The active destination path supplies ancestry evidence even when a
      // provider clock precedes the server completion. Unproven retained paths
      // keep their original parents; wall-clock order must not assign them.
      if (withdrawn || !index.reaches(branchTargets.get(merge.scope) ?? '', n.id)) continue;
      // Several completed PRs can leave the stored ref unchanged. Children
      // already redirected to its earlier projection must follow the latest
      // operation, or later joins become detached tips beside main.
      const parents = n.parents.map(p => p === merge.after || p === replacedTip ? merge.id : p);
      for (const p of n.parents) if (!parents.includes(p)) children.get(p)?.delete(n.id);
      for (const p of parents) addChild(p, n.id);
      n.parents = parents;
    }
    if (!withdrawn && pinBranch && scopeOf(pinBranch) === merge.scope && pinHead === merge.after) projectedHead = merge.id;
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
  for (const birth of history.filter(h => h.kind === 'orphan' && birthCounts.get(h.branch_id) === 1)) {
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
  for (const { merge, birth, merged } of evidence) {
    if (!merged || !birth || birth.kind !== 'birth' || !(time(birth.created_at) <= time(merge.created_at))) continue;
    const birthId = `graph:birth:${birth.id}`;
    const mergeId = `graph:merge:${merge.id}`;
    const node = nodes.get(mergeId);
    if (!node || !nodes.has(birthId)) continue;
    // Normal captures already join their own birth. Shared/legacy captures
    // may not: connect the confirmed operations, never change their parents.
    if (reachesSnapshot(nodes, merge.source!, birthId) || node.parents.includes(birthId)) continue;
    // Inconsistent historical evidence must not create a display cycle.
    if (reachesSnapshot(nodes, birthId, mergeId)) continue;
    const previous = node.parents[0];
    // A no-op PR's source is already on main. Repeating that source edge would
    // obscure the branch identity, especially when several PRs share a hash.
    const alreadyIncluded = reachesSnapshot(nodes, previous, merge.source!);
    node.parents = [previous, birthId, ...node.parents.slice(1).filter(p => !alreadyIncluded || p !== merge.source)];
    lifecycleEdges.set(mergeId, new Set([birthId]));
  }
  const projectedRefs = refs.map(ref => {
    const target = ref.kind === 'branch' ? mergeForTip.get(`${bindings.refKey(ref)}:${ref.target}`) : undefined;
    return target && !inactiveMerges.has(target) ? { ...ref, target } : ref;
  });
  return { snapshots: [...nodes.values()], events, lifecycleEdges, pinHead: projectedHead, refs: projectedRefs };
}

/** Visibility is applied only after evidence and operation edges are fixed.
 * Retain virtual ancestors of visible rows, but stop at hidden real captures:
 * skipping across those captures would fabricate a conversation edge. Events
 * with a visible source remain selectable even without a visible descendant. */
export function visibleBranchGraph(projection: ReturnType<typeof projectBranchGraph>, visibleIds: ReadonlySet<string>) {
  const byId = new Map(projection.snapshots.map(s => [s.id, s]));
  const included = new Set<string>();
  const stack = [...visibleIds];
  for (const event of projection.events.values()) if (visibleIds.has(event.snapshot)) stack.push(event.id);
  while (stack.length) {
    const id = stack.pop()!;
    if (included.has(id)) continue;
    const node = byId.get(id);
    if (!node) continue;
    included.add(id);
    for (const p of [...(node.parents ?? []), ...(node.graft_parents ?? [])]) {
      if (projection.events.has(p)) stack.push(p);
    }
  }
  const snapshots = projection.snapshots.filter(s => included.has(s.id));
  const foldedParents = new Set<string>();
  for (const s of snapshots) for (const p of [...(s.parents ?? []), ...(s.graft_parents ?? [])]) {
    if (byId.has(p) && !included.has(p)) foldedParents.add(p);
  }
  return { ...projection, snapshots, foldedParents };
}
