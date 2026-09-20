import type { GraphState, Ref, Snapshot } from './types';
import { reachesSnapshot } from './graphIndex';

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

/** Route server-confirmed operations through virtual display nodes. No history,
 * publication, identity or completion is inferred here. Stored parents stay intact. */
export function projectBranchGraph(snapshots: Snapshot[], refs: Ref[], state: GraphState | undefined, pinHead?: string | null, pinBranch?: string) {
  const nodes = new Map(snapshots.map(s => [s.id, {...s, parents: [...(s.parents ?? [])], graft_parents: [...(s.graft_parents ?? [])]}]));
  const events = new Map<string, GraphEvent>();
  const lifecycleEdges = new Map<string, Set<string>>();
  const replacements = new Map<string,string>();
  const inactive = new Set<string>();
  let projectedHead = pinHead;
  const integrations = state?.integrations ?? [];
  const integrationParents = new Map(integrations.flatMap(i => Object.entries(i.parents)));
  const integrationScopes = new Set(integrations.map(i => i.scope));
  const key = (scope: string, target: string) => JSON.stringify([scope,target]);
  for (const merge of state?.operations.merges ?? []) {
    const source = nodes.get(merge.source);
    if (!source) continue; // incomplete data is reported by the query contract
    const previousCandidate = replacements.get(key(merge.scope,merge.before));
    const previous = previousCandidate && !inactive.has(previousCandidate) ? previousCandidate : merge.before;
    const replaced = replacements.get(key(merge.scope,merge.after));
    const node = {...source, id: merge.id, branch: merge.branch, parents: [previous,merge.source], graft_parents: [], grafted: false,
      created_at: merge.created_at, message: `${merge.from} → ${merge.branch}`, memory_hash: undefined, session_id: undefined};
    nodes.set(merge.id,node);
    events.set(merge.id,{id:merge.id,kind:'merge',branch:merge.branch,sourceBranch:merge.from,snapshot:merge.source,evidence:merge.event_id,prNumber:merge.pr_number});
    const integrated = integrationParents.get(merge.id);
    if (integrated) { node.parents = [...integrated]; continue; }
    // An unverified or past operation remains historical when this branch has
    // a code-scoped integration timeline. It cannot replace the active head.
    if (integrationScopes.has(merge.scope)) { lifecycleEdges.set(merge.id,new Set(node.parents)); inactive.add(merge.id); continue; }
    if (merge.historical_only) { lifecycleEdges.set(merge.id,new Set(node.parents)); inactive.add(merge.id); continue; }
    replacements.set(key(merge.scope,merge.after),merge.id);
    if (merge.withdrawn) inactive.add(merge.id);
    for (const id of merge.represented_grafts) {
      const n = nodes.get(id);
      if (n) { n.graft_parents = n.graft_parents.filter(p => p !== merge.before); if (!n.graft_parents.length) n.grafted = false; }
    }
    for (const id of merge.redirect_children) {
      const n = nodes.get(id);
      if (n) n.parents = n.parents.map(p => p === merge.after || p === replaced ? merge.id : p);
    }
    if (!merge.withdrawn && pinBranch && state?.ref_scopes[pinBranch] === merge.scope && pinHead === merge.after) projectedHead = merge.id;
  }
  for (const birth of state?.operations.births ?? []) {
    const source = nodes.get(birth.source);
    if (!source) continue;
    nodes.set(birth.id,{...source,id:birth.id,branch:birth.branch,parents:birth.orphan ? [] : [birth.source],graft_parents:[],grafted:false,
      created_at:birth.created_at,message:birth.branch,memory_hash:undefined,session_id:undefined});
    events.set(birth.id,{id:birth.id,kind:'birth',branch:birth.branch,snapshot:birth.source,evidence:birth.event_id,orphan:birth.orphan || undefined});
    for (const id of birth.children) {
      const n = nodes.get(id);
      if (n) n.parents = birth.orphan ? [birth.id] : n.parents.map(p => p === birth.source ? birth.id : p);
    }
  }
  for (const merge of state?.operations.merges ?? []) {
    const node = nodes.get(merge.id);
    if (!node) continue;
    if (merge.source_birth && nodes.has(merge.source_birth)) {
      // A no-change PR may use the same real snapshot for main and source.
      // Replace only the source arm, preserving main and prior integrations.
      node.parents = [...(node.parents.length === 1 ? [merge.source] : node.parents.slice(0,-1)),merge.source_birth];
    }
    const birth = merge.lifecycle_birth;
    if (!birth || !nodes.has(birth) || reachesSnapshot(nodes,merge.source,birth) || node.parents.includes(birth) || reachesSnapshot(nodes,birth,merge.id)) continue;
    const previous = node.parents[0];
    const alreadyIncluded = reachesSnapshot(nodes,previous,merge.source);
    node.parents = [previous,birth,...node.parents.slice(1).filter(p => !alreadyIncluded || p !== merge.source)];
    const edges = lifecycleEdges.get(merge.id) ?? new Set<string>(); edges.add(birth); lifecycleEdges.set(merge.id,edges);
  }
  for (const integration of integrations) {
    const target = nodes.get(integration.target);
    if (target) target.graft_parents = [...new Set([...target.graft_parents,...integration.extra_parents])];
    if (pinBranch === integration.branch && pinHead === integration.target) projectedHead = integration.head;
  }
  const projectedRefs = refs.map(ref => {
    const integration = ref.kind === 'branch' ? integrations.find(i => i.branch === ref.name && i.target === ref.target) : undefined;
    if (integration) return {...ref,target:integration.head};
    const scope = state?.ref_scopes[ref.name];
    const target = ref.kind === 'branch' && scope ? replacements.get(key(scope,ref.target)) : undefined;
    return target && !inactive.has(target) ? {...ref,target} : ref;
  });
  return {snapshots:[...nodes.values()],events,lifecycleEdges,pinHead:projectedHead,refs:projectedRefs};
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
