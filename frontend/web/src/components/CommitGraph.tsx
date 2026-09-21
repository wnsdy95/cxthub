// CommitGraph — GitHub network graph style commit tree.
// Text-free pure graph: lane colors, top branch labels, node tooltips on hover, viewer integration on click.
// Lane layout is handled in graph.ts (pure function), this file renders only the SVG.
import { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { ContextSemantics, GraphState, HistoryEvent, Ref, RefLogEntry, Snapshot } from '../types';
import { layoutGraph, mainlinesOf, sessionBoundaries, compactionBoundaries } from '../graph';
import { projectBranchGraph, visibleBranchGraph, type GraphEvent } from '../graphProjection';
import { completedBranchEvidence } from '../graphEvidence';
import { GraphIndex } from '../graphIndex';
import { graphStatus, graphProgress } from '../graphState';
import { hiddenProgressIds } from '../contextHistory';
import { useGraphPosition, useJoinPreview, useJoinSnapshot } from '../hooks';
import { useT } from '../i18n';

const LANE_W = 22; // Lane width
const ROW_H = 26; // Row height (text-free — compact)
const HEAD_H = 44; // Sticky lane-label area; kept in sync with .graph-head.
const R = 4.5; // Node radius
const EMPTY_HISTORY: HistoryEvent[] = [];

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
const LIFECYCLE_DASH = '6 3';

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


function when(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

export function CommitGraph({
  snapshots,
  selectedId,
  selectedEventId,
  onSelect,
  badges,
  refs,

  history = EMPTY_HISTORY,
  semantics,
  historyError = false,
  graphLoading = false,
  graphError,
  retryGraph,
  graphState,
  pinBranch,
  joinBranch,
  repoId,
  readRepoId,
}: {
  snapshots: Snapshot[];
  selectedId: string | null;
  selectedEventId?: string;
  onSelect: (id: string, event?: GraphEvent) => void;
  badges: Map<string, { name: string; kind: string }[]>;
/** Branch ref list. If present, unpushed commits outside the shared timeline are lightened and separated by a tear line. */
  refs?: Ref[];
/** Server ref movements; missing evidence never creates a historical path. */
  reflog?: RefLogEntry[];
  history?: HistoryEvent[];
  semantics?: ContextSemantics;
  historyError?: boolean;
  graphLoading?: boolean;
  graphError?: string;
  retryGraph?: () => void;
/** Uncommitted hook-capture IDs, rendered as hollow dashed nodes with an individual label. */
  uncommitted?: Set<string>;
  graphState?: GraphState;
/** Default branch name — always fixed at the leftmost lane (0) for this branch chain. */
  pinBranch?: string;
/** Join target git branch — fixed to the current branch to avoid guessing multiple memberships */
  joinBranch?: string;
/** repo ID — activates drag-and-drop join if provided */
  repoId?: string | null;
  readRepoId?: string;
}) {
  const t = useT();
  const graphIndex = useMemo(() => new GraphIndex(snapshots), [snapshots]);
  const mergeEvidence = useMemo(() => completedBranchEvidence(snapshots, history, graphIndex, semantics), [snapshots, history, graphIndex, semantics]);
  const [showArchived, setShowArchived] = useState(false);
  const [expandedHistory, setExpandedHistory] = useState<Set<string>>(new Set());
  const [positionId, setPositionId] = useState('');
  const positions = useMemo(() => (graphState?.positions ?? []).flatMap(p => {
    const event=history.find(e=>e.id===p.event_id);
    return event ? [{...event,branch:p.branch,archived:p.archived}] : [];
  }),[graphState,history]);
  const positionEvent = positions.find(e=>e.id===positionId);
  const selectedPosition = useGraphPosition(readRepoId ?? repoId,positionEvent?.id ?? '',graphState?.revision);
  const currentGraph = positionEvent ? selectedPosition.data : graphState;
  const position = positionEvent && currentGraph ? {branch:positionEvent.branch,snapshot:positionEvent.target!,archived:positionEvent.archived} : undefined;
  const historyGroups = useMemo(()=>graphProgress(currentGraph),[currentGraph]);
  // A selection made elsewhere in the viewer must reveal its recorded path.
  const expandedKeys = useMemo(() => {
    const next = new Set(expandedHistory);
    if (selectedId && hiddenProgressIds(historyGroups, next).has(selectedId)) {
      const group = historyGroups.find((item) => item.before === selectedId)
        ?? historyGroups.find((item) => item.collapsibleIds.has(selectedId));
      if (group) next.add(group.key);
    }
    return next;
  }, [expandedHistory, historyGroups, selectedId]);
  const hiddenHistory = useMemo(() => hiddenProgressIds(historyGroups, expandedKeys), [historyGroups, expandedKeys]);
  const revealedHistory = useMemo(() => new Set(historyGroups
    .filter((group) => expandedKeys.has(group.key)).flatMap((group) => [...group.snapshotIds])), [historyGroups, expandedKeys]);
  const status = useMemo(()=>graphStatus(currentGraph ?? graphState),[currentGraph,graphState]);
  const selectedArchived = selectedId !== null && status.archivedOnly.has(selectedId);
  const archivedVisible = showArchived || selectedArchived;
  const visibleSnapshots = useMemo(
    () => snapshots.filter((snapshot) => !hiddenHistory.has(snapshot.id)
      && (archivedVisible || !status.archivedOnly.has(snapshot.id) || revealedHistory.has(snapshot.id))),
    [archivedVisible, snapshots, status.archivedOnly, hiddenHistory, revealedHistory],
  );
  const graphIdentity = refs?.[0]?.repo_id ?? repoId ?? '';
  useEffect(() => { setShowArchived(false); setExpandedHistory(new Set()); setPositionId(''); }, [graphIdentity]);
  const pinHead = useMemo(
    () => position && !position.archived && position.branch === pinBranch ? position.snapshot : (pinBranch ? refs?.find((r) => r.kind === 'branch' && r.name === pinBranch)?.target ?? null : null),
    [refs, pinBranch, position?.branch, position?.snapshot, position?.archived],
  );
  const fullProjection = useMemo(() => projectBranchGraph(snapshots, refs ?? [], currentGraph, pinHead, pinBranch), [snapshots, refs, currentGraph, pinHead, pinBranch]);
  // Validate before folding too: hiding a bad component must not hide its error.
  const projectedIndex = useMemo(() => new GraphIndex(fullProjection.snapshots), [fullProjection]);
  const projection = useMemo(() => visibleBranchGraph(fullProjection, new Set(visibleSnapshots.map(s => s.id))), [fullProjection, visibleSnapshots]);
  const graphIssues = graphIndex.issues.length ? graphIndex.issues : projectedIndex.issues;
  const { rows, laneCount } = useMemo(() => graphError || graphIssues.length || (positionEvent && !currentGraph) ? { rows: [], laneCount: 0 } : layoutGraph(projection.snapshots, projection.pinHead), [projection, graphIssues, positionEvent, currentGraph, graphError]);
  const missingParents = graphIndex.missingParents;
  function toggleHistory(key: string) {
    const next = new Set(expandedKeys);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    const hidden = hiddenProgressIds(historyGroups, next);
    if (selectedId && hidden.has(selectedId)) {
      const fallback = pinHead ?? visibleSnapshots.find((snapshot) => !hidden.has(snapshot.id))?.id;
      if (fallback) onSelect(fallback);
    }
    setExpandedHistory(next);
  }
  const svgW = Math.max(laneCount, 1) * LANE_W;
  // Publication tiers come from the same server projection used by On Hold.
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
  const lifecycleSegments = useMemo(() => {
    const segments = new Set<string>();
    for (const row of rows) {
      const targets = projection.lifecycleEdges.get(row.snap.id);
      for (const lane of row.branchesOut) {
        const target = row.outgoing[lane];
        if (target && targets?.has(target)) segments.add(`${lane}:${target}`);
      }
    }
    return segments;
  }, [rows, projection]);
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
  const mainlines = useMemo(() => mainlinesOf(fullProjection.refs, fullProjection.snapshots), [fullProjection]);

  // Only the server decides join scope and eligibility. React keeps interaction state.
  const join = useJoinSnapshot();
  const [dragId, setDragId] = useState<string | null>(null);
  const [dropRow, setDropRow] = useState<string | null>(null);
  const [joinAsk, setJoinAsk] = useState<{snapshot:string; branch?:string; dropTarget?:string; error?:string} | null>(null);
  const preview = useJoinPreview(repoId, joinAsk?.snapshot ?? dragId, joinAsk?.branch ?? joinBranch);
  const plan = preview.data;
  const reasonText = (reason: import('../types').JoinPreview['reason']) => {
    switch (reason) {
      case 'branch_required': case 'no_branch': return t('graph.joinNoBranch');
      case 'already_head': return t('graph.joinAlreadyHead');
      case 'unpushed': case 'uncommitted': return t('graph.joinUnpushed');
      case 'cross_branch': return t('graph.joinCrossBranch');
      case 'branched': return t('graph.joinBranched');
      case 'natural_history': return t('graph.joinNoTarget');
      default: return null;
    }
  };
  const droppable = new Set(plan?.drop_targets ?? []);
  const rejectedDrop = Boolean(joinAsk?.dropTarget && plan && !plan.reason && !droppable.has(joinAsk.dropTarget));
  const dragHint = dragId ? preview.isFetching ? t('graph.joinLoading')
    : preview.error ? t('graph.joinUnavailable') : reasonText(plan?.reason) : null;
  const joinEnabled = !graphIssues.length && Boolean(repoId);
  function openJoinModal(rowId: string) {
    if (!dragId) return;
    setJoinAsk({snapshot:dragId, branch:plan?.branch || joinBranch, dropTarget:rowId});
  }
  function runJoin(includeDescendants: boolean) {
    if (!repoId || !joinAsk || !plan || plan.reason || rejectedDrop || preview.isFetching || joinAsk.error || graphIssues.length) return;
    join.mutate({repoId, preview:plan, includeDescendants}, {
      onSuccess: () => setJoinAsk(null),
      onError: (e) => setJoinAsk(current => current ? {...current,error:e.message} : null),
    });
  }
  async function refreshJoin() {
    const result = await preview.refetch();
    if (!result.error) setJoinAsk(current => current ? {...current,error:undefined} : null);
  }

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
      const graphEvent = projection.events.get(snapshot.id);
      if (graphEvent) return { text: graphEvent.branch, archived: false };
      const rowBadges = badges.get(snapshot.id) ?? [];
      const branchBadge =
        (pinBranch && lane === 0
          ? rowBadges.find((badge) => badge.kind === 'branch' && badge.name === pinBranch)
          : undefined) ?? rowBadges.find((badge) => badge.kind === 'branch');
      const archivedBadge = rowBadges.find((badge) => badge.kind === 'archived');
      const joinedBadge = rowBadges.find((badge) => badge.kind === 'joined');
      return branchBadge
        ? { text: branchBadge.name, archived: false }
        : joinedBadge
          ? { text: joinedBadge.name, archived: false }
          : archivedBadge
            ? { text: t('graph.archivedLane', { branch: archivedBadge.name }), archived: true }
            : { text: snapshot.branch ?? '', archived: false };
    };
  }, [badges, pinBranch, t, projection]);

  // Lane numbers are reusable after a line ends. Preserve the label belonging
  // to each active segment, then replace it when a later session reuses the
  // same lane instead of leaking the old branch name down the graph.
  const laneLabelsByRow = useMemo(() => {
    let active: (LaneLabel | null)[] = Array(laneCount).fill(null);
    if (pinHead && pinBranch && rows.some((row) => row.lane === 0 && row.snap.id === projection.pinHead)) {
      active[0] = { text: pinBranch, archived: false };
    }
    return rows.map((row) => {
      if (active[row.lane] === null || row.incoming[row.lane] !== row.snap.id) {
        active[row.lane] = labelForSnapshot(row.snap, row.lane);
      }
      const current = [...active];
      for (const lane of row.branchesOut) {
        const parent = projectedIndex.byId.get(row.outgoing[lane] ?? '');
        const edgeBranch = projection.edgeBranches.get(row.snap.id)?.get(row.outgoing[lane] ?? '');
        current[lane] = edgeBranch
          ? { text: edgeBranch, archived: false }
          : parent ? labelForSnapshot(parent, lane) : labelForSnapshot(row.snap, lane);
      }
      active = row.outgoing.map((target, lane) => (target ? current[lane] ?? null : null));
      return current;
    });
  }, [rows, laneCount, pinHead, pinBranch, labelForSnapshot, projectedIndex, projection]);

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
      {graphError ? <p role="alert" className="graph-history-error">{t('graph.loadFailed')} {graphError} {retryGraph && <button onClick={retryGraph}>{t('context.retryRead')}</button>}</p>
        : historyError && <p role="status" className="graph-history-error">{t('graph.historyUnavailable')}</p>}
      {graphIssues.length > 0 && <div role="alert" className="graph-history-error graph-invalid">
        <p>{t('graph.invalidStructure')}</p>
        {graphIssues.map(issue => <p key={issue.kind} data-graph-issue={issue.kind}>
          {t(issue.kind === 'duplicate-id' ? 'graph.duplicateIds' : 'graph.cycleDetected', { count: issue.count })}
          {' '}<code>{issue.ids.join(', ')}{issue.count > issue.ids.length ? ' …' : ''}</code>
        </p>)}
        {retryGraph && <button type="button" onClick={retryGraph}>{t('context.retryRead')}</button>}
      </div>}
      {!graphIssues.length && projection.foldedParents.size > 0 && <p role="status" className="graph-folded-edges">{t('graph.foldedConnections', { count: projection.foldedParents.size })}</p>}
      {graphLoading && <p role="status">{t('graph.loading')}</p>}
      {missingParents.size > 0 && <p role="status" className="graph-history-error">{t('graph.missingParents', { count: missingParents.size })}</p>}
      {mergeEvidence.length > 0 && <details className="graph-history-panel graph-merge-records">
        <summary>{t('graph.mergeRecords', { count: mergeEvidence.length })}</summary>
        <ul className="graph-history-list">
          {mergeEvidence.map(({ merge, birth, lineage, placementIntact, sourceAvailable }) => {
            const event: GraphEvent = { id: `graph:merge:${merge.id}`, kind: 'merge', branch: merge.branch,
              sourceBranch: merge.pr!.head_branch, snapshot: merge.source ?? '', evidence: merge.id, prNumber: merge.pr!.number };
            return <li key={merge.id} data-branch-lineage={lineage}>
              <strong>{merge.pr!.head_branch} → {merge.branch} · PR #{merge.pr!.number}</strong>
              <span>{t('graph.mergeVerified')}</span>
              {!placementIntact && <span>{t('graph.historicalPlacement')}</span>}
              <span>{t(`graph.lineage_${lineage}`)}</span>
              <div className="graph-history-actions">
                <button type="button" disabled={!sourceAvailable} onClick={() => onSelect(event.snapshot, event)}>{t('graph.viewMergeSource')}</button>
                {birth?.source && graphIndex.byId.has(birth.source) && <button type="button"
                  onClick={() => onSelect(birth.source!, { id: `graph:birth:${birth.id}`, kind: 'birth', branch: birth.branch, snapshot: birth.source!, evidence: birth.id })}>
                  {t('graph.viewBranchSource')}
                </button>}
              </div>
            </li>;
          })}
        </ul>
      </details>}
      {positionEvent && !currentGraph && selectedPosition.isPending && <p role="status">{t('graph.loading')}</p>}
      {selectedPosition.error && <p role="alert">{selectedPosition.error.message} <button onClick={() => { void selectedPosition.refetch(); }}>{t('context.retryRead')}</button></p>}
      {positions.length > 0 && <div className="graph-history-scope">
        <label>{t('graph.browsePosition')}
          <select aria-label={t('graph.browsePosition')} value={positionEvent?.id ?? ''} onChange={(event) => {
            const next = positions.find((item) => item.id === event.target.value);
            setPositionId(next?.id ?? '');
            setExpandedHistory(new Set());
            const target = next?.target ?? refs?.find((ref) => ref.kind === 'branch' && ref.name === pinBranch)?.target;
            if (target) onSelect(target);
          }}>
            <option value="">{t('graph.serverBranchPositions')}</option>
            {positions.map((event) => <option key={event.id} value={event.id}>
              {event.branch || 'HEAD'} · {event.git_after?.slice(0, 7) || event.target?.replace(/^sha256:/, '').slice(0, 7)} · {when(event.created_at)} · {event.worktree_id?.slice(0, 6)}
            </option>)}
          </select>
        </label>
        {positionEvent && <span>{t('graph.browsePositionHint')}</span>}
      </div>}
      {history.some((event) => event.kind === 'birth' || event.kind === 'attach' || event.kind === 'orphan' || event.kind === 'rename' || event.kind === 'archive') &&
        <details className="graph-history-panel graph-births">
          <summary>{t('graph.branchOperations')}</summary>
          <ul className="graph-history-list">
            {history.filter((event) => event.kind === 'birth' || event.kind === 'attach' || event.kind === 'orphan' || event.kind === 'rename' || event.kind === 'archive').slice().reverse().map((event) => <li key={event.id}>
              <span className="graph-history-branch" title={`${event.branch} · ${event.branch_id}`}>{event.kind === 'rename' ? `${event.previous_branch} → ${event.branch}` : event.local_branch && event.local_branch !== event.branch ? `${event.local_branch} → ${event.branch}` : event.branch}</span>
              <span>{event.kind === 'rename' ? t('graph.branchRenamed') : event.kind === 'archive' ? t('graph.branchArchived') : event.kind === 'orphan' ? t('graph.orphanBirth') : event.kind === 'attach' ? t('graph.branchAttached') : t('graph.branchBorn')}</span>
              <time dateTime={event.created_at}>{when(event.created_at)}</time>
              {event.target && graphIndex.byId.has(event.target) && <div className="graph-history-actions"><button type="button" className="graph-history-view" onClick={() => onSelect(event.target!)}>
                {t('graph.viewBranchSource')} · {event.target.replace(/^sha256:/, '').slice(0, 7)}
              </button></div>}
              {event.kind === 'orphan' && event.memory_hash && <span>{t('graph.inheritedProjectMemory')}</span>}
            </li>)}
          </ul>
        </details>}
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
        {status.tagged.size > 0 && <span className="graph-status-item tagged">{t('graph.taggedCount', { count: status.tagged.size })}</span>}
      </div>
      {historyGroups.length > 0 && (
        <section className="graph-history-panel" aria-label={t('graph.previousProgress')}>
          <div className="graph-history-heading">{t('graph.previousProgress')} · {t('graph.previousProgressCount', {
            count: new Set(historyGroups.flatMap((group) => [...group.snapshotIds])).size,
          })}</div>
          <ul className="graph-history-list">
            {historyGroups.map((group) => (
              <li key={group.key}>
                <span className="graph-history-branch" title={group.branch}>{group.branch}</span>
                <span className="graph-history-position" title={`${group.before} → ${group.after}`}>
                  <code>{group.before.replace(/^sha256:/, '').slice(0, 7)}</code> → <code>{group.after.replace(/^sha256:/, '').slice(0, 7)}</code>
                </span>
                <time dateTime={group.createdAt}>{when(group.createdAt)}</time>
                <div className="graph-history-actions">
                  {group.collapsibleIds.size > 0 ? (
                    <button type="button" className="graph-history-toggle" aria-expanded={expandedKeys.has(group.key)}
                      onClick={() => toggleHistory(group.key)}>
                      {expandedKeys.has(group.key)
                        ? t('graph.collapsePrevious', { count: group.collapsibleIds.size })
                        : t('graph.expandPrevious', { count: group.collapsibleIds.size })}
                    </button>
                  ) : <span>{t('graph.previousProgressShared')}</span>}
                  <button type="button" className="graph-history-view" onClick={() => {
                    setExpandedHistory(new Set([...expandedKeys, group.key]));
                    onSelect(group.before);
                  }}>{t('graph.viewPrevious')}</button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}
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
                    const fallback = pinHead ?? visibleSnapshots.find((snapshot) => !status.archivedOnly.has(snapshot.id))?.id;
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
      {projection.lifecycleEdges.size > 0 && <p className="graph-lifecycle-legend">{t('graph.lifecycleLine')}</p>}
      <div className="graph-viewport" ref={graphViewportRef}>
        <div className="graph-canvas" style={{ width: svgW + (uncommittedIds.size ? 100 : 0) }}>
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
            if (lifecycleSegments.has(`${r.lane}:${r.snap.id}`)) {
              segs.push(<line key="top" data-graph-edge="lifecycle" x1={x} y1={0} x2={x} y2={mid} stroke={laneColor(r.lane)} strokeDasharray={LIFECYCLE_DASH} />);
            } else if (seams.has(`${r.lane}:${r.snap.id}`)) {
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
            const lifecycle = lifecycleSegments.has(`${j}:${r.snap.id}`);
            segs.push(
              <path
                key={`in${j}`}
                data-graph-edge={lifecycle ? 'lifecycle' : undefined}
                d={`M ${cx(j)} 0 C ${cx(j)} ${mid} ${x} ${mid * 0.4} ${x} ${mid}`}
                stroke={seam ? SEAM : laneColor(j)}
                strokeDasharray={lifecycle ? LIFECYCLE_DASH : seam ? SEAM_DASH : sess ? SESSION_DASH : undefined}
                fill="none"
              />,
            );
          }
          for (const k of r.branchesOut) {
            const seamOut = seams.has(`${k}:${r.outgoing[k]}`); // Overlay graft edge exit curve
            const lifecycle = lifecycleSegments.has(`${k}:${r.outgoing[k]}`);
            segs.push(
              <path
                key={`out${k}`}
                data-graph-edge={lifecycle ? 'lifecycle' : undefined}
                data-graph-parent={r.outgoing[k]}
                d={`M ${x} ${mid} C ${cx(k)} ${mid * 1.6} ${cx(k)} ${mid} ${cx(k)} ${ROW_H}`}
                stroke={seamOut ? SEAM : laneColor(k)}
                strokeDasharray={lifecycle ? LIFECYCLE_DASH : seamOut ? SEAM_DASH : undefined}
                fill="none"
              />,
            );
          }
          for (let j = 0; j < Math.max(r.incoming.length, r.outgoing.length); j++) {
            if (j === r.lane || r.mergesIn.includes(j)) continue;
            if (r.incoming[j] && r.incoming[j] === r.outgoing[j]) {
              const seam = seams.has(`${j}:${r.incoming[j]}`);
              const sess = sessionSeams.has(`${j}:${r.incoming[j]}`);
              const lifecycle = lifecycleSegments.has(`${j}:${r.incoming[j]}`);
              segs.push(
                <line
                  key={`p${j}`}
                  data-graph-edge={lifecycle ? 'lifecycle' : undefined}
                  x1={cx(j)}
                  y1={0}
                  x2={cx(j)}
                  y2={ROW_H}
                  stroke={seam ? SEAM : laneColor(j)}
                  strokeDasharray={lifecycle ? LIFECYCLE_DASH : seam ? SEAM_DASH : sess ? SESSION_DASH : undefined}
                />,
              );
            }
          }

          const graphEvent = projection.events.get(r.snap.id);
          const selectId = graphEvent?.snapshot ?? r.snap.id;
          const sel = selectedEventId ? r.snap.id === selectedEventId && selectId === selectedId : !graphEvent && r.snap.id === selectedId;
          // 3rd layer distinction: Uncommitted (hook capture, before commit) ⊂ Unreachable, so uncommitted determination takes precedence over push.
          const isUncommitted = uncommittedIds.has(r.snap.id);
          const isUnpushed = !isUncommitted && unpushed.has(r.snap.id);
          const next = rowIdx + 1 < rows.length ? rows[rowIdx + 1].snap.id : null;
          // Bottom boundary of the push block — distinguished by a truncation line. Uncommitted lines also enter the unpushed set (unreachable), so "is the next line a push commit" must be determined without uncommitted lines — otherwise, uncommitted lines between would be mistaken for the truncation line.
          const nextIsUnpushedCommit = next !== null && unpushed.has(next) && !uncommittedIds.has(next);
          const blockEnd = isUnpushed && next !== null && !nextIsUnpushedCommit;
          // Join branch: shared (pushed) node but not part of any branch's mainline — light tone.
          const isSide = !graphEvent && !isUncommitted && !isUnpushed && (refs?.length ?? 0) > 0 && !mainlines.has(r.snap.id);
          return (
            <li key={r.snap.id}>
              <button
                data-graph-row-index={rowIdx}
                data-graph-id={r.snap.id}
                data-graph-node-lane={r.lane}
                data-graph-event={graphEvent?.kind}
                data-graph-snapshot={selectId}
                aria-pressed={sel}
                data-graph-branch={graphEvent?.branch ?? r.snap.branch}
                className={`graph-row${sel ? ' on' : ''}${dropRow === r.snap.id ? ' drop-target' : ''}${dragId === r.snap.id ? ' dragging' : ''}${dragId && droppable.has(r.snap.id) ? ' droppable' : ''}`}
                onContextMenu={(e) => { if (joinEnabled && !graphEvent) { e.preventDefault(); setJoinAsk({snapshot:r.snap.id,branch:joinBranch}); } }}
                onClick={() => onSelect(selectId, graphEvent)}
                onMouseEnter={(e) => showTip(r.snap.id, e.currentTarget)}
                onMouseLeave={() => setTip(null)}
                onFocus={(e) => showTip(r.snap.id, e.currentTarget)}
                onBlur={() => setTip(null)}
                aria-label={graphEvent ? `${graphEvent.branch} · ${graphEvent.kind === 'birth' ? t(graphEvent.orphan ? 'graph.orphanBirth' : 'graph.branchBorn') : t('graph.branchMerged', { branch: graphEvent.sourceBranch ?? '' })}` : `${r.snap.message || '(no message)'} · ${isUncommitted ? t('graph.uncommitted') : isUnpushed ? t('graph.unpushed') : status.tagged.has(r.snap.id) ? t('graph.tagged') : status.pushed.has(r.snap.id) ? t('graph.pushed') : t('graph.archivedLane', { branch: r.snap.branch })}`}
                draggable={joinEnabled && !isUncommitted && !graphEvent}
                onDragStart={(e) => {
                  e.dataTransfer.effectAllowed = 'move';
                  e.dataTransfer.setData('text/plain', r.snap.id);
                  setTip(null);
                  setDragId(r.snap.id);
                }}
                onDragEnd={() => {
                  setDragId(null);
                  setDropRow(null);
                }}
                onDragOver={(e) => {
                  if (!joinEnabled || !dragId || graphEvent) return;
                  if (!preview.isFetching && !preview.error && plan?.reason !== 'branch_required' && !droppable.has(r.snap.id)) return;
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
                {/* Pending is a node state; through-lines may belong to published branches. */}
                <svg width={svgW} height={ROW_H} className="graph-svg" aria-hidden="true" opacity={isUnpushed ? 0.42 : isSide ? 0.6 : 1}>
                  {defs.length > 0 && <defs>{defs}</defs>}
                  {segs}
                  {sel && <circle cx={x} cy={mid} r={R + 3} fill="none" stroke={laneColor(r.lane)} strokeWidth={1.2} />}
                  {r.snap.grafted && (
                    <circle cx={x} cy={mid} r={R + 2.5} fill="none" stroke={SEAM} strokeWidth={1.2} strokeDasharray="2 2" />
                  )}
                  {compactions.has(r.snap.id) && (
                    <circle cx={x} cy={mid} r={R + 2.5} fill="none" stroke={COMPACT} strokeWidth={1.2} />
                  )}
                  {graphEvent ? (
                    <rect className="branch-event-node" x={x-R} y={mid-R} width={R*2} height={R*2} transform={`rotate(45 ${x} ${mid})`} fill={graphEvent.kind === 'merge' ? laneColor(r.lane) : 'var(--bg)'} stroke={laneColor(r.lane)} strokeWidth={1.5} />
                  ) : isUncommitted ? (
                    // Uncommitted = a hook capture not yet linked to a commit.
                    // A dotted node distinguishes durable capture state without
                    // claiming that the provider process is still alive.
                    <circle className="uncommitted-node" cx={x} cy={mid} r={R} stroke="#9a6a00" strokeWidth={1.5} strokeDasharray="2.5 2" />
                  ) : (
                    <circle cx={x} cy={mid} r={R} fill={laneColor(r.lane)} />
                  )}
                </svg>
                {isUncommitted && <span className="graph-uncommitted-badge" style={{ left: (Math.max(...occupiedLanes(r)) + 1) * LANE_W + 12 }} aria-hidden="true">{t('graph.uncommittedLabel')}</span>}
              </button>
              {blockEnd && <div className="graph-status-divider" data-graph-divider={r.snap.id}>
                <svg width={svgW} height={20} className="graph-svg" aria-hidden="true">
                  {r.outgoing.map((target, lane) => {
                    if (!target) return null;
                    const key = `${lane}:${target}`;
                    const lifecycle = lifecycleSegments.has(key);
                    const seam = seams.has(key);
                    return <line key={lane} data-graph-edge={lifecycle ? 'lifecycle' : undefined}
                      x1={cx(lane)} y1={0} x2={cx(lane)} y2={20}
                      stroke={seam ? SEAM : laneColor(lane)}
                      strokeDasharray={lifecycle ? LIFECYCLE_DASH : seam ? SEAM_DASH : sessionSeams.has(key) ? SESSION_DASH : undefined} />;
                  })}
                </svg>
                <div className="unpushed-divider">
                  {t('graph.unpushedDivider')}
                </div>
              </div>}
            </li>
          );
        })}
        {rows.length === 0 && !graphLoading && !graphError && !historyError && !(positionEvent && !currentGraph) && <li className="ws-empty">{t('graph.noCommits')}</li>}
          </ul>
        </div>
      </div>

      {dragHint && <div className="join-hint">{dragHint}</div>}

      {joinAsk && (
        <div className="modal-back" onClick={() => !join.isPending && setJoinAsk(null)}>
          <div className="modal" role="dialog" aria-label={t('graph.joinTitle')} onClick={(e) => e.stopPropagation()}>
            <h3>{t('graph.joinTitle')}</h3>
            {preview.isFetching ? <p role="status">{t('graph.joinLoading')}</p> : preview.error ? <p role="alert">{t('graph.joinUnavailable')}</p> : plan && <>
              {plan.branches.length > 1 && <select aria-label={t('graph.joinChooseBranch')} value={joinAsk.branch ?? ''}
                disabled={join.isPending} onChange={e => setJoinAsk({...joinAsk, branch:e.target.value,error:undefined})}>
                <option value="">{t('graph.joinChooseBranch')}</option>
                {plan.branches.map(b => <option key={b.branch} value={b.branch}>{b.branch}</option>)}
              </select>}
              {plan.reason || rejectedDrop ? <p role="status">{rejectedDrop ? t('graph.joinNoTarget') : reasonText(plan.reason)}</p> : <>
                <p><code>{plan.snapshot.replace(/^sha256:/, '').slice(0,10)}</code>{' '}{t('graph.joinBody',{branch:plan.branch})}</p>
                <p>{t('graph.joinHead')}: <code>{plan.expected_head?.replace(/^sha256:/,'').slice(0,10)}</code></p>
                {plan.descendants > 0 && <p>{t('graph.joinAsk',{count:String(plan.descendants)})}</p>}
              </>}
            </>}
            {joinAsk.error && <p className="join-error" role="alert">{joinAsk.error} {t('graph.joinReviewChanged')}</p>}
            <div className="modal-actions">
              {(joinAsk.error || preview.error) ? <button disabled={preview.isFetching} onClick={() => void refreshJoin()}>{t('graph.joinRefresh')}</button>
                : plan && !plan.reason && !rejectedDrop && !preview.isFetching && <>
                  {plan.descendants > 0 && <button className="primary" disabled={join.isPending || graphIssues.length > 0} onClick={() => runJoin(true)}>{t('graph.joinAll',{count:String(plan.descendants+1)})}</button>}
                  <button disabled={join.isPending || graphIssues.length > 0} onClick={() => runJoin(false)}>{t(plan.descendants ? 'graph.joinOnly' : 'graph.joinGo')}</button>
                </>}
              <button disabled={join.isPending} onClick={() => setJoinAsk(null)}>{t('common.cancel')}</button>
            </div>
          </div>
        </div>
      )}

      {tip && tipRow && (
        <div className="graph-tip" style={{ left: tip.left, top: tip.top, width: TIP_W }}>
          {projection.events.has(tipRow.snap.id) ? <strong>{tipRow.snap.branch} · {projection.events.get(tipRow.snap.id)!.kind === 'birth'
            ? t(projection.events.get(tipRow.snap.id)!.orphan ? 'graph.orphanBirth' : 'graph.branchBorn')
            : t('graph.branchMerged', { branch: projection.events.get(tipRow.snap.id)!.sourceBranch ?? '' })}</strong>
            : <><code>{tipRow.snap.id.replace(/^sha256:/, '').slice(0, 10)}</code> {tipRow.snap.message || '(no message)'}</>}
          <em>
            {tipRow.snap.author?.name || tipRow.snap.author?.email || '?'} · {tipRow.snap.branch} · {tipRow.snap.provider} ·{' '}
            {when(tipRow.snap.created_at)}
          </em>
          {projection.events.has(tipRow.snap.id) && <em>{t('graph.eventOpensSnapshot')}
            {projection.events.get(tipRow.snap.id)?.prNumber ? ` · PR #${projection.events.get(tipRow.snap.id)!.prNumber}` : ''}</em>}
          {(projection.lifecycleEdges.has(tipRow.snap.id) || [...projection.lifecycleEdges.values()].some(ids => ids.has(tipRow.snap.id)))
            && <em>{t('graph.lifecycleLine')}</em>}
          {tipRow.snap.grafted && <em style={{ color: '#d29922' }}>{t('graph.appended')}</em>}
          {boundaries.has(tipRow.snap.id) && <em>{t('graph.newSession')}</em>}
          {compactions.has(tipRow.snap.id) && <em style={{ color: COMPACT }}>{t('graph.compaction')}</em>}
          {!projection.events.has(tipRow.snap.id) && (refs?.length ?? 0) > 0 &&
            !mainlines.has(tipRow.snap.id) &&
            !unpushed.has(tipRow.snap.id) &&
            !uncommittedIds.has(tipRow.snap.id) && <em style={{ color: SEAM }}>{t('graph.sideChain')}</em>}
          {uncommittedIds.has(tipRow.snap.id) ? (
            <em style={{ color: SEAM }}>{t('graph.uncommitted')}</em>
          ) : (
            unpushed.has(tipRow.snap.id) && <em style={{ color: TICK }}>{t('graph.unpushed')}</em>
          )}
          {status.tagged.has(tipRow.snap.id) && <em>{t('graph.tagged')}</em>}
          {status.pushed.has(tipRow.snap.id) && <em>{t('graph.pushed')}</em>}
          {badges.get(tipRow.snap.id)?.length ? (
            <span className="tip-badges">
              {badges.get(tipRow.snap.id)!.map((b) => (
                <span
                  key={b.kind + b.name}
                  className={`ref-badge ${b.kind}`}
                  title={
                    b.kind === 'archived'
                      ? t('context.archivedBranchTitle')
                      : b.kind === 'joined'
                        ? t('context.joinedBranchTitle')
                        : undefined
          }
                >
                  {b.kind === 'tag' ? '⌂ ' : b.kind === 'archived' ? '⊟ ' : b.kind === 'joined' ? '⎘ ' : ''}
                  {b.kind === 'archived'
                    ? t('context.archivedBranchBadge', { branch: b.name })
                    : b.kind === 'joined'
                      ? t('context.joinedBranchBadge', { branch: b.name })
                      : b.name}
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
