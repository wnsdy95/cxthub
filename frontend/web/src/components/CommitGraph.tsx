// CommitGraph — GitHub network graph style commit tree.
// Text-free pure graph: lane colors, top branch labels, node tooltips on hover, viewer integration on click.
// Lane layout is handled in graph.ts (pure function), this file renders only the SVG.
import { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { Ref, Snapshot } from '../types';
import { layoutGraph, mainlineOf, mainlinesOf, sessionBoundaries, compactionBoundaries } from '../graph';
import { sharedReachable } from '../onhold';
import { classifyGraphSnapshots } from '../graphStatus';
import { useJoinSnapshot } from '../hooks';
import { useT } from '../i18n';

const LANE_W = 22; // Lane width
const ROW_H = 26; // Row height (text-free — compact)
const HEAD_H = 44; // Sticky lane-label area; kept in sync with .graph-head.
const R = 4.5; // Node radius

// Cycle lane colors (ink + desaturated colors).
const LANE_COLORS = ['#16181d', '#2e7d5b', '#8250df', '#b4452c', '#0969da', '#bf8700'];
const laneColor = (i: number) => LANE_COLORS[i % LANE_COLORS.length];
const cx = (lane: number) => lane * LANE_W + LANE_W / 2;
// Graft join color — append edges are a gradient from lane color to join color with dotted lines.
// "Same branch but different context session joined" is indicated.
const SEAM = '#d29922';
const SEAM_DASH = '3 3';
// Session boundary — lineage stays connected, but the edge where the agent session changes uses a dotted line and tear-line tick.
// It is visually weaker than a graft seam: retain the lane color without a color transition.
const SESSION_DASH = '1.5 3';
const TICK = '#8a919e';
// Compression boundary — nodes where the context window is compressed within the same session. Unlike session boundaries (edge ticks),
// the sequence does not break, so nodes are marked with a separate ring (different color from graft seam).
const COMPACT = '#8957e5';

function occupiedLanes(row: {
  lane: number;
  incoming: (string | null)[];
  outgoing: (string | null)[];
}): number[] {
  const lanes: number[] = [];
  const count = Math.max(row.lane + 1, row.incoming.length, row.outgoing.length);
  for (let lane = 0; lane < count; lane++) {
    if (lane === row.lane || row.incoming[lane] || row.outgoing[lane]) lanes.push(lane);
  }
  return lanes;
}

// Internal session ref includes branch byte length as a separate component to prevent prefix comparison from misidentifying as a git branch.
function sessionRefPrefix(branch: string): string {
  return `fork/v1/${new TextEncoder().encode(branch).length}/${branch}/`;
}

function when(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

export function CommitGraph({
  snapshots,
  selectedId,
  onSelect,
  badges,
  refs,
  uncommitted,
  pinBranch,
  joinBranch,
  repoId,
}: {
  snapshots: Snapshot[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  badges: Map<string, { name: string; kind: string }[]>;
/** Branch ref list. If present, unpushed commits outside the shared timeline are lightened and separated by a tear line. */
  refs?: Ref[];
/** Uncommitted hook-capture IDs, rendered as hollow dashed nodes with their own divider. */
  uncommitted?: Set<string>;
/** Default branch name — always fixed at the leftmost lane (0) for this branch chain. */
  pinBranch?: string;
/** Join target git branch — fixed to the current branch to avoid guessing multiple memberships */
  joinBranch?: string;
/** repo ID — activates drag-and-drop join if provided */
  repoId?: string | null;
}) {
  const t = useT();
  const [showArchived, setShowArchived] = useState(false);
  const status = useMemo(
    () => classifyGraphSnapshots(refs ?? [], snapshots, uncommitted),
    [refs, snapshots, uncommitted],
  );
  const selectedArchived = selectedId !== null && status.archivedOnly.has(selectedId);
  const archivedVisible = showArchived || selectedArchived;
  const visibleSnapshots = useMemo(
    () => archivedVisible ? snapshots : snapshots.filter((snapshot) => !status.archivedOnly.has(snapshot.id)),
    [archivedVisible, snapshots, status.archivedOnly],
  );
  const graphIdentity = refs?.[0]?.repo_id ?? repoId ?? '';
  useEffect(() => setShowArchived(false), [graphIdentity]);
  const pinHead = useMemo(
    () => (pinBranch ? refs?.find((r) => r.kind === 'branch' && r.name === pinBranch)?.target ?? null : null),
    [refs, pinBranch],
  );
  const { rows, laneCount } = useMemo(() => layoutGraph(visibleSnapshots, pinHead), [visibleSnapshots, pinHead]);
  const svgW = Math.max(laneCount, 1) * LANE_W;
  // Unpushed = branch ref unreachable (outside shared timeline — unsync shadow push·residue included).
  // Determined the same way as onhold (sharedReachable = parents ∪ graft_parents walk).
  const unpushed = status.unpushed;
  const uncommittedIds = status.uncommitted;

  // Graft edge identification: "lane expectation parent" set in a grafted snapshot row is maintained to the parent row,
  // allowing consistent matching of all segments (child bot·through·parent top) under the key `${lane}:${expectedHash}`.
  // Overlay graft (graft_parents present): only overlay edges are seams; the natural parent (parents[0]) remains the ordinary lineage edge.
  // Overlay edges are placed on the node lane (root graft with no parent) or branchesOut new lane. Existing destructive graft data (no graft_parents) remains parents[0].
  const seams = useMemo(() => {
    const s = new Set<string>();
    for (const r of rows) {
      if (!r.snap.grafted) continue;
      const overlay = new Set(r.snap.graft_parents ?? []);
      if (overlay.size > 0) {
        const lanes = [r.lane, ...r.branchesOut];
        for (const j of lanes) {
          const h = r.outgoing[j];
          if (h && overlay.has(h)) s.add(`${j}:${h}`);
        }
      } else {
        const p = r.snap.parents?.[0];
        if (p) s.add(`${r.lane}:${p}`);
      }
    }
    return s;
  }, [rows]);
  // Session boundary edge: matches child bot·through·parent top with the same key system (`${lane}:${expectedHash}`).
  const boundaries = useMemo(() => sessionBoundaries(visibleSnapshots), [visibleSnapshots]);
  const sessionSeams = useMemo(() => {
    const s = new Set<string>();
    for (const r of rows) {
      const p = r.snap.parents?.[0];
      if (boundaries.has(r.snap.id) && p) s.add(`${r.lane}:${p}`);
    }
    return s;
  }, [rows, boundaries]);
  // Compression boundary: nodes after context compression (same session — lineage unchanged, only node markers).
  const compactions = useMemo(() => compactionBoundaries(visibleSnapshots), [visibleSnapshots]);
  // Main lineage (union of all branch refs' first-parents) — shared nodes not here = join paths.
  // Different branches: distinguish "current trunk vs appended branch".
  const mainlines = useMemo(() => mainlinesOf(refs ?? [], visibleSnapshots), [refs, visibleSnapshots]);

  // ── Drag & Drop Reordering (join) ────────────────────────────────────────────
  // Reorder commits of branch fork (side branch) to behind the branch head.
  // Context session is tied to its git branch — no cross-branch joins. Segment/tip
  // calculations and supersede are performed by the server (join operation), while this calculation is for display and guidance.
  const join = useJoinSnapshot();
  const [dragId, setDragId] = useState<string | null>(null);
  const [dropRow, setDropRow] = useState<string | null>(null);
  const [dragHint, setDragHint] = useState<string | null>(null);
  const [joinAsk, setJoinAsk] = useState<{
    snapshot: string;
    branch: string;
    descendants: number;
    error?: string;
  } | null>(null);
  const byId = useMemo(() => new Map(snapshots.map((s) => [s.id, s])), [snapshots]);
  // Child map based on first-parent (session branch calculation — merge/graft edges exclude inheritance).
  const childrenOf = useMemo(() => {
    const m = new Map<string, string[]>();
    for (const s of snapshots) {
      const p = s.parents?.[0];
      if (p) m.set(p, [...(m.get(p) ?? []), s.id]);
    }
    return m;
  }, [snapshots]);
  type DragPlan = {
    branch: string;
    tip: string;
    droppable: Set<string>;
    descendants: number;
    reason: string | null;
  };
/** Reachable set following natural parents — "true history" of the branch (exclude graft side branches). */
  function naturalReachOf(headId: string): Set<string> {
    const seen = new Set<string>();
    const stack = [headId];
    while (stack.length > 0) {
      const cur = stack.pop() as string;
      if (seen.has(cur)) continue;
      seen.add(cur);
      for (const p of byId.get(cur)?.parents ?? []) stack.push(p);
    }
    return seen;
  }
/** Drag plan: target branch (= commit's git branch), droppable row, descendant count, reasons for infeasibility. */
  const dragPlan = useMemo<DragPlan | null>(() => {
    if (!dragId) return null;
    const src = byId.get(dragId);
    const memberships = src ? (src.branches?.length ? src.branches : [src.branch]) : [];
    const available = memberships.filter((name) =>
      (refs ?? []).some((r) => r.kind === 'branch' && r.name === name && r.name !== 'HEAD' && r.target),
    );
    // Same snapshot can have multiple git branch memberships due to content deduplication and reflog, so we do not guess the target based on the default lane or birth label. The selected branch is the explicit target, and only safe to choose when membership is unique before selection.
    const branch = joinBranch && available.includes(joinBranch)
      ? joinBranch
      : available.length === 1
        ? available[0]
        : '';
    const refB = (refs ?? []).find((r) => r.kind === 'branch' && r.name === branch && r.name !== 'HEAD' && r.target);
    if (!src || !refB) {
      return { branch, tip: dragId, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinNoBranch') };
    }
    if (refB.target === dragId) {
      return { branch, tip: dragId, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinAlreadyHead') };
    }
    if (unpushed.has(dragId)) {
      return { branch, tip: dragId, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinUnpushed') };
    }
    const branchShared = sharedReachable(
      (refs ?? []).filter(
        (r) =>
          (r.kind === 'branch' && r.name === branch) ||
          (r.kind === 'session' && r.name.startsWith(sessionRefPrefix(branch))),
      ),
      snapshots,
    );
    const foreignShared = sharedReachable(
      (refs ?? []).filter(
        (r) =>
          (r.kind === 'branch' && r.name !== branch) ||
          (r.kind === 'session' && !r.name.startsWith(sessionRefPrefix(branch))),
      ),
      snapshots,
    );
    if (!branchShared.has(dragId)) {
      return { branch, tip: dragId, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinNoTarget') };
    }
    if (foreignShared.has(dragId)) {
      return { branch, tip: dragId, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinCrossBranch') };
    }
    if (naturalReachOf(refB.target).has(dragId)) {
      // already included in natural history — no reordering (not a side branch, but the backbone).
      return { branch, tip: dragId, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinNoTarget') };
    }
    // natural path continues as long as the first-parent child is unique. SessionID is for boundary indication only; it is not a branch identity. If there are multiple children, lane selection is ambiguous, so it is blocked in this context.
    const segment = new Set<string>([dragId]);
    let tip = dragId;
    while (true) {
      const kids = (childrenOf.get(tip) ?? []).filter((id) => {
        const child = byId.get(id);
        const childBranches = child?.branches?.length ? child.branches : child ? [child.branch] : [];
        // An uncommitted hook capture is visible in its own graph layer, but it
        // is not part of the joinable commit segment.
        return child != null && !uncommittedIds.has(id) && childBranches.includes(branch);
      });
      if (kids.length === 0) break;
      if (kids.length > 1) {
        return { branch, tip, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinBranched') };
      }
      tip = kids[0];
      // like the server, X above natural descendants must also be shared commits from branch/session refs. Do not publish objects-only push tips as join segments.
      if (!branchShared.has(tip)) {
        return { branch, tip, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinUnpushed') };
      }
      if (foreignShared.has(tip)) {
        return { branch, tip, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinCrossBranch') };
      }
      if (segment.has(tip)) {
        return { branch, tip, droppable: new Set<string>(), descendants: 0, reason: t('graph.joinBranched') };
      }
      segment.add(tip);
    }
    // the actual operation appends to the current head of the selected git branch. Therefore, the drop zone is limited to the first-parent ancestry row of that branch. Opening other side branches that can be reached via grafting would lead to user confusion about selecting that branch as a merge target.
    const droppable = new Set(
      [...mainlineOf(refB.target, snapshots)].filter((id) => !segment.has(id)),
    );
    return { branch, tip, droppable, descendants: segment.size - 1, reason: null as string | null };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dragId, byId, refs, snapshots, childrenOf, joinBranch, uncommittedIds, unpushed]);
  const droppable = dragPlan?.droppable ?? new Set<string>();
  function openJoinModal(rowId: string) {
    if (!dragId || !dragPlan || dragPlan.reason || !dragPlan.droppable.has(rowId)) return;
    setJoinAsk({ snapshot: dragId, branch: dragPlan.branch, descendants: dragPlan.descendants });
  }
  function runJoin(includeDescendants: boolean) {
    if (!repoId || !joinAsk) return;
    join.mutate(
      { repoId, branch: joinAsk.branch, snapshot: joinAsk.snapshot, includeDescendants },
      {
        onSuccess: () => setJoinAsk(null),
        onError: (e) => setJoinAsk({ ...joinAsk, error: e instanceof Error ? e.message : String(e) }),
      },
    );
  }
  const joinEnabled = Boolean(repoId) && (refs?.length ?? 0) > 0;

  // when the drag plan is calculated, an immediate reason for rejection is provided (silent rejection appears as a failure).
  useEffect(() => {
    setDragHint(dragId && dragPlan ? dragPlan.reason : null);
  }, [dragId, dragPlan]);

  // Hover tooltip — render at viewport fixed coordinates, flip to left if no space on the right.
  const TIP_W = 300;
  const [tip, setTip] = useState<{ id: string; left: number; top: number } | null>(null);
  function showTip(id: string, el: HTMLElement) {
    const rect = el.getBoundingClientRect();
    const rightAnchor = rect.left + svgW + 12;
    const left =
      rightAnchor + TIP_W + 12 <= window.innerWidth
        ? rightAnchor
        : Math.max(8, rect.left - TIP_W - 12); // Flip: graph left side
    const top = Math.min(Math.max(rect.top + rect.height / 2, 48), window.innerHeight - 48);
    setTip({ id, left, top });
  }
  useEffect(() => {
    if (!tip) return;
    const clear = () => setTip(null);
    window.addEventListener('scroll', clear, true);
    return () => window.removeEventListener('scroll', clear, true);
  }, [tip]);
  const tipRow = tip ? rows.find((r) => r.snap.id === tip.id) : null;

  type LaneLabel = { text: string; archived: boolean };
  const [laneTip, setLaneTip] = useState<
    (LaneLabel & { top: number; left?: number; right?: number; below: boolean; color: string }) | null
  >(null);
  function showLaneTip(label: LaneLabel, lane: number, element: HTMLElement) {
    if (label.text.length <= 6) return;
    const rect = element.getBoundingClientRect();
    const anchorRight = rect.left > window.innerWidth / 2;
    const below = rect.top < 48;
    setLaneTip({
      ...label,
      top: below ? rect.bottom + 6 : rect.top - 6,
      left: anchorRight ? undefined : Math.max(8, rect.left),
      right: anchorRight ? Math.max(8, window.innerWidth - rect.right) : undefined,
      below,
      color: label.archived ? TICK : laneColor(lane),
    });
  }
  useEffect(() => {
    if (!laneTip) return;
    const clear = () => setLaneTip(null);
    window.addEventListener('scroll', clear, true);
    window.addEventListener('resize', clear);
    return () => {
      window.removeEventListener('scroll', clear, true);
      window.removeEventListener('resize', clear);
    };
  }, [laneTip]);

  const labelForSnapshot = useMemo(() => {
    return (snapshot: Snapshot, lane: number): LaneLabel => {
      const rowBadges = badges.get(snapshot.id) ?? [];
      const branchBadge =
        (pinBranch && lane === 0
          ? rowBadges.find((badge) => badge.kind === 'branch' && badge.name === pinBranch)
          : undefined) ?? rowBadges.find((badge) => badge.kind === 'branch');
      const archivedBadge = rowBadges.find((badge) => badge.kind === 'archived');
      return branchBadge
        ? { text: branchBadge.name, archived: false }
        : archivedBadge
          ? { text: t('graph.archivedLane', { branch: archivedBadge.name }), archived: true }
          : { text: snapshot.branch ?? '', archived: false };
    };
  }, [badges, pinBranch, t]);

  // Lane numbers are reusable after a line ends. Preserve the label belonging
  // to each active segment, then replace it when a later session reuses the
  // same lane instead of leaking the old branch name down the graph.
  const laneLabelsByRow = useMemo(() => {
    let active: (LaneLabel | null)[] = Array(laneCount).fill(null);
    if (pinHead && pinBranch && rows.some((row) => row.lane === 0 && row.snap.id === pinHead)) {
      active[0] = { text: pinBranch, archived: false };
    }
    return rows.map((row) => {
      if (active[row.lane] === null || row.incoming[row.lane] !== row.snap.id) {
        active[row.lane] = labelForSnapshot(row.snap, row.lane);
      }
      const current = [...active];
      for (const lane of row.branchesOut) {
        const parent = byId.get(row.outgoing[lane] ?? '');
        current[lane] = parent ? labelForSnapshot(parent, lane) : labelForSnapshot(row.snap, lane);
      }
      active = row.outgoing.map((target, lane) => (target ? current[lane] ?? null : null));
      return current;
    });
  }, [rows, laneCount, pinHead, pinBranch, labelForSnapshot, byId]);

  // The labels are a viewport overlay for the graph lines, not a separate
  // always-on legend. Keep only labels whose lane has a visible node/segment.
  // Horizontal clipping and movement are handled by placing the label header
  // in the same scroll canvas as the SVG rows below.
  const graphViewportRef = useRef<HTMLDivElement>(null);
  const [visibleRows, setVisibleRows] = useState<Set<number> | null>(null);
  useEffect(() => {
    const viewport = graphViewportRef.current;
    // Do not project row indices from the previous repo/layout onto this one
    // while the new observer is collecting its first intersections.
    setVisibleRows(null);
    if (!viewport || typeof IntersectionObserver === 'undefined') {
      return;
    }

    const intersectingRows = new Set<number>();
    const publish = () => {
      const next = new Set(intersectingRows);
      setVisibleRows((current) => {
        if (current && current.size === next.size && [...current].every((row) => next.has(row))) return current;
        return next;
      });
    };
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const rowIndex = Number((entry.target as HTMLElement).dataset.graphRowIndex);
          if (!Number.isInteger(rowIndex)) continue;
          if (entry.isIntersecting) intersectingRows.add(rowIndex);
          else intersectingRows.delete(rowIndex);
        }
        publish();
      },
      {
        root: viewport,
        // A row hidden behind the sticky label header is not graph-visible.
        rootMargin: `-${HEAD_H}px 0px 0px 0px`,
        threshold: 0.01,
      },
    );
    viewport.querySelectorAll<HTMLElement>('[data-graph-row-index]').forEach((row) => observer.observe(row));
    return () => observer.disconnect();
  }, [rows]);
  const laneLabels = useMemo(() => {
    const labels: (LaneLabel | null)[] = Array(laneCount).fill(null);
    const rowIndices = visibleRows === null
      ? rows.map((_row, index) => index)
      : [...visibleRows].sort((left, right) => left - right);
    for (const rowIndex of rowIndices) {
      const row = rows[rowIndex];
      if (!row) continue;
      for (const lane of occupiedLanes(row)) {
        if (labels[lane] === null) labels[lane] = laneLabelsByRow[rowIndex]?.[lane] ?? null;
      }
    }
    return labels;
  }, [rows, laneCount, laneLabelsByRow, visibleRows]);

  return (
    <div className="graph-wrap">
      <div className="graph-status" aria-label={t('graph.statusLabel')}>
        <span className="graph-status-item pushed">
          <i aria-hidden="true" /> {t('graph.pushedCount', { count: status.pushed.size })}
        </span>
        <span className="graph-status-item unpushed">
          <i aria-hidden="true" /> {t('graph.unpushedCount', { count: status.unpushed.size })}
        </span>
        <span className="graph-status-item uncommitted">
          <i aria-hidden="true" /> {t('graph.uncommittedCount', { count: status.uncommitted.size })}
        </span>
      </div>
      {status.archived.length > 0 && (
        <details className="graph-archive-panel">
          <summary title={t('graph.archivedBranchesTitle', { count: status.archivedBranches })}>
            {t('graph.archivedBranchList', { count: status.archived.length })}
          </summary>
          <ul className="graph-archive-list">
            {status.archived.map((item) => (
              <li key={`${item.branch}:${item.target}`}>
                <button
                  type="button"
                  className="graph-archive-entry"
                  disabled={!item.targetAvailable}
                  aria-label={t('graph.openArchivedBranch', { branch: item.branch })}
                  onClick={() => {
                    setShowArchived(true);
                    onSelect(item.target);
                  }}
                >
                  <span title={item.branch}>⊟ {item.branch}</span>
                  <code>{item.target.replace(/^sha256:/, '').slice(0, 10)}</code>
                  <em>
                    {!item.targetAvailable
                      ? t('graph.archivedBranchUnavailable')
                      : item.uniqueCount > 0
                        ? t('graph.archivedBranchUnique', { count: item.uniqueCount })
                        : t('graph.archivedBranchShared')}
                  </em>
                </button>
              </li>
            ))}
          </ul>
          {status.archivedOnly.size > 0 && (
            <button
              type="button"
              className="graph-archive-toggle"
              aria-pressed={archivedVisible}
              onClick={() => {
                if (archivedVisible) {
                  if (selectedArchived) {
                    const fallback = pinHead ?? snapshots.find((snapshot) => !status.archivedOnly.has(snapshot.id))?.id;
                    if (fallback) onSelect(fallback);
                  }
                  setShowArchived(false);
                } else {
                  setShowArchived(true);
                }
              }}
            >
              {archivedVisible
                ? t('graph.hideArchived', { count: status.archivedOnly.size })
                : t('graph.showArchived', { count: status.archivedOnly.size })}
            </button>
          )}
        </details>
      )}
      <div className="graph-viewport" ref={graphViewportRef}>
        <div className="graph-canvas" style={{ width: svgW }}>
          {/* Top: branch labels per currently visible track. The header and SVG rows share one scroll canvas. */}
          <div className="graph-head">
            {laneLabels.map((label, i) =>
              label?.text ? (
                <span
                  key={i}
                  data-graph-lane={i}
                  className={`graph-lane-label${label.archived ? ' archived' : ''}${label.text.length > 6 ? ' truncated' : ''}`}
                  style={{ left: cx(i), color: label.archived ? TICK : laneColor(i) }}
                  tabIndex={label.text.length > 6 ? 0 : undefined}
                  aria-label={label.text.length > 6 ? label.text : undefined}
                  onMouseEnter={(event) => showLaneTip(label, i, event.currentTarget)}
                  onMouseLeave={() => setLaneTip(null)}
                  onFocus={(event) => showLaneTip(label, i, event.currentTarget)}
                  onBlur={() => setLaneTip(null)}
                >
                  {/* Truncate after 6 characters; the unclipped portal tooltip shows the full name on hover/focus. */}
                  <span className="lane-label-short">{label.text.length > 6 ? label.text.slice(0, 6) + '…' : label.text}</span>
                </span>
              ) : null,
            )}
          </div>

          <ul className="graph">
        {rows.map((r, rowIdx) => {
          const x = cx(r.lane);
          const mid = ROW_H / 2;
          const rid = r.snap.id.replace(/^sha256:/, '').slice(0, 10); // Gradient ID (document-wide unique)
          const segs: JSX.Element[] = [];
          const defs: JSX.Element[] = [];
          if (r.incoming[r.lane] === r.snap.id) {
            // Parent (join target) row upper half: Dark→Rainbow gradient on entry.
            if (seams.has(`${r.lane}:${r.snap.id}`)) {
              defs.push(
                <linearGradient
                  key="gin"
                  id={`seam-in-${rid}`}
                  gradientUnits="userSpaceOnUse"
                  x1={x}
                  y1={0}
                  x2={x}
                  y2={mid}
                >
                  <stop offset="0%" stopColor={SEAM} />
                  <stop offset="100%" stopColor={laneColor(r.lane)} />
                </linearGradient>,
              );
              segs.push(<line key="top" x1={x} y1={0} x2={x} y2={mid} stroke={`url(#seam-in-${rid})`} strokeDasharray={SEAM_DASH} />);
            } else if (sessionSeams.has(`${r.lane}:${r.snap.id}`)) {
              segs.push(<line key="top" x1={x} y1={0} x2={x} y2={mid} stroke={laneColor(r.lane)} strokeDasharray={SESSION_DASH} />);
            } else {
              segs.push(<line key="top" x1={x} y1={0} x2={x} y2={mid} stroke={laneColor(r.lane)} />);
            }
          }
          if (r.outgoing[r.lane]) {
            // Grafted node row lower half: Rainbow→Dark gradient on exit (new context start point).
            if (seams.has(`${r.lane}:${r.outgoing[r.lane]}`)) {
              defs.push(
                <linearGradient
                  key="gout"
                  id={`seam-out-${rid}`}
                  gradientUnits="userSpaceOnUse"
                  x1={x}
                  y1={mid}
                  x2={x}
                  y2={ROW_H}
                >
                  <stop offset="0%" stopColor={laneColor(r.lane)} />
                  <stop offset="100%" stopColor={SEAM} />
                </linearGradient>,
              );
              segs.push(<line key="bot" x1={x} y1={mid} x2={x} y2={ROW_H} stroke={`url(#seam-out-${rid})`} strokeDasharray={SEAM_DASH} />);
            } else if (sessionSeams.has(`${r.lane}:${r.outgoing[r.lane]}`)) {
              // Session boundary exit: Dashed line + row bottom truncation tick (horizontal short line).
              segs.push(<line key="bot" x1={x} y1={mid} x2={x} y2={ROW_H} stroke={laneColor(r.lane)} strokeDasharray={SESSION_DASH} />);
              if (boundaries.has(r.snap.id)) {
                segs.push(<line key="tick" x1={x - 4.5} y1={ROW_H - 1} x2={x + 4.5} y2={ROW_H - 1} stroke={TICK} strokeWidth={1.4} />);
              }
            } else {
              segs.push(<line key="bot" x1={x} y1={mid} x2={x} y2={ROW_H} stroke={laneColor(r.lane)} />);
            }
          }
          for (const j of r.mergesIn) {
            const seam = seams.has(`${j}:${r.snap.id}`);
            const sess = sessionSeams.has(`${j}:${r.snap.id}`);
            segs.push(
              <path
                key={`in${j}`}
                d={`M ${cx(j)} 0 C ${cx(j)} ${mid} ${x} ${mid * 0.4} ${x} ${mid}`}
                stroke={seam ? SEAM : laneColor(j)}
                strokeDasharray={seam ? SEAM_DASH : sess ? SESSION_DASH : undefined}
                fill="none"
              />,
            );
          }
          for (const k of r.branchesOut) {
            const seamOut = seams.has(`${k}:${r.outgoing[k]}`); // Overlay graft edge exit curve
            segs.push(
              <path
                key={`out${k}`}
                d={`M ${x} ${mid} C ${cx(k)} ${mid * 1.6} ${cx(k)} ${mid} ${cx(k)} ${ROW_H}`}
                stroke={seamOut ? SEAM : laneColor(k)}
                strokeDasharray={seamOut ? SEAM_DASH : undefined}
                fill="none"
              />,
            );
          }
          for (let j = 0; j < Math.max(r.incoming.length, r.outgoing.length); j++) {
            if (j === r.lane || r.mergesIn.includes(j)) continue;
            if (r.incoming[j] && r.incoming[j] === r.outgoing[j]) {
              const seam = seams.has(`${j}:${r.incoming[j]}`);
              const sess = sessionSeams.has(`${j}:${r.incoming[j]}`);
              segs.push(
                <line
                  key={`p${j}`}
                  x1={cx(j)}
                  y1={0}
                  x2={cx(j)}
                  y2={ROW_H}
                  stroke={seam ? SEAM : laneColor(j)}
                  strokeDasharray={seam ? SEAM_DASH : sess ? SESSION_DASH : undefined}
                />,
              );
            }
          }

          const sel = r.snap.id === selectedId;
          // 3rd layer distinction: Uncommitted (hook capture, before commit) ⊂ Unreachable, so uncommitted determination takes precedence over push.
          const isUncommitted = uncommittedIds.has(r.snap.id);
          const isUnpushed = !isUncommitted && unpushed.has(r.snap.id);
          const next = rowIdx + 1 < rows.length ? rows[rowIdx + 1].snap.id : null;
          // Uncommitted block bottom boundary (commit history from next row) — exclusive truncation line.
          const uncommittedEnd = isUncommitted && next !== null && !uncommittedIds.has(next);
          // Bottom boundary of the push block — distinguished by a truncation line. Uncommitted lines also enter the unpushed set (unreachable), so "is the next line a push commit" must be determined without uncommitted lines — otherwise, uncommitted lines between would be mistaken for the truncation line.
          const nextIsUnpushedCommit = next !== null && unpushed.has(next) && !uncommittedIds.has(next);
          const blockEnd = isUnpushed && next !== null && !nextIsUnpushedCommit;
          // Join branch: shared (pushed) node but not part of any branch's mainline — light tone.
          const isSide = !isUncommitted && !isUnpushed && (refs?.length ?? 0) > 0 && !mainlines.has(r.snap.id);
          return (
            <li key={r.snap.id}>
              <button
                data-graph-row-index={rowIdx}
                data-graph-node-lane={r.lane}
                className={`graph-row${sel ? ' on' : ''}${dropRow === r.snap.id ? ' drop-target' : ''}${dragId === r.snap.id ? ' dragging' : ''}${dragId && droppable.has(r.snap.id) ? ' droppable' : ''}`}
                onClick={() => onSelect(r.snap.id)}
                onMouseEnter={(e) => showTip(r.snap.id, e.currentTarget)}
                onMouseLeave={() => setTip(null)}
                onFocus={(e) => showTip(r.snap.id, e.currentTarget)}
                onBlur={() => setTip(null)}
                aria-label={`${r.snap.message || '(no message)'} · ${isUncommitted ? t('graph.uncommitted') : isUnpushed ? t('graph.unpushed') : status.pushed.has(r.snap.id) ? t('graph.pushed') : t('graph.archivedLane', { branch: r.snap.branch })}`}
                draggable={joinEnabled && !isUncommitted}
                onDragStart={(e) => {
                  e.dataTransfer.effectAllowed = 'move';
                  e.dataTransfer.setData('text/plain', r.snap.id);
                  setTip(null);
                  setDragId(r.snap.id);
                }}
                onDragEnd={() => {
                  setDragId(null);
                  setDropRow(null);
                  setDragHint(null);
                }}
                onDragOver={(e) => {
                  if (!joinEnabled || !dragId || !droppable.has(r.snap.id)) return;
                  e.preventDefault(); // Allow drop only on valid targets
                  e.dataTransfer.dropEffect = 'move';
                  setDropRow(r.snap.id);
                }}
                onDragLeave={() => setDropRow((cur) => (cur === r.snap.id ? null : cur))}
                onDrop={(e) => {
                  e.preventDefault();
                  setDropRow(null);
                  openJoinModal(r.snap.id);
                }}
              >
                {/* Unpushed/uncommitted lines are light — visually distinguish them from the shared timeline (push pre) */}
                <svg width={svgW} height={ROW_H} className="graph-svg" aria-hidden="true" opacity={isUnpushed || isUncommitted ? 0.42 : isSide ? 0.6 : 1}>
                  {defs.length > 0 && <defs>{defs}</defs>}
                  {segs}
                  {sel && <circle cx={x} cy={mid} r={R + 3} fill="none" stroke={laneColor(r.lane)} strokeWidth={1.2} />}
                  {r.snap.grafted && (
                    <circle cx={x} cy={mid} r={R + 2.5} fill="none" stroke={SEAM} strokeWidth={1.2} strokeDasharray="2 2" />
                  )}
                  {compactions.has(r.snap.id) && (
                    <circle cx={x} cy={mid} r={R + 2.5} fill="none" stroke={COMPACT} strokeWidth={1.2} />
                  )}
                  {isUncommitted ? (
                    // Uncommitted = a hook capture not yet linked to a commit.
                    // A dotted node distinguishes durable capture state without
                    // claiming that the provider process is still alive.
                    <circle className="uncommitted-node" cx={x} cy={mid} r={R} stroke={laneColor(r.lane)} strokeWidth={1.5} strokeDasharray="2.5 2" />
                  ) : (
                    <circle cx={x} cy={mid} r={R} fill={laneColor(r.lane)} />
                  )}
                </svg>
              </button>
              {uncommittedEnd && <div className="uncommitted-divider">{t('graph.uncommittedDivider')}</div>}
              {blockEnd && <div className="unpushed-divider">{t('graph.unpushedDivider')}</div>}
            </li>
          );
        })}
        {rows.length === 0 && <li className="ws-empty">{t('graph.noCommits')}</li>}
          </ul>
        </div>
      </div>

      {dragHint && <div className="join-hint">{dragHint}</div>}

      {joinAsk && (
        <div className="modal-back" onClick={() => setJoinAsk(null)}>
          <div className="modal" role="dialog" aria-label={t('graph.joinTitle')} onClick={(e) => e.stopPropagation()}>
            <h3>{t('graph.joinTitle')}</h3>
            {joinAsk.error ? (
              <p className="join-error">{joinAsk.error}</p>
            ) : (
              <>
                <p>
                  <code>{joinAsk.snapshot.replace(/^sha256:/, '').slice(0, 10)}</code>{' '}
                  {t('graph.joinBody', { branch: joinAsk.branch })}
                </p>
                {joinAsk.descendants > 0 && <p>{t('graph.joinAsk', { count: String(joinAsk.descendants) })}</p>}
              </>
            )}
            <div className="modal-actions">
              {!joinAsk.error && joinAsk.descendants > 0 && (
                <>
                  <button className="primary" disabled={join.isPending} onClick={() => runJoin(true)}>
                    {t('graph.joinAll', { count: String(joinAsk.descendants + 1) })}
                  </button>
                  <button disabled={join.isPending} onClick={() => runJoin(false)}>
                    {t('graph.joinOnly')}
                  </button>
                </>
              )}
              {!joinAsk.error && joinAsk.descendants === 0 && (
                <button className="primary" disabled={join.isPending} onClick={() => runJoin(false)}>
                  {t('graph.joinGo')}
                </button>
              )}
              <button onClick={() => setJoinAsk(null)}>{t('common.cancel')}</button>
            </div>
          </div>
        </div>
      )}

      {tip && tipRow && (
        <div className="graph-tip" style={{ left: tip.left, top: tip.top, width: TIP_W }}>
          <code>{tipRow.snap.id.replace(/^sha256:/, '').slice(0, 10)}</code> {tipRow.snap.message || '(no message)'}
          <em>
            {tipRow.snap.author?.name || tipRow.snap.author?.email || '?'} · {tipRow.snap.branch} · {tipRow.snap.provider} ·{' '}
            {when(tipRow.snap.created_at)}
          </em>
          {tipRow.snap.grafted && <em style={{ color: '#d29922' }}>{t('graph.appended')}</em>}
          {boundaries.has(tipRow.snap.id) && <em>{t('graph.newSession')}</em>}
          {compactions.has(tipRow.snap.id) && <em style={{ color: COMPACT }}>{t('graph.compaction')}</em>}
          {(refs?.length ?? 0) > 0 &&
            !mainlines.has(tipRow.snap.id) &&
            !unpushed.has(tipRow.snap.id) &&
            !uncommittedIds.has(tipRow.snap.id) && <em style={{ color: SEAM }}>{t('graph.sideChain')}</em>}
          {uncommittedIds.has(tipRow.snap.id) ? (
            <em style={{ color: SEAM }}>{t('graph.uncommitted')}</em>
          ) : (
            unpushed.has(tipRow.snap.id) && <em style={{ color: TICK }}>{t('graph.unpushed')}</em>
          )}
          {status.pushed.has(tipRow.snap.id) && <em>{t('graph.pushed')}</em>}
          {badges.get(tipRow.snap.id)?.length ? (
            <span className="tip-badges">
              {badges.get(tipRow.snap.id)!.map((b) => (
                <span
                  key={b.kind + b.name}
                  className={`ref-badge ${b.kind}`}
                  title={b.kind === 'archived' ? t('context.archivedBranchTitle') : undefined}
                >
                  {b.kind === 'tag' ? '⌂ ' : b.kind === 'archived' ? '⊟ ' : ''}
                  {b.kind === 'archived' ? t('context.archivedBranchBadge', { branch: b.name }) : b.name}
                </span>
              ))}
            </span>
          ) : null}
        </div>
      )}
      {laneTip &&
        createPortal(
          <span
            className={`graph-lane-tip${laneTip.below ? ' below' : ''}${laneTip.archived ? ' archived' : ''}`}
            role="tooltip"
            style={{ top: laneTip.top, left: laneTip.left, right: laneTip.right, color: laneTip.color }}
          >
            {laneTip.text}
          </span>,
          document.body,
        )}
    </div>
  );
}
