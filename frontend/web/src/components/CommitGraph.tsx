// CommitGraph — GitHub network graph style commit tree.
// Text-free pure graph: lane colors, top branch labels, node tooltips on hover, viewer integration on click.
// Lane layout is handled in graph.ts (pure function), this file renders only the SVG.
import { useEffect, useMemo, useState } from 'react';
import type { Ref, Snapshot } from '../types';
import { layoutGraph, mainlineOf, mainlinesOf, sessionBoundaries, compactionBoundaries } from '../graph';
import { sharedReachable } from '../onhold';
import { useJoinSnapshot } from '../hooks';
import { useT } from '../i18n';

const LANE_W = 22; // Lane width
const ROW_H = 26; // Row height (text-free — compact)
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
  const pinHead = useMemo(
    () => (pinBranch ? refs?.find((r) => r.kind === 'branch' && r.name === pinBranch)?.target ?? null : null),
    [refs, pinBranch],
  );
  const { rows, laneCount } = useMemo(() => layoutGraph(snapshots, pinHead), [snapshots, pinHead]);
  const svgW = Math.max(laneCount, 1) * LANE_W;
  // Unpushed = branch ref unreachable (outside shared timeline — unsync shadow push·residue included).
  // Determined the same way as onhold (sharedReachable = parents ∪ graft_parents walk).
  const unpushed = useMemo(() => {
    if (!refs || refs.length === 0) return new Set<string>();
    const shared = sharedReachable(refs, snapshots);
    return new Set(snapshots.filter((s) => !shared.has(s.id)).map((s) => s.id));
  }, [refs, snapshots]);

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
  const boundaries = useMemo(() => sessionBoundaries(snapshots), [snapshots]);
  const sessionSeams = useMemo(() => {
    const s = new Set<string>();
    for (const r of rows) {
      const p = r.snap.parents?.[0];
      if (boundaries.has(r.snap.id) && p) s.add(`${r.lane}:${p}`);
    }
    return s;
  }, [rows, boundaries]);
  // Compression boundary: nodes after context compression (same session — lineage unchanged, only node markers).
  const compactions = useMemo(() => compactionBoundaries(snapshots), [snapshots]);
  // Main lineage (union of all branch refs' first-parents) — shared nodes not here = join paths.
  // Different branches: distinguish "current trunk vs appended branch".
  const mainlines = useMemo(() => mainlinesOf(refs ?? [], snapshots), [refs, snapshots]);

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
        return child != null && !(uncommitted?.has(id) ?? false) && childBranches.includes(branch);
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
  }, [dragId, byId, refs, snapshots, childrenOf, joinBranch, uncommitted, unpushed]);
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

  // Track label: topmost (latest) node of each track — ref badge (branch name) first, then snapshot label.
  // Pin track (default branch) has fixed label — append promotion after main ref points to the snapshot of the feature birth label, making the left track read as main.
  const laneLabels = useMemo(() => {
    type LaneLabel = { text: string; archived: boolean };
    const labels: (LaneLabel | null)[] = Array(laneCount).fill(null);
    if (pinHead && pinBranch && rows.some((r) => r.lane === 0 && r.snap.id === pinHead)) {
      labels[0] = { text: pinBranch, archived: false };
    }
    for (const r of rows) {
      if (labels[r.lane] !== null) continue;
      const rowBadges = badges.get(r.snap.id) ?? [];
      const branchBadge =
        (pinBranch ? rowBadges.find((b) => b.kind === 'branch' && b.name === pinBranch && r.lane === 0) : undefined) ??
        rowBadges.find((b) => b.kind === 'branch');
      const archivedBadge = rowBadges.find((b) => b.kind === 'archived');
      labels[r.lane] = branchBadge
        ? { text: branchBadge.name, archived: false }
        : archivedBadge
          ? { text: t('graph.archivedLane', { branch: archivedBadge.name }), archived: true }
          : { text: r.snap.branch ?? '', archived: false };
    }
    return labels;
  }, [rows, laneCount, badges, pinHead, pinBranch, t]);

  return (
    <div className="graph-wrap">
      {/* Top: branch labels per track (track color, tilt — to prevent overlap) */}
      <div className="graph-head" style={{ width: svgW }}>
        {laneLabels.map((label, i) =>
          label?.text ? (
            <span
              key={i}
              className={`graph-lane-label${label.archived ? ' archived' : ''}${label.text.length > 6 ? ' truncated' : ''}`}
              style={{ left: cx(i), color: label.archived ? TICK : laneColor(i) }}
            >
              {/* Truncate after 6 characters (to prevent overflow) — show full name as background chip on hover */}
              <span className="lane-label-short">{label.text.length > 6 ? label.text.slice(0, 6) + '…' : label.text}</span>
              {label.text.length > 6 && <span className="lane-label-full">{label.text}</span>}
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
                <linearGradient key="gin" id={`seam-in-${rid}`} x1="0" y1="0" x2="0" y2="1">
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
                <linearGradient key="gout" id={`seam-out-${rid}`} x1="0" y1="0" x2="0" y2="1">
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
          const isUncommitted = uncommitted?.has(r.snap.id) ?? false;
          const isUnpushed = !isUncommitted && unpushed.has(r.snap.id);
          const next = rowIdx + 1 < rows.length ? rows[rowIdx + 1].snap.id : null;
          // Uncommitted block bottom boundary (commit history from next row) — exclusive truncation line.
          const uncommittedEnd = isUncommitted && next !== null && !(uncommitted?.has(next) ?? false);
          // Bottom boundary of the push block — distinguished by a truncation line. Uncommitted lines also enter the unpushed set (unreachable), so "is the next line a push commit" must be determined without uncommitted lines — otherwise, uncommitted lines between would be mistaken for the truncation line.
          const nextIsUnpushedCommit = next !== null && unpushed.has(next) && !(uncommitted?.has(next) ?? false);
          const blockEnd = isUnpushed && next !== null && !nextIsUnpushedCommit;
          // Join branch: shared (pushed) node but not part of any branch's mainline — light tone.
          const isSide = !isUncommitted && !isUnpushed && (refs?.length ?? 0) > 0 && !mainlines.has(r.snap.id);
          return (
            <li key={r.snap.id}>
              <button
                className={`graph-row${sel ? ' on' : ''}${dropRow === r.snap.id ? ' drop-target' : ''}${dragId === r.snap.id ? ' dragging' : ''}${dragId && droppable.has(r.snap.id) ? ' droppable' : ''}`}
                onClick={() => onSelect(r.snap.id)}
                onMouseEnter={(e) => showTip(r.snap.id, e.currentTarget)}
                onMouseLeave={() => setTip(null)}
                onFocus={(e) => showTip(r.snap.id, e.currentTarget)}
                onBlur={() => setTip(null)}
                aria-label={r.snap.message}
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
            !(uncommitted?.has(tipRow.snap.id) ?? false) && <em style={{ color: SEAM }}>{t('graph.sideChain')}</em>}
          {uncommitted?.has(tipRow.snap.id) ? (
            <em style={{ color: SEAM }}>{t('graph.uncommitted')}</em>
          ) : (
            unpushed.has(tipRow.snap.id) && <em style={{ color: TICK }}>{t('graph.unpushed')}</em>
          )}
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
    </div>
  );
}
