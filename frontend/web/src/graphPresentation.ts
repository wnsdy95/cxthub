import { GraphIndex } from './graphIndex';
import type { visibleBranchGraph } from './graphProjection';

export type GraphViewMode = 'commits' | 'prs';
type Projection = ReturnType<typeof visibleBranchGraph>;

export function summarizePRGraph(projection: Projection, preserved: ReadonlySet<string>) {
  const index = new GraphIndex(projection.snapshots);
  const collapsedCounts = new Map<string, number>();
  if (index.issues.length) return { ...projection, collapsedCounts, hiddenCount: 0 };

  const keep = new Set([...preserved, ...projection.events.keys(), ...projection.refs.map(ref => ref.target)]);
  if (projection.pinHead) keep.add(projection.pinHead);
  const children = new Map<string, number>();
  for (const snapshot of projection.snapshots) {
    const parents = [...new Set([...(snapshot.parents ?? []), ...(snapshot.graft_parents ?? [])])];
    for (const parent of parents) children.set(parent, (children.get(parent) ?? 0) + 1);
    if (snapshot.parents?.length !== 1 || snapshot.grafted || snapshot.graft_parents?.length
      || parents.some(parent => !index.byId.has(parent))) keep.add(snapshot.id);
    for (const parent of snapshot.graft_parents ?? []) keep.add(parent);
    if (snapshot.grafted && !snapshot.graft_parents?.length && snapshot.parents?.[0]) keep.add(snapshot.parents[0]);
  }
  for (const [source, targets] of projection.lifecycleEdges) {
    keep.add(source);
    for (const target of targets) keep.add(target);
  }
  for (const snapshot of projection.snapshots) {
    if (children.get(snapshot.id) !== 1) keep.add(snapshot.id);
  }

  const resolved = new Map<string, { target: string; count: number }>();
  function resolve(start: string) {
    const path: string[] = [];
    let target = start;
    while (index.byId.has(target) && !keep.has(target) && !resolved.has(target)) {
      path.push(target);
      target = index.byId.get(target)!.parents![0];
    }
    let result = resolved.get(target) ?? { target, count: 0 };
    for (let offset = path.length - 1; offset >= 0; offset--) {
      result = { target: result.target, count: result.count + 1 };
      resolved.set(path[offset], result);
    }
    return result;
  }

  const junctionParents = new Set<string>();
  for (const snapshot of projection.snapshots) {
    const parents = [...new Set([...(snapshot.parents ?? []), ...(snapshot.graft_parents ?? [])])];
    if (parents.length < 2) continue;
    const targets = parents.map(parent => resolve(parent).target);
    const targetCounts = new Map<string, number>();
    for (const target of targets) targetCounts.set(target, (targetCounts.get(target) ?? 0) + 1);
    parents.forEach((parent, position) => {
      if (targetCounts.get(targets[position])! > 1) junctionParents.add(parent);
    });
  }
  for (const parent of junctionParents) keep.add(parent);
  if (junctionParents.size) resolved.clear();

  const edgeBranches = new Map(projection.edgeBranches);
  const snapshots = projection.snapshots.filter(snapshot => keep.has(snapshot.id)).map(snapshot => {
    let collapsed = 0;
    const labels = projection.edgeBranches.get(snapshot.id);
    const remappedLabels = new Map<string, string>();
    const parents = (snapshot.parents ?? []).map(parent => {
      const result = resolve(parent);
      collapsed += result.count;
      const label = labels?.get(parent);
      if (label) remappedLabels.set(result.target, label);
      return result.target;
    });
    if (labels) edgeBranches.set(snapshot.id, remappedLabels);
    if (collapsed) collapsedCounts.set(snapshot.id, collapsed);
    return collapsed ? { ...snapshot, parents } : snapshot;
  });
  return { ...projection, snapshots, edgeBranches, collapsedCounts, hiddenCount: projection.snapshots.length - snapshots.length };
}
