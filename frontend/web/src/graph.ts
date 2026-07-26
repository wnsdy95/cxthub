// Commit graph layout — same mental model as git log --graph.
//
// Input: Latest snapshots (including parent links). Output: Row by row (node lanes, connection status of above/below lanes).
// Algorithm (top→bottom = latest→past):
//   Each lane holds "commit hash expected next".
//   When processing commit c: c's lane (new lane if none = branch tip).
//   Other lanes expecting c join this node (fork point — reverse of fork).
//   Node lane updates to expect first parent, additional parents (merges) open new lanes.
import type { Ref, Snapshot } from './types';

export interface GraphRow {
  snap: Snapshot;
/** Index of lane where the node is placed */
  lane: number;
/** State of the lane above the row (expected hash, null = empty lane) */
  incoming: (string | null)[];
/** State of the lane below the row */
  outgoing: (string | null)[];
/** Indices of lanes joining above this node (fork points) */
  mergesIn: number[];
/** Indices of lanes opening below this node (additional parents) */
  branchesOut: number[];
}

// Session boundary: parent exists, both session_id present, and different snapshots (new agent session's first commit). Snapshots without session_id are undeterminable → no boundary (prefer underreporting over overreporting).
//
// Even if grafted, if session_id is actually different, it is marked as a session boundary (true session start > graft notation).
// Purpose of restoring "new session start" marker in snapshots polluted by past timeout recovery — pure grafts (same session) have the same session_id, so they don't trigger here.
export function sessionBoundaries(snapshots: Snapshot[]): Set<string> {
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  const out = new Set<string>();
  for (const s of snapshots) {
    const p = s.parents?.[0] ? byId.get(s.parents[0]) : undefined;
    if (p && s.session_id && p.session_id && s.session_id !== p.session_id) out.add(s.id);
  }
  return out;
}

// Compression boundary: compact_boundary does not change sessionId, so it is not captured by sessionBoundaries. compaction_count greater than first parent's count = compaction occurred after this point. Snapshots without parent or count info are undeterminable → no boundary.
export function compactionBoundaries(snapshots: Snapshot[]): Set<string> {
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  const out = new Set<string>();
  for (const s of snapshots) {
    const p = s.parents?.[0] ? byId.get(s.parents[0]) : undefined;
    if (p && (s.compaction_count ?? 0) > (p.compaction_count ?? 0)) out.add(s.id);
  }
  return out;
}

// Primary lineage (first-parent) — chain from head following only the natural parent (parents[0]). Appends (grafts) do not include merged branches: "direct ancestor" determination's single source. Note: If append occurs, the direct ancestor moves to the chain of the last merged session (like git FF).
export function mainlineOf(head: string | undefined, snapshots: Snapshot[]): Set<string> {
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  const out = new Set<string>();
  let cur = head;
  while (cur && !out.has(cur)) {
    const s = byId.get(cur);
    if (!s) break;
    out.add(cur);
    cur = s.parents?.[0];
  }
  return out;
}

// Union of primary lineages of all branch refs — snapshots not in this union (but reachable) = merged branches (side chains).
export function mainlinesOf(refs: Ref[], snapshots: Snapshot[]): Set<string> {
  const out = new Set<string>();
  const byId = new Map(snapshots.map((s) => [s.id, s]));
  for (const r of refs) {
    if (r.kind !== 'branch' || !r.target) continue;
    let cur: string | undefined = r.target;
    while (cur && !out.has(cur)) {
      const s = byId.get(cur);
      if (!s) break;
      out.add(cur);
      cur = s.parents?.[0];
    }
  }
  return out;
}

export function layoutGraph(snapshots: Snapshot[], pinHead?: string | null): { rows: GraphRow[]; laneCount: number } {
  const lanes: (string | null)[] = [];
  // Default branch fix: pinHead (default branch's head) pinned to the expected value in lane 0 ensures the entire chain is always on the leftmost lane, and newer feature tips receive the right lane. If head is not in the snapshot list (e.g., unpinned), it is not pinned — preventing an empty vertical line from drawing to the end of the graph.
  if (pinHead && snapshots.some((s) => s.id === pinHead)) lanes.push(pinHead);
  const rows: GraphRow[] = [];
  let laneCount = 0;

  for (const snap of snapshots) {
    const incoming = [...lanes];
    // 1) Node lane: first lane expected for this commit, or reuse an empty slot or create a new one.
    let lane = lanes.findIndex((h) => h === snap.id);
    if (lane === -1) {
      lane = lanes.findIndex((h) => h === null);
      if (lane === -1) lane = lanes.length;
      lanes[lane] = snap.id; // not present in incoming, so no line continues upward into this tip
    }
    // 2) Expected commits from other lanes join and close (at the fork point).
    const mergesIn: number[] = [];
    for (let j = 0; j < lanes.length; j++) {
      if (j !== lane && lanes[j] === snap.id) {
        mergesIn.push(j);
        lanes[j] = null;
      }
    }
    // 3) Parent connection: The first parent is the node line continuation, the rest are new lines (merge commit).
    // graft_parents (server overlay graft edge) should also be considered as parents for the join curve to be drawn —
    // if not drawn, the graft previous history will appear as a disconnected component (reachability = parents ∪ graft_parents).
    const parents = [...(snap.parents ?? []), ...(snap.graft_parents ?? [])].filter(
      (p, i, arr) => Boolean(p) && arr.indexOf(p) === i,
    );
    const branchesOut: number[] = [];
    if (parents.length === 0) {
      lanes[lane] = null; // root — lane end
    } else {
      lanes[lane] = parents[0];
      for (const p of parents.slice(1)) {
        if (lanes.includes(p)) continue; // converge on the lane that is already waiting for this parent
        let k = lanes.findIndex((h) => h === null);
        if (k === -1) k = lanes.length;
        lanes[k] = p;
        branchesOut.push(k);
      }
    }
    // Clean up the tail's empty lanes (minimize horizontal width).
    while (lanes.length > 0 && lanes[lanes.length - 1] === null) lanes.pop();

    rows.push({ snap, lane, incoming, outgoing: [...lanes], mergesIn, branchesOut });
    laneCount = Math.max(laneCount, incoming.length, lanes.length, lane + 1);
  }
  return { rows, laneCount };
}
