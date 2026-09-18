import type { Snapshot } from './types';

export type GraphIssue = { kind: 'duplicate-id' | 'cycle'; ids: string[]; count: number };
export type Closure = { ids: Set<string>; complete: boolean };
type Mode = 'all' | 'conversation';

/** A legacy destructive graft in parents[0] proves inclusion, not conversation continuity. */
export function conversationParents(snapshot: Snapshot): string[] {
  const parents = snapshot.parents ?? [];
  return snapshot.grafted && !snapshot.graft_parents?.length ? parents.slice(1) : parents;
}

/** Iterative traversal also works on the mutable, display-only projection.
 * Never cache it: inserting an operation changes its reachability. */
export function reachesSnapshot(nodes: ReadonlyMap<string, Snapshot>, root: string, target: string): boolean {
  const seen = new Set<string>();
  const stack = [root];
  while (stack.length) {
    const id = stack.pop()!;
    if (id === target) return nodes.has(id);
    if (seen.has(id)) continue;
    seen.add(id);
    const s = nodes.get(id);
    if (s) {
      for (const p of s.parents ?? []) stack.push(p);
      for (const p of s.graft_parents ?? []) stack.push(p);
    }
  }
  return false;
}

/** One immutable repository-view index. Cache limits count memberships, not
 * just roots: caching every closure of a long chain would use quadratic space. */
export class GraphIndex {
  readonly byId: Map<string, Snapshot>;
  readonly missingParents = new Set<string>();
  readonly issues: GraphIssue[] = [];
  readonly stats = { traversed: 0, cacheHits: 0, cachedMemberships: 0 };
  private readonly parents = new Map<string, string[]>();
  private readonly cache = new Map<string, Closure>();
  private readonly intervals = new Map<Mode, Map<string, { start: number; end: number }>>();

  constructor(snapshots: Snapshot[], readonly cacheBudget = 100_000) {
    this.byId = new Map();
    const duplicates = new Set<string>();
    for (const s of snapshots) {
      if (this.byId.has(s.id)) duplicates.add(s.id);
      this.byId.set(s.id, s);
      this.parents.set(s.id, [...new Set([...(s.parents ?? []), ...(s.graft_parents ?? [])].filter(Boolean))]);
    }
    if (duplicates.size) this.issues.push({ kind: 'duplicate-id', ids: [...duplicates].slice(0, 12), count: duplicates.size });
    for (const parents of this.parents.values()) for (const p of parents) {
      if (!this.byId.has(p)) this.missingParents.add(p);
    }
    // DFS stack, not recursion: a valid 100k-node chain must not overflow JS.
    const done = new Set<string>();
    const active = new Map<string, number>();
    for (const root of this.byId.keys()) {
      if (done.has(root)) continue;
      const stack = [{ id: root, next: 0 }];
      active.set(root, 0);
      while (stack.length) {
        const frame = stack[stack.length - 1];
        const parents = this.parents.get(frame.id)!;
        if (frame.next === parents.length) {
          done.add(frame.id); active.delete(frame.id); stack.pop(); continue;
        }
        const p = parents[frame.next++];
        if (!this.byId.has(p) || done.has(p)) continue;
        const cycleStart = active.get(p);
        if (cycleStart !== undefined) {
          const members = stack.slice(cycleStart).map(f => f.id);
          this.issues.push({ kind: 'cycle', ids: members.slice(0, 12), count: members.length });
          return; // One concrete cycle witness is enough to stop drawing.
        }
        active.set(p, stack.length); stack.push({ id: p, next: 0 });
      }
    }
  }

  private edges(id: string, mode: Mode): string[] {
    const s = this.byId.get(id);
    return mode === 'all' ? this.parents.get(id) ?? [] : s ? conversationParents(s) : [];
  }

  /** A DFS subtree proves reachability in O(1). Cross edges still use an exact
   * traversal; intervals never serve as negative ancestry evidence. Starting
   * with tips makes ordinary long session chains cheap regardless of input order. */
  private tree(mode: Mode) {
    const cached = this.intervals.get(mode);
    if (cached) return cached;
    const intervals = new Map<string, { start: number; end: number }>();
    const nonTips = new Set<string>();
    for (const id of this.byId.keys()) for (const p of this.edges(id, mode)) nonTips.add(p);
    let clock = 0;
    const visit = (root: string) => {
      if (intervals.has(root)) return;
      intervals.set(root, { start: clock++, end: -1 });
      const stack = [{ id: root, next: 0 }];
      while (stack.length) {
        const frame = stack[stack.length - 1];
        const edges = this.edges(frame.id, mode);
        if (frame.next === edges.length) {
          intervals.get(frame.id)!.end = clock; stack.pop(); continue;
        }
        const p = edges[frame.next++];
        if (!this.byId.has(p) || intervals.has(p)) continue;
        intervals.set(p, { start: clock++, end: -1 }); stack.push({ id: p, next: 0 });
      }
    };
    for (const id of this.byId.keys()) if (!nonTips.has(id)) visit(id);
    for (const id of this.byId.keys()) visit(id);
    this.intervals.set(mode, intervals);
    return intervals;
  }

  reaches(root: string, target: string, mode: Mode = 'all'): boolean {
    if (!this.byId.has(root) || !this.byId.has(target)) return false;
    const cached = this.cache.get(`${mode}:${root}`);
    if (cached) { this.stats.cacheHits++; return cached.ids.has(target); }
    const tree = this.tree(mode), a = tree.get(root)!, b = tree.get(target)!;
    if (a.start <= b.start && b.start < a.end) return true;
    const seen = new Set<string>();
    const stack = [root];
    while (stack.length) {
      const id = stack.pop()!;
      if (id === target) return true;
      if (seen.has(id)) continue;
      seen.add(id); this.stats.traversed++;
      for (const p of this.edges(id, mode)) stack.push(p);
    }
    return false;
  }

  closure(root: string, mode: Mode = 'all'): Closure {
    const key = `${mode}:${root}`, cached = this.cache.get(key);
    if (cached) { this.stats.cacheHits++; this.cache.delete(key); this.cache.set(key, cached); return cached; }
    const ids = new Set<string>(), stack = [root];
    let complete = true;
    while (stack.length) {
      const id = stack.pop()!;
      if (ids.has(id)) continue;
      ids.add(id); this.stats.traversed++;
      if (!this.byId.has(id)) { complete = false; continue; }
      for (const p of this.edges(id, mode)) stack.push(p);
    }
    const result = { ids, complete };
    if (ids.size <= this.cacheBudget) {
      while (this.cache.size && (this.stats.cachedMemberships + ids.size > this.cacheBudget || this.cache.size >= 64)) {
        const oldest = this.cache.keys().next().value!;
        this.stats.cachedMemberships -= this.cache.get(oldest)!.ids.size; this.cache.delete(oldest);
      }
      this.cache.set(key, result); this.stats.cachedMemberships += ids.size;
    }
    return result;
  }
}
