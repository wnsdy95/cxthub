// Lane layout and reachability for a window of Git commits, as the provider
// lists them. The window is the newest commits by time; a parent outside it is
// not drawn, and the row says that history goes on below what is shown.

export interface GitNode { sha: string; parents: string[]; at: string }

function time(value: string): number {
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : -Infinity;
}

/** Children before their parents; among the commits that are ready, the newest first. */
export function orderGitNodes<Node extends GitNode>(nodes: Node[]): Node[] {
  const byId = new Map(nodes.map(node => [node.sha, node]));
  const waiting = new Map<string, number>(nodes.map(node => [node.sha, 0]));
  for (const node of nodes) {
    for (const parent of new Set(node.parents)) {
      if (byId.has(parent)) waiting.set(parent, waiting.get(parent)! + 1);
    }
  }
  const newest = (a: Node, b: Node) => time(b.at) - time(a.at) || a.sha.localeCompare(b.sha);
  const ready = nodes.filter(node => waiting.get(node.sha) === 0).sort(newest);
  const ordered: Node[] = [];
  while (ready.length > 0) {
    const node = ready.shift()!;
    ordered.push(node);
    let added = false;
    for (const parent of new Set(node.parents)) {
      if (!byId.has(parent)) continue;
      const left = waiting.get(parent)! - 1;
      waiting.set(parent, left);
      if (left === 0) {
        ready.push(byId.get(parent)!);
        added = true;
      }
    }
    if (added) ready.sort(newest);
  }
  return ordered;
}

export interface GitRow<Node> {
  node: Node;
  lane: number;
  /** The commit each lane is waiting for above this row, and below it. */
  incoming: (string | null)[];
  outgoing: (string | null)[];
  /** Other lanes that were waiting for this commit; they end here. */
  mergesIn: number[];
  /** Lanes the second and later parents continue on. */
  branchesOut: number[];
  /** A parent is outside the window: the history goes on below what is shown. */
  cut: boolean;
}

/** Lays commits out in lanes the way `git log --graph` does. `pin` keeps one line, usually the default branch, on the first lane. */
export function layoutGitGraph<Node extends GitNode>(nodes: Node[], pin?: string): { rows: GitRow<Node>[]; laneCount: number } {
  const available = new Set(nodes.map(node => node.sha));
  const lanes: (string | null)[] = [];
  if (pin && available.has(pin)) lanes.push(pin);
  const rows: GitRow<Node>[] = [];
  let laneCount = 0;
  for (const node of orderGitNodes(nodes)) {
    const incoming = [...lanes];
    let lane = lanes.indexOf(node.sha);
    if (lane === -1) {
      lane = lanes.indexOf(null);
      if (lane === -1) lane = lanes.length;
      lanes[lane] = node.sha;
    }
    const mergesIn: number[] = [];
    for (let other = 0; other < lanes.length; other++) {
      if (other !== lane && lanes[other] === node.sha) {
        mergesIn.push(other);
        lanes[other] = null;
      }
    }
    const parents = [...new Set(node.parents)];
    const shown = parents.filter(parent => available.has(parent));
    const branchesOut: number[] = [];
    if (shown.length === 0) {
      lanes[lane] = null;
    } else {
      lanes[lane] = shown[0];
      for (const parent of shown.slice(1)) {
        let target = lanes.indexOf(parent);
        if (target === -1) {
          target = lanes.indexOf(null);
          if (target === -1) target = lanes.length;
          lanes[target] = parent;
        }
        branchesOut.push(target);
      }
    }
    while (lanes.length > 0 && lanes[lanes.length - 1] === null) lanes.pop();
    rows.push({ node, lane, incoming, outgoing: [...lanes], mergesIn, branchesOut, cut: shown.length < parents.length });
    laneCount = Math.max(laneCount, incoming.length, lanes.length, lane + 1);
  }
  return { rows, laneCount };
}

/**
 * The commits reachable from `head` that are not reachable from `base`: what a
 * branch holds that its base branch does not yet. `complete` is false when the
 * window ends before that can be told.
 */
export function commitsNotOn<Node extends GitNode>(byId: Map<string, Node>, head: string, base: string): { shas: string[]; complete: boolean } {
  if (!byId.has(head) || !byId.has(base)) return { shas: [], complete: false };
  const onBase = new Set<string>();
  const queue = [base];
  while (queue.length > 0) {
    const sha = queue.pop()!;
    if (onBase.has(sha)) continue;
    onBase.add(sha);
    for (const parent of byId.get(sha)?.parents ?? []) if (byId.has(parent)) queue.push(parent);
  }
  const own = new Set<string>();
  let complete = true;
  const walk = [head];
  while (walk.length > 0) {
    const sha = walk.pop()!;
    if (onBase.has(sha) || own.has(sha)) continue;
    const node = byId.get(sha);
    if (!node) {
      complete = false;
      continue;
    }
    own.add(sha);
    walk.push(...node.parents);
  }
  const shas = orderGitNodes([...own].map(sha => byId.get(sha)!)).map(node => node.sha);
  return { shas, complete };
}
