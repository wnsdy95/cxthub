// Lane layout and reachability for a window of Git commits, as the provider
// lists them. The window is the newest commits by time; a parent outside it is
// not drawn, and the row says that history goes on below what is shown.

export interface GitNode {
  sha: string; parents: string[]; at: string;
  /**
   * Commits drawn joining this one with a dotted line though they are not
   * its parents: the newest commit of a branch whose pull request was merged
   * as this commit by squash or rebase. Reachability ignores them.
   */
  joined?: string[];
}

/** The commits a node is drawn from: its parents, then the commits it joined that are not among them. */
function edgesOf(node: GitNode, joins: boolean): { sha: string; soft: boolean }[] {
  const parents = [...new Set(node.parents)];
  const soft = joins ? [...new Set(node.joined ?? [])].filter(sha => sha !== node.sha && !parents.includes(sha)) : [];
  return [...parents.map(sha => ({ sha, soft: false })), ...soft.map(sha => ({ sha, soft: true }))];
}

function time(value: string): number {
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : -Infinity;
}

/**
 * Children before their parents; among the commits that are ready, the newest
 * first. With `joins`, a commit also comes before the commits it joined.
 */
export function orderGitNodes<Node extends GitNode>(nodes: Node[], joins = false): Node[] {
  const byId = new Map(nodes.map(node => [node.sha, node]));
  const waiting = new Map<string, number>(nodes.map(node => [node.sha, 0]));
  for (const node of nodes) {
    for (const { sha: parent } of edgesOf(node, joins)) {
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
    for (const { sha: parent } of edgesOf(node, joins)) {
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
  /** Lanes in `incoming` and `outgoing` that carry only a dotted line: the way to a commit this one or one above joined. */
  incomingSoft: boolean[];
  outgoingSoft: boolean[];
  /** Those of `branchesOut` that leave for a commit this one joined, drawn dotted. */
  softOut: number[];
  /** A parent is outside the window: the history goes on below what is shown. */
  cut: boolean;
}

/**
 * Lays commits out in lanes the way `git log --graph` does. `pin` keeps one
 * line, usually the default branch, on the first lane. A commit's joined
 * commits are laid out like further parents on lanes marked soft; if they
 * would close a loop, they are left out and the graph is the plain one.
 */
export function layoutGitGraph<Node extends GitNode>(nodes: Node[], pin?: string): { rows: GitRow<Node>[]; laneCount: number } {
  const available = new Set(nodes.map(node => node.sha));
  let joins = nodes.some(node => node.joined?.length);
  let ordered = orderGitNodes(nodes, joins);
  if (ordered.length < nodes.length) {
    joins = false;
    ordered = orderGitNodes(nodes);
  }
  const lanes: (string | null)[] = [];
  const soft: boolean[] = [];
  const open = (lane: number, sha: string | null, dotted: boolean) => { lanes[lane] = sha; soft[lane] = sha !== null && dotted; };
  if (pin && available.has(pin)) open(0, pin, false);
  const rows: GitRow<Node>[] = [];
  let laneCount = 0;
  for (const node of ordered) {
    const incoming = [...lanes];
    const incomingSoft = lanes.map((_, lane) => soft[lane] ?? false);
    // The commit sits on a lane that reaches it by a line of its own when there is one.
    let lane = lanes.findIndex((waiting, index) => waiting === node.sha && !soft[index]);
    if (lane === -1) lane = lanes.indexOf(node.sha);
    if (lane === -1) {
      lane = lanes.indexOf(null);
      if (lane === -1) lane = lanes.length;
      open(lane, node.sha, false);
    }
    const mergesIn: number[] = [];
    for (let other = 0; other < lanes.length; other++) {
      if (other !== lane && lanes[other] === node.sha) {
        mergesIn.push(other);
        open(other, null, false);
      }
    }
    const edges = edgesOf(node, joins);
    const shown = edges.filter(edge => available.has(edge.sha));
    const branchesOut: number[] = [];
    const softOut: number[] = [];
    if (shown.length === 0) {
      open(lane, null, false);
    } else {
      open(lane, shown[0].sha, shown[0].soft);
      for (const parent of shown.slice(1)) {
        let target = lanes.indexOf(parent.sha);
        if (target === -1) {
          target = lanes.indexOf(null);
          if (target === -1) target = lanes.length;
          open(target, parent.sha, parent.soft);
        } else if (!parent.soft) soft[target] = false;
        branchesOut.push(target);
        if (parent.soft) softOut.push(target);
      }
    }
    while (lanes.length > 0 && lanes[lanes.length - 1] === null) {
      lanes.pop();
      soft.length = lanes.length;
    }
    const realParents = new Set(node.parents);
    const cut = [...realParents].some(parent => !available.has(parent));
    rows.push({ node, lane, incoming, outgoing: [...lanes], mergesIn, branchesOut, incomingSoft, outgoingSoft: lanes.map((_, index) => soft[index] ?? false), softOut, cut });
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
