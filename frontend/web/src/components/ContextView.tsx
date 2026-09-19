import {EffectiveMemory} from './EffectiveMemory';
// ContextView — GitHub repo view context browser.
// Automatically displays the latest context of the default branch (main/master),
// and provides a branch dropdown + commit log (click to show context at that point in time).
import { useEffect, useMemo, useRef, useState } from 'react';
import type { Repo, Workspace, Snapshot, Pending } from '../types';
import { useDocPages, useMemory, useMe, useFork, useSnapDiff, useSearch, useRepoView, useReflog } from '../hooks';
import { navigate, repoPath } from '../route';
import { holdCounts, reachableSnapshotIds } from '../onhold';
import { usePaged, PageControl } from './Pagination';
import { mainlineOf, sessionBoundaries, compactionBoundaries } from '../graph';
import { atLeast, canWriteAsset, type Role } from '../roles';
import { CommitGraph } from './CommitGraph';
import { ContextSelectionNotice, useContextSelection } from './ContextSelection';
import { AIBar, AIIcon, PROVIDER_META, PROVIDER_LOGOS, PROVIDER_INK, modelColor, modelLogo } from './AIBar';
import { About, TeamSettings, SecretsPanel } from './About';
import type { ViewMode } from './EventStream';
import { short, when } from '../snapshotFormat';
import { PRPromotions } from './PRPromotions';
import {GitScans} from './GitScans';
import {CodeApplicability} from './CodeApplicability';
import { GitChanges } from './GitChanges';
import { MemoryPanel } from './MemoryPanel';
import { saveBlob } from '../zip';
import { api } from '../api';
import { DocEvents } from './DocEvents';
import { useT, Rich } from '../i18n';

// Participant AI points (overlap) — a reduced version of the GitHub participant avatar stack. Overlays snapshots models[] (if any)
// on provider color circles. Continuous commit color changes soon indicate the "range of work done by which AI" intervals.
// Order rule: models[0] = direct tool (representative model — Envelope.OrderedModels places it at the front)
// → leftmost, z-order also leftmost (behind participants not claiming representation).
function AIDots({ s }: { s: Snapshot }) {
  // "<synthetic>" is a harness synthetic placeholder — it is not drawn even if it remains in the old version snapshot meta.
  const models = [...new Set((s.models ?? []).filter((m) => m !== '<synthetic>'))];
  const items = models.length
    ? models.map((m) => ({ key: m, logo: modelLogo(m), color: modelColor(m), title: m }))
    : [
        {
          key: s.provider,
          logo: PROVIDER_LOGOS[s.provider] ?? null,
          color: PROVIDER_META[s.provider]?.color ?? '#b6bcc6',
          title: s.provider,
        },
      ];
  return (
    <span className="ai-dots" title={items.map((i) => i.title).join(' · ')}>
      {items.map((i, idx) => (
        <AIIcon
          key={i.key}
          logo={i.logo}
          color={i.color}
          title={i.title}
          style={{ marginLeft: idx === 0 ? 0 : -5, position: 'relative', zIndex: items.length - idx }}
        />
      ))}
    </span>
  );
}

// Representative (direct) model — last used model (envelope.source_model, last-wins). Does not list all models participated in (that's AIDots' job).
// If the source_model in the old doc is "<synthetic>" (placeholder for a record synthesized by Claude Code harness — not a real model),
// fallback to the last model in the real model list.
function directModel(env: { source_model?: string; source_models?: string[] }): string {
  if (env.source_model && env.source_model !== '<synthetic>') return env.source_model;
  const real = (env.source_models ?? []).filter((m) => m !== '<synthetic>');
  return real[real.length - 1] ?? '';
}

// Token count notation: 1.2k / 41k / 1.3M (numbers under 1000 are displayed as is).
function tok(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 10_000) return `${Math.round(n / 1000)}k`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

// Default branch selection: repo.default_branch → main → master → first branch.
function pickDefaultBranch(names: string[], preferred: string): string | null {
  for (const c of [preferred, 'main', 'master']) {
    if (c && names.includes(c)) return c;
  }
  return names[0] ?? null;
}

type ContextWorkspace = Pick<Workspace, 'id' | 'owner_username' | 'slug' | 'visibility'> &
  Partial<Pick<Workspace, 'settings_policy' | 'secrets_policy'>>;

export function ContextView({ repo, ws, role }: { repo: Repo; ws: ContextWorkspace | null; role: Role | null }) {
  // repo derivative state (excluding refs·stash snapshots·badges·graph sources) must use the same assembly point as the On Hold tab — if input splits, badge count = tab row count guarantee is broken.
  const t = useT();
  const { refs, snapshots: allSnapshots, badges, graphSnapshots, committedSnapshots, uncommittedIds, localAhead, reflog, sharedIds, history, semantics, historyError, graphLoading, graphError, retryGraph, pendings, unsyncs } =
    useRepoView(repo.id, repo.default_branch || 'main');
  const branches = useMemo(() => refs.filter((r) => r.kind === 'branch').map((r) => r.name).sort(), [refs]);

  const [branchSelection, setBranchSelection] = useState<{ repo: string; name: string | null; id?: string }>();
  const selectedBranch = branchSelection?.repo === repo.id ? refs.find(r => r.kind === 'branch'
    && (branchSelection.id ? r.branch_id === branchSelection.id : r.name === branchSelection.name)) : undefined;
  const branch = selectedBranch?.name ?? pickDefaultBranch(branches, repo.default_branch);
  function setBranch(name: string | null) {
    setBranchSelection({ repo: repo.id, name, id: refs.find(r => r.kind === 'branch' && r.name === name)?.branch_id });
  }
  useEffect(() => {
    if (!graphLoading && !selectedBranch && branch) setBranch(branch);
  }, [repo.id, selectedBranch, branch, graphLoading]);

  // Branch log = git log <branch>: every snapshot reachable through natural or
  // graft-overlay parents. First-parent/mainline styling remains a separate
  // concern below; snapshots unreachable from the selected ref stay graph-only.
  const snapshots = useMemo(() => {
    const head = refs.find((r) => r.kind === 'branch' && r.name === branch)?.target;
    if (!head) return [];
    const reachable = reachableSnapshotIds([head], allSnapshots);
    return allSnapshots.filter((s) => reachable.has(s.id));
  }, [refs, branch, allSnapshots]);
  // Selected branch's main lineage (head's first-parent direct ancestor) — distinguishes merge branches (⎘).
  const mainline = useMemo(() => {
    const head = refs.find((r) => r.kind === 'branch' && r.name === branch)?.target;
    return mainlineOf(head, allSnapshots);
  }, [refs, branch, allSnapshots]);
  // Commit list "N per page" (default 5) — first page on branch switch.
  const pagedCommits = usePaged(snapshots, branch);
  // Session boundary (first commit made by a different agent session from the parent) — list separator.
  const boundaries = useMemo(() => sessionBoundaries(allSnapshots), [allSnapshots]);
  // Compression boundary (context compression after parent commit) — separate marker from session boundary.
  const compactions = useMemo(() => compactionBoundaries(allSnapshots), [allSnapshots]);
  // Pending context: a capture from the same session as the branch tip is its
  // uncommitted continuation. This does not imply that the provider is alive.
  // If there are unsync push commits, it's the unsync tip in the On Hold tab.
  // (Context tab shows only shared timeline — pending work is only indicated by badges). Orphan pending is also handled by On Hold.
  const sharedPendingTargets = sharedIds;
  const continuing = useMemo(() => {
    const m = new Map<string, Pending>(); // tip snapshot id → pending
    for (const p of pendings) {
      if (p.dismissed || sharedPendingTargets.has(p.target)) continue;
      const head = refs.find((r) => r.kind === 'branch' && r.name === p.branch)?.target;
      if (!head) continue;
      if (unsyncs.some((u) => u.branch === p.branch && u.target !== head)) continue; // pending commit is ahead
      const tip = allSnapshots.find((s) => s.id === head);
      if (tip?.session_id && tip.session_id === p.session_id) m.set(tip.id, p);
    }
    return m;
  }, [pendings, unsyncs, refs, allSnapshots, sharedPendingTargets]);
  // Branch-specific pending count (for tip badges) — same definition as rows in On Hold tab (onhold.ts shared).
  const holdCount = useMemo(() => holdCounts(refs, allSnapshots, unsyncs, pendings, sharedIds), [refs, allSnapshots, unsyncs, pendings, sharedIds]);
  const [snapId, setSnapId] = useState<string | null>(null);
  const { viewerRef, openSnapshot, selectedEvent } = useContextSelection(repo.id, snapId, setSnapId);
  // Auto-selection is conservative: keep current selection if it exists in the full list (user click respected),
  // otherwise set to branch head. (Orphan commits selected in the graph are also kept).
  useEffect(() => {
    setSnapId((cur) => (cur && allSnapshots.some((s) => s.id === cur) ? cur : snapshots[0]?.id ?? null));
  }, [branch, snapshots, allSnapshots]);

  // View mode — Full / Prompt only (folded) / Prompt + Response (message only).
  const [viewMode, setViewMode] = useState<ViewMode>('all');

  // Fork — Create a new branch from a selected commit (member or above, API POST /fork).
  const me = useMe().data;
  const forkMut = useFork();
  const [forkOpen, setForkOpen] = useState(false);
  const [forkName, setForkName] = useState('');

  // Diff — The commit clicked on "Compare" becomes the base, and shows the CIR event delta with the selected commit from the subsequent list (API POST /diff — doc hash pairs). Maintained until the base is cleared.
  const [compareBase, setCompareBase] = useState<Snapshot | null>(null);

  // Search — Commit metadata and indexed conversation text. Debounced by 300ms, query after, select snapshot on result click (branch agnostic — searchable across all snapshots).
  const [q, setQ] = useState('');
  const [dq, setDq] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setDq(q.trim()), 300);
    return () => clearTimeout(t);
  }, [q]);
  const searchQ = useSearch(repo.id, dq);
  const searching = dq.length >= 2;

  const selected = allSnapshots.find((s) => s.id === snapId) ?? null;
  const diffQ = useSnapDiff(
    repo.id,
    compareBase && selected && compareBase.id !== selected.id ? compareBase.doc_hash : null,
    compareBase && selected && compareBase.id !== selected.id ? selected.doc_hash : null,
  );
  const parent = useMemo(() => allSnapshots.find(s => s.id === selected?.parents?.[0]) ?? null, [allSnapshots, selected]);
  const docQ = useDocPages(repo.id, selected?.doc_hash ?? null, parent?.doc_hash);
  const page = docQ.data?.pages[0];
  const doc = page ? { cir: { envelope: page.envelope, events: page.events } } : undefined;
  const inheritedCount = page?.inherited ?? 0;
  const [inheritedOpen, setInheritedOpen] = useState(false);
  const [memoryOpen, setMemoryOpen] = useState(false);
  const [downloadError, setDownloadError] = useState('');
  const [downloading, setDownloading] = useState(false);
  useEffect(() => { setInheritedOpen(false); setMemoryOpen(false); setDownloadError(''); }, [selected?.id]);
  const memoryQ = useMemory(repo.id, selected?.memory_hash ?? null, memoryOpen);
  const memory = memoryQ.data;
  const tailPending = selected ? continuing.get(selected.id) ?? null : null;
  async function downloadRaw() {
    if (!selected) return;
    setDownloading(true); setDownloadError('');
    try {
      const full = await api.getDoc(repo.id, selected.doc_hash);
      saveBlob(new Blob([JSON.stringify(full.cir, null, 2)], { type: 'application/json' }), `${short(selected.id)}-context.json`);
    } catch (err) { setDownloadError((err as Error).message); }
    finally { setDownloading(false); }
  }

  const mainRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const element = mainRef.current;
    if (!element) return;
    let frame = 0;
    const measure = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        element.style.setProperty('--context-top', `${Math.max(68, element.getBoundingClientRect().top)}px`);
      });
    };
    measure();
    const observer = new ResizeObserver(measure);
    if (element.parentElement) observer.observe(element.parentElement);
    window.addEventListener('resize', measure);
    window.addEventListener('scroll', measure, { passive: true });
    return () => { cancelAnimationFrame(frame); observer.disconnect(); window.removeEventListener('resize', measure); window.removeEventListener('scroll', measure); };
  }, [branches.length > 0]);

  if (branches.length === 0 && !graphLoading && !graphError) {
    return <div className="empty-box"><Rich>{t('context.noContextYet')}</Rich></div>;
  }

  return (
    <div className="ctx ctx-cols">
      <div className="ctx-main" ref={mainRef} tabIndex={0} role="region" aria-label={t('dashboard.context')}>
      <div className="ctx-bar">
        <select aria-label={t('common.branch')} value={branch ?? ''} onChange={(e) => setBranch(e.target.value)}>
          {branches.map((b) => (
            <option key={b} value={b}>
              {b}
            </option>
          ))}
        </select>
        <span className="ctx-count">{t('context.branchCommits', { branch: branch ?? '', count: snapshots.length })}</span>
        <input
          className="ctx-search"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={t('context.searchPlaceholder')}
          aria-label={t('context.searchAria')}
        />
      </div>

      {searching && (
        <div className="search-results">
          {searchQ.isLoading && <div className="skel" style={{ height: 40 }} />}
          {searchQ.isError && <p className="empty-box">{t('context.searchFailed', { msg: (searchQ.error as Error).message })}</p>}
          {searchQ.data && (
            <>
              {(searchQ.data.hits ?? []).map((hit, i) => (
                <button
                  key={`${hit.snapshot_id}-${hit.kind}-${hit.seq ?? i}`}
                  className={`search-hit${hit.snapshot_id === snapId ? ' on' : ''}`}
                  onClick={() => openSnapshot(hit.snapshot_id)}
                >
                  <span className={`hit-kind ${hit.kind}`}>{hit.kind === 'commit' ? t('context.hitCommit') : hit.role || t('context.hitConvo')}</span>
                  <code>{short(hit.snapshot_id)}</code>
                  <span className="hit-snippet">{hit.snippet}</span>
                  <em>{hit.branch} · {hit.created_at.slice(0, 10)}</em>
                </button>
              ))}
              {(searchQ.data.hits ?? []).length === 0 && <p className="empty-box">{t('context.noResults')}</p>}
              {searchQ.data.truncated && <p className="hit-truncated">{t('context.searchTruncated')}</p>}
            </>
          )}
        </div>
      )}

      {snapshots.length > 0 && <PageControl paged={pagedCommits} />}
      <ul className="commits">
        {pagedCommits.visible.map((s) => (
          <li key={s.id}>
            <button
              className={`commit-row${s.id === snapId ? ' on' : ''}${mainline.has(s.id) ? '' : ' off-mainline'}`}
              onClick={() => openSnapshot(s.id)}
              onKeyDown={e => {
                if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
                  e.preventDefault();
                  e.currentTarget.querySelector('.commit-scroll')?.scrollBy({ left: e.key === 'ArrowLeft' ? -160 : 160 });
                }
              }}
            >
              <span className="commit-scroll">
              <code>{short(s.id)}</code>
              <span className="commit-msg">{s.message || t('common.noMessage')}</span>
              {!mainline.has(s.id) && (
                <span className="ref-badge seam" title={t('context.sideBadgeTitle')}>
                  ⎘ {t('context.sideBadge')}
                </span>
              )}
              {badges.get(s.id)?.map((b) => {
                // Current branch tip badge: branch name is duplicated in dropdown, so marked as "head"
                // (other branch/tag badges keep their names).
                const isHead = b.kind === 'branch' && b.name === branch;
                return (
                  <span
                    key={b.kind + b.name}
                    className={`ref-badge ${isHead ? 'head' : b.kind}`}
                    title={
                      b.kind === 'archived'
                        ? t('context.archivedBranchTitle')
                        : b.kind === 'joined'
                          ? t('context.joinedBranchTitle')
                          : undefined
                    }
                  >
                    {b.kind === 'tag' ? '⌂ ' : b.kind === 'archived' ? '⊟ ' : b.kind === 'joined' ? '⎘ ' : ''}
                    {isHead
                      ? '⌑ head'
                      : b.kind === 'archived'
                        ? t('context.archivedBranchBadge', { branch: b.name })
                        : b.kind === 'joined'
                          ? t('context.joinedBranchBadge', { branch: b.name })
                          : b.name}
                  </span>
                );
              })}
              {s.memory_hash && (
                <span className="ref-badge memory" title={t('context.memoryBadgeTitle')}>
                  ◆
                </span>
              )}
              {continuing.has(s.id) && <span className="ref-badge pending">{t('context.inProgressBadge')}</span>}
              {(() => {
                // Suspended badge counts from this line as the tip based on "branch ref name" —
                // snapshot birth labels (s.branch) may not match the actual ref after ff/fork.
                const rowHold = (badges.get(s.id) ?? [])
                  .filter((b) => b.kind === 'branch')
                  .reduce((n, b) => n + (holdCount.get(b.name) ?? 0), 0);
                return rowHold > 0 ? (
                  <span
                    className="ref-badge pending link"
                    role="link"
                    title={t('context.viewInOnHold')}
                    onClick={(e) => {
                      e.stopPropagation();
                      if (ws) navigate(repoPath(ws, repo, 'onhold'));
                    }}
                  >
                    {t('context.holdBadge', { count: rowHold })}
                  </span>
                ) : null;
              })()}
              </span>
              <span className="commit-meta">
                <AIDots s={s} />
                <em><span>{s.author?.name || s.author?.email || s.provider}</span><time dateTime={s.created_at}>{when(s.created_at)}</time></em>
              </span>
            </button>
            {/* Graft join point: this line starts a new context (does not inherit from the previous session) */}
            {s.grafted && <div className="seam-divider">{t('context.graftDivider')}</div>}
            {/* Session boundary: continues the lineage but starts a different agent session from this line */}
            {boundaries.has(s.id) && <div className="session-divider">{t('context.newSessionDivider')}</div>}
            {/* Compression boundary: same session but context window compressed at this commit */}
            {compactions.has(s.id) && <div className="compaction-divider">{t('context.compactionDivider')}</div>}
          </li>
        ))}
        {snapshots.length === 0 && <li className="ws-empty">{t('context.noCommitsBranch')}</li>}
      </ul>

      {selected && (
        <div
          className="viewer"
          ref={viewerRef}
          style={{ ['--assistant-ink' as string]: PROVIDER_INK[selected.provider] ?? 'var(--text)' } as React.CSSProperties}
        >
          <ContextSelectionNotice event={selectedEvent} />
          <div className="viewer-head">
            <code>{short(selected.id)}</code> {selected.message}
            {selected.grafted && <span className="ref-badge seam">⎘ appended</span>}
            {doc && directModel(doc.cir.envelope) && (
              <em>
                {' '}
                · {directModel(doc.cir.envelope)}
              </em>
            )}
            {Boolean(doc?.cir.envelope.context_tokens) && (
              <em>
                {' '}
                · {t('context.contextTok', { n: tok(doc!.cir.envelope.context_tokens!) })}
                {Boolean(doc!.cir.envelope.output_tokens) && <> · {t('context.outputTok', { n: tok(doc!.cir.envelope.output_tokens!) })}</>}
              </em>
            )}
            {/* Download: stores received data as-is — raw CIR / compressed memory JSON */}
            <span className="dl-btns">
              {/* Branch actions have distinct semantics: fork a shared commit at that point,
                  or use checkout -b on a local-ahead tip to move the entire chain to a new branch.
                  Intermediate local-ahead nodes cannot form a valid branch action, so hide it there. */}
              {atLeast(role, 'member') &&
                (!localAhead.ids.has(selected.id) ? (
                  <button
                    className={`dl-btn${forkOpen ? ' on' : ''}`}
                    title={t('context.forkTitle')}
                    onClick={() => setForkOpen((v) => !v)}
                  >
                    ⑂ fork
                  </button>
                ) : localAhead.tips.has(selected.id) ? (
                  <button
                    className={`dl-btn${forkOpen ? ' on' : ''}`}
                    title={t('context.branchOffTitle')}
                    onClick={() => setForkOpen((v) => !v)}
                  >
                    {t('context.branchOff')}
                  </button>
                ) : null)}
              <button
                className={`dl-btn${compareBase ? ' on' : ''}`}
                title={compareBase ? t('context.compareClear') : t('context.compareSet')}
                onClick={() => setCompareBase(compareBase ? null : selected)}
              >
                {t('context.compare')}
              </button>
              <select
                className="view-mode"
                aria-label={t('context.viewModeAria')}
                title={t('context.promptOnlyTitle')}
                value={viewMode}
                onChange={(e) => setViewMode(e.target.value as ViewMode)}
              >
                <option value="all">{t('context.viewModeAll')}</option>
                <option value="prompts">{t('common.promptOnly')}</option>
                <option value="chat">{t('context.viewModeChat')}</option>
              </select>
              {doc && (
                <button
                  className="dl-btn"
                  title={t('context.rawTitle')}
                  disabled={downloading}
                  onClick={() => void downloadRaw()}
                >
                  ↓ raw
                </button>
              )}
              {memory && (
                <button
                  className="dl-btn"
                  title={t('context.memoryTitle')}
                  onClick={() =>
                    saveBlob(
                      new Blob([JSON.stringify(memory, null, 2)], { type: 'application/json' }),
                      `${short(selected.id)}-memory.json`,
                    )
                  }
                >
                  ↓ memory
                </button>
              )}
            </span>
          </div>
          {forkOpen && (
            <div className="action-row">
              <code>{short(selected.id)}</code> {t('onhold.forkFromSuffix')}
              <input
                value={forkName}
                onChange={(e) => setForkName(e.target.value)}
                placeholder={t('common.newBranch')}
                aria-label={t('common.newBranch')}
              />
              <button
                disabled={!forkName.trim() || forkMut.isPending}
                onClick={() =>
                  forkMut.mutate(
                    {
                      repoId: repo.id,
                      from: selected.id,
                      newBranch: forkName.trim(),
                      author: { name: me?.nickname || me?.username || '', email: me?.email || '' },
                    },
                    {
                      onSuccess: (out) => {
                        setForkOpen(false);
                        setForkName('');
                        setBranch(out.branch);
                      },
                    },
                  )
                }
              >
                {forkMut.isPending ? t('common.creating') : t('context.createBranch')}
              </button>
              {forkMut.isError && <em className="err">{t('context.forkFailed')}</em>}
            </div>
          )}
          {compareBase && (
            <div className="action-row">
              {t('context.compareBaseLabel')} <code>{short(compareBase.id)}</code>
              {compareBase.id !== selected.id ? (
                <>
                  ↔ <code>{short(selected.id)}</code>
                </>
              ) : (
                <em>{t('context.comparePick')}</em>
              )}
              <button className="dl-btn close" onClick={() => setCompareBase(null)}>
                {t('common.close')} ×
              </button>
            </div>
          )}
          {compareBase && compareBase.id !== selected.id && (
            <div className="diff-panel">
              {compareBase.doc_hash === selected.doc_hash ? (
                <p className="empty-box">{t('context.diffSame')}</p>
              ) : diffQ.isLoading ? (
                <div className="skel" style={{ height: 40 }} />
              ) : (
                (() => {
                  const ch = diffQ.data?.changes ?? [];
                  if (ch.length === 0) return <p className="empty-box">{t('context.diffNoChange')}</p>;
                  return (
                    <>
                      <p className="diff-head">
                        {t('context.diffCount', { count: ch.length })} (+{ch.filter((c) => c.op === 'add').length} −
                        {ch.filter((c) => c.op === 'remove').length})
                      </p>
                      {ch.map((c, i) => (
                        <div key={i} className={`diff-line ${c.op === 'add' ? 'add' : 'del'}`}>
                          {c.op === 'add' ? '+' : '−'} [{c.seq}] {c.summary}
                        </div>
                      ))}
                    </>
                  );
                })()
              )}
            </div>
          )}
          {downloadError && <p role="alert" className="err">{downloadError}</p>}
          {selected.memory_hash && <MemoryPanel memory={memory} open={memoryOpen} onToggle={setMemoryOpen}>
            {memoryOpen && memoryQ.isLoading && <div className="skel" style={{ height: 60 }} />}
            {memoryOpen && memoryQ.isError && <p role="alert" className="err">{memoryQ.error.message} <button onClick={() => void memoryQ.refetch()}>{t('context.retryRead')}</button></p>}
          </MemoryPanel>}
          <EffectiveMemory key={`effective:${repo.id}:${selected.id}`} repoId={repo.id} snapshotId={selected.id} memoryHash={selected.memory_hash} />
          {doc && inheritedCount > 0 && (
            <details className="inherited-block" open={inheritedOpen} onToggle={e => setInheritedOpen(e.currentTarget.open)}>
              <summary>↰ {t('context.inherited', { count: inheritedCount })} {parent && t('context.inheritedFrom', { hash: short(parent.id) })}</summary>
              {inheritedOpen && <DocEvents repoId={repo.id} hash={selected.doc_hash} start={0} end={inheritedCount} mode={viewMode} />}
            </details>
          )}
          <DocEvents key={`main-${selected.id}`} repoId={repo.id} hash={selected.doc_hash} base={parent?.doc_hash} mode={viewMode} />
          {page && page.total > 0 && page.total === inheritedCount && <p className="empty-box">{t('context.allInherited')}</p>}
          {page?.total === 0 && <p className="empty-box">{t('context.noEvents')}</p>}
          {doc && tailPending && <>
            <div className="session-divider pending-divider">{t('onhold.continuingConvo', { when: when(tailPending.updated_at) })}</div>
            <DocEvents repoId={repo.id} hash={tailPending.target} base={selected.doc_hash} mode={viewMode} />
          </>}

        </div>
      )}
      </div>

      {/* Right rail: About → team settings → secrets → commit graph → AI participants. */}
      <aside className="ctx-side">
        <PRPromotions repoId={repo.id} canRetry={atLeast(role, 'member')} />
        <CodeApplicability key={`code-state:${repo.id}`} repoId={repo.id} />
        <GitScans key={`git-scans:${repo.id}`} repoId={repo.id} canRetry={atLeast(role, 'member')} />
        <GitChanges key={`git-changes:${repo.id}`} repoId={repo.id} canRetry={atLeast(role, 'member')} />
        <About repo={repo} canEdit={canWriteAsset(role, undefined)} />
        {atLeast(role, 'puller') && (
          <TeamSettings
            repoId={repo.id}
            canWrite={canWriteAsset(role, ws?.settings_policy)}
            showLockedControl={ws?.visibility === 'public'}
          />
        )}
        {atLeast(role, 'puller') && (
          <SecretsPanel
            key={repo.id}
            repoId={repo.id}
            canWrite={canWriteAsset(role, ws?.secrets_policy)}
            showLockedControl={ws?.visibility === 'public'}
          />
        )}
        <span className="label">{t('common.commitGraphTotal', { count: committedSnapshots.length })}</span>
        <CommitGraph snapshots={graphSnapshots} selectedId={snapId} selectedEventId={selectedEvent?.id} onSelect={openSnapshot} badges={badges} refs={refs} reflog={reflog} history={history} semantics={semantics} historyError={historyError} graphLoading={graphLoading} graphError={graphError} retryGraph={retryGraph} uncommitted={uncommittedIds} pinBranch={repo.default_branch || 'main'} joinBranch={branch ?? undefined} repoId={atLeast(role, 'member') ? repo.id : null} />
        <ReflogPanel repoId={repo.id} />
        <AIBar snapshots={committedSnapshots} />
      </aside>
    </div>
  );
}

// ReflogPanel — ref movement log (equivalent to git reflog). It is collapsible and queried only while open (audit finding #10).
function ReflogPanel({ repoId }: { repoId: string }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const q = useReflog(repoId, open);
  return (
    <details className="reflog" onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary className="label">{t('context.reflog')}</summary>
      <ul className="reflog-list">
        {(q.data ?? []).slice(0, 30).map((e, i) => (
          <li key={i}>
            <em>{(e.created_at ?? '').slice(5, 16).replace('T', ' ')}</em> <code>{e.name}</code>{' '}
            <code>{e.old ? short(e.old) : '∅'}</code>→<code>{short(e.new)}</code>
          </li>
        ))}
        {open && !q.isLoading && (q.data ?? []).length === 0 && <li className="ws-empty">{t('context.reflogEmpty')}</li>}
      </ul>
    </details>
  );
}
