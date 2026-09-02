// ContextView — GitHub repo view context browser.
// Automatically displays the latest context of the default branch (main/master),
// and provides a branch dropdown + commit log (click to show context at that point in time).
import { useEffect, useMemo, useState } from 'react';
import type { Repo, Workspace, CIREvent, Snapshot, Pending } from '../types';
import { useDoc, useMemory, useMe, useFork, useSnapDiff, useSearch, usePendings, useUnsyncs, useRepoView, useReflog } from '../hooks';
import { navigate, wsPath } from '../route';
import { holdCounts, reachableSnapshotIds, sharedReachable } from '../onhold';
import { usePaged, PageControl } from './Pagination';
import { mainlineOf, sessionBoundaries, compactionBoundaries } from '../graph';
import { atLeast, canWriteAsset, type Role } from '../roles';
import { CommitGraph } from './CommitGraph';
import { AIBar, AIIcon, PROVIDER_META, PROVIDER_LOGOS, PROVIDER_INK, modelColor, modelLogo } from './AIBar';
import { About, TeamSettings, SecretsPanel } from './About';
import { Markdown } from './Markdown';
import { MemoryPanel } from './MemoryPanel';
import { saveBlob } from '../zip';
import { useT, Rich } from '../i18n';

export function short(hash: string): string {
  return hash.replace(/^sha256:/, '').slice(0, 10);
}

export function when(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

// commonEventPrefix counts how many events in two CIR event streams are the same from the beginning.
// Since consecutive sessions inherit the parent doc as the prefix (continuity resolution),
// the same logic was duplicated in three places (ContextView tailStart·inheritedCount, OnHoldView tailStart).
export function commonEventPrefix(a: CIREvent[], b: CIREvent[]): number {
  let n = 0;
  while (n < a.length && n < b.length && JSON.stringify(a[n]) === JSON.stringify(b[n])) n++;
  return n;
}

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
  const { refs, snapshots: allSnapshots, badges, graphSnapshots, committedSnapshots, uncommittedIds, localAhead } =
    useRepoView(repo.id, repo.default_branch || 'main');
  const branches = useMemo(() => refs.filter((r) => r.kind === 'branch').map((r) => r.name).sort(), [refs]);

  const [branch, setBranch] = useState<string | null>(null);
  useEffect(() => {
    setBranch(pickDefaultBranch(branches, repo.default_branch));
  }, [repo.id, repo.default_branch, branches]);

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
  const pendings = usePendings(repo.id).data ?? [];
  const unsyncs = useUnsyncs(repo.id).data ?? [];
  const sharedPendingTargets = useMemo(() => sharedReachable(refs, allSnapshots), [refs, allSnapshots]);
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
  const holdCount = useMemo(() => holdCounts(refs, allSnapshots, unsyncs, pendings), [refs, allSnapshots, unsyncs, pendings]);
  const [snapId, setSnapId] = useState<string | null>(null);
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

  // Search — Commit message/author + conversation body (server scan). Debounced by 300ms, query after, select snapshot on result click (branch agnostic — searchable across all snapshots).
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
  const docQ = useDoc(repo.id, selected?.doc_hash ?? null);
  const doc = docQ.data;
  const memory = useMemory(repo.id, selected?.id ?? null, Boolean(selected?.memory_hash)).data;
  // Continuation tail: If the selected commit is the tip preceding an
  // uncommitted capture, render only the events after that commit.
  const tailPending = selected ? continuing.get(selected.id) ?? null : null;
  const tailDoc = useDoc(repo.id, tailPending?.target ?? null).data;
  const tailStart = useMemo(() => {
    if (!doc || !tailDoc) return 0;
    return commonEventPrefix(tailDoc.cir.events, doc.cir.events);
  }, [doc, tailDoc]);

  // Context continuation determination — The CIR of the continuation session includes the parent doc as a prefix, so it hides sections that match from the beginning of the parent doc and events (similar UX to collapsing email quotes). Only the first parent is considered (most snapshots have a single parent; merges are treated as new events safely).
  const parent = useMemo(() => {
    const pid = selected?.parents?.[0];
    return pid ? allSnapshots.find((s) => s.id === pid) ?? null : null;
  }, [selected, allSnapshots]);
  const parentDoc = useDoc(repo.id, parent?.doc_hash ?? null).data;
  const inheritedCount = useMemo(() => {
    if (!doc || !parentDoc) return 0;
    return commonEventPrefix(doc.cir.events, parentDoc.cir.events);
  }, [doc, parentDoc]);

  if (branches.length === 0) {
    return <div className="empty-box"><Rich>{t('context.noContextYet')}</Rich></div>;
  }

  return (
    <div className="ctx ctx-cols">
      <div className="ctx-main">
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
                  onClick={() => setSnapId(hit.snapshot_id)}
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
              onClick={() => setSnapId(s.id)}
            >
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
                      if (ws) navigate(wsPath(ws, 'onhold'));
                    }}
                  >
                    {t('context.holdBadge', { count: rowHold })}
                  </span>
                ) : null;
              })()}
              <AIDots s={s} />
              <em>
                {s.author?.name || s.author?.email || s.provider} · {when(s.created_at)}
              </em>
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
          style={{ ['--assistant-ink' as string]: PROVIDER_INK[selected.provider] ?? 'var(--text)' } as React.CSSProperties}
        >
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
                  onClick={() =>
                    saveBlob(
                      new Blob([JSON.stringify(doc.cir, null, 2)], { type: 'application/json' }),
                      `${short(selected.id)}-context.json`,
                    )
                  }
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
          {memory && <MemoryPanel memory={memory} />}
          {docQ.isLoading && <div className="skel" style={{ height: 60 }} />}
          {doc && inheritedCount > 0 && (
            <details className="inherited-block">
              <summary>
                ↰ {t('context.inherited', { count: inheritedCount })}
                {parent && (
                  <>
                    {' '}
                    {t('context.inheritedFrom', { hash: short(parent.id) })}
                  </>
                )}
              </summary>
              <EventStream key={`inh-${selected.id}`} events={doc.cir.events.slice(0, inheritedCount)} mode={viewMode} />
            </details>
          )}
          {doc && (
            <EventStream
              key={`main-${selected.id}`}
              events={doc.cir.events.slice(inheritedCount)}
              offset={inheritedCount}
              mode={viewMode}
            />
          )}
          {doc && doc.cir.events.length > 0 && doc.cir.events.length === inheritedCount && (
            <p className="empty-box">{t('context.allInherited')}</p>
          )}
          {doc && doc.cir.events.length === 0 && <p className="empty-box">{t('context.noEvents')}</p>}
          {/* Continuing pending conversation from the same session; the next commit will absorb it. */}
          {doc && tailPending && tailDoc && tailDoc.cir.events.length > tailStart && (
            <>
              <div className="session-divider pending-divider">
                {t('onhold.continuingConvo', { when: when(tailPending.updated_at) })}
              </div>
              <EventStream
                key={`tail-${selected.id}-${tailPending.target}`}
                events={tailDoc.cir.events.slice(tailStart)}
                offset={tailStart}
                mode={viewMode}
              />
            </>
          )}
        </div>
      )}
      </div>

      {/* Right rail: About → team settings → secrets → commit graph → AI participants. */}
      <aside className="ctx-side">
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
            repoId={repo.id}
            canWrite={canWriteAsset(role, ws?.secrets_policy)}
            showLockedControl={ws?.visibility === 'public'}
          />
        )}
        <span className="label">{t('common.commitGraphTotal', { count: committedSnapshots.length })}</span>
        <CommitGraph snapshots={graphSnapshots} selectedId={snapId} onSelect={setSnapId} badges={badges} refs={refs} uncommitted={uncommittedIds} pinBranch={repo.default_branch || 'main'} joinBranch={branch ?? undefined} repoId={atLeast(role, 'member') ? repo.id : null} />
        <ReflogPanel repoId={repo.id} />
        <AIBar snapshots={committedSnapshots} />
      </aside>
    </div>
  );
}

const CXT_SEED_PREFIXES = ['[cxt seed] Branch-switch context:', '[cxt] This session was resumed from a branch context seed.'];

function isCompactSummaryEvent(ev: CIREvent): boolean {
  if (ev.kind !== 'message') return false;
  if (ev.compact_summary) return true;
  const text = (ev.blocks ?? []).map((b) => b.text).join('\n').trimStart();
  return CXT_SEED_PREFIXES.some((prefix) => text.startsWith(prefix));
}

// Prompt-only view hides assistant, tool, and reasoning events while retaining user prompts.
// Clicking a prompt expands every AI event in that turn until the next prompt; clicking again
// collapses it. Expansion state resets for each snapshot key.
function isPrompt(ev: CIREvent): boolean {
  // Distillation is for role=user only — not a human prompt — do not use as turn head.
  return ev.kind === 'message' && ev.role === 'user' && !isCompactSummaryEvent(ev);
}

// chatGroups — group turn body into "hidden work (pre: tools·reasoning) + result message" units.
// In prompt+response mode, append ▸ AI N (pre count) to response lines to expand response units.
function chatGroups(body: { ev: CIREvent; idx: number }[]): { pre: { ev: CIREvent; idx: number }[]; msg: { ev: CIREvent; idx: number } | null }[] {
  const out: { pre: { ev: CIREvent; idx: number }[]; msg: { ev: CIREvent; idx: number } | null }[] = [];
  let pre: { ev: CIREvent; idx: number }[] = [];
  for (const item of body) {
    if (item.ev.kind === 'message') {
      out.push({ pre, msg: item });
      pre = [];
    } else {
      pre.push(item);
    }
  }
  if (pre.length > 0) out.push({ pre, msg: null });
  return out;
}

// Non-rendered events (encryption reasoning without summary — ReasoningRow returns null) are excluded from turn body — the number of rows shown when expanded should match the "AI N" count.
function isRendered(ev: CIREvent): boolean {
  return !(ev.kind === 'reasoning' && !(ev.redacted_summary ?? '').trim());
}

// View mode: all = all events / prompts = prompts only (fold turns) / chat = prompts+assistant messages only (hide tools·results·reasoning — read conversation flow only).
export type ViewMode = 'all' | 'prompts' | 'chat';

export function EventStream({ events, offset = 0, mode }: { events: CIREvent[]; offset?: number; mode: ViewMode }) {
  const tr = useT();
  const [open, setOpen] = useState<Set<number>>(new Set());
  if (mode === 'all') {
    return (
      <>
        {events.map((ev, i) => (
          <EventRow key={offset + i} ev={ev} />
        ))}
      </>
    );
  }

  // Turn splitting: prompt as head, body up to next prompt. Events before the first prompt are grouped into a leading turn (headIdx=-1) with no head, shown as a collapsible row.
  type Turn = { head: CIREvent | null; headIdx: number; body: { ev: CIREvent; idx: number }[] };
  const turns: Turn[] = [];
  let cur: Turn = { head: null, headIdx: -1, body: [] };
  events.forEach((ev, i) => {
    if (isPrompt(ev)) {
      turns.push(cur);
      cur = { head: ev, headIdx: i, body: [] };
    } else if (isRendered(ev)) {
      cur.body.push({ ev, idx: i });
    }
  });
  turns.push(cur);

  const toggle = (k: number) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });

  return (
    <>
      {turns.map((t) => {
        if (!t.head && t.body.length === 0) return null; // Empty leading turn
        const opened = open.has(t.headIdx);
        const body = t.body;
        // chat mode: the entire conversation (prompt+response) is shown from the beginning (user confirmed) —
        // only the hidden work (tools·reasoning) per response is collapsed. No turn-based collapsing.
        return (
          <div key={t.headIdx} className={`turn${opened ? ' open' : ''}`}>
            {t.head ? (
              mode === 'chat' ? (
                <EventRow ev={t.head} />
              ) : (
                <div
                  className={`turn-head${body.length > 0 ? ' has-more' : ''}`}
                  onClick={body.length > 0 ? () => toggle(t.headIdx) : undefined}
                  title={body.length > 0 ? `${tr('context.aiEvents', { count: body.length })} ${opened ? tr('common.collapse') : tr('common.expand')}` : undefined}
                >
                  <EventRow ev={t.head} />
                  {body.length > 0 && (
                    <span className="turn-count">
                      {opened ? '▾' : '▸'} AI {body.length}
                    </span>
                  )}
                </div>
              )
            ) : mode === 'chat' ? null : (
              <button className="turn-lead" onClick={() => toggle(t.headIdx)}>
                {opened ? '▾' : '▸'} {tr('context.leadingEvents', { count: body.length })}
              </button>
            )}
            {(mode === 'chat' || opened) &&
              (mode === 'chat'
                ? // chat: collapses the hidden work (tools·reasoning) that made each response into ▸ AI N.
                  chatGroups(t.body).map((g, gi) => {
                    const key = g.msg ? g.msg.idx : t.headIdx * 100000 + gi + 1;
                    const subOpened = open.has(-1000 - key); // negative space without colliding with turn key
                    return (
                      <div key={`g${key}`}>
                        {/* time-based rendering (user confirmed): work (tools·reasoning) appears before the response
                            so the expanded content is above the response — same reading order as in full mode.
                            vertical guide (│) + indentation to show that the following response is part of the same group.
                            For interrupted or ongoing tail work with no answer, keep the anchor above
                            and do not imply a conclusion that does not exist. */}
                        {subOpened && g.msg && (
                          <div className="asst-work">
                            {g.pre.map(({ ev, idx }) => (
                              <EventRow key={offset + idx} ev={ev} />
                            ))}
                          </div>
                        )}
                        {g.msg ? (
                          <div
                            className={`turn-head${g.pre.length > 0 ? ' has-more' : ''}`}
                            onClick={g.pre.length > 0 ? () => toggle(-1000 - key) : undefined}
                            title={g.pre.length > 0 ? `${tr('context.aiEvents', { count: g.pre.length })} ${subOpened ? tr('common.collapse') : tr('common.expand')}` : undefined}
                          >
                            <EventRow ev={g.msg.ev} />
                            {g.pre.length > 0 && (
                              <span className="turn-count">
                                {subOpened ? '▾' : '▸'} AI {g.pre.length}
                              </span>
                            )}
                          </div>
                        ) : (
                          g.pre.length > 0 && (
                            <button className="turn-lead" onClick={() => toggle(-1000 - key)}>
                              {subOpened ? '▾' : '▸'} AI {g.pre.length}
                            </button>
                          )
                        )}
                        {subOpened && !g.msg && (
                          <div className="asst-work">
                            {g.pre.map(({ ev, idx }) => (
                              <EventRow key={offset + idx} ev={ev} />
                            ))}
                          </div>
                        )}
                      </div>
                    );
                  })
                : body.map(({ ev, idx }) => <EventRow key={offset + idx} ev={ev} />))}
          </div>
        );
      })}
    </>
  );
}

// Event line: role label + block (text is body), tool_call is tool-specific details
// (Edit=diff, Write=file content, Bash=command), tool_result/reasoning is collapsible.
// In CIR, the original tool input/output is preserved, so rendering is handled (truncation is for display — data is complete).
function EventRow({ ev }: { ev: CIREvent }) {
  const t = useT();
  if (ev.kind === 'tool_call') return <ToolCallRow ev={ev} />;
  if (ev.kind === 'tool_result') return <ToolResultRow ev={ev} />;
  if (ev.kind === 'reasoning') return <ReasoningRow ev={ev} />;
  if (ev.kind === 'compaction') return <div className="compaction-divider">{t('context.compactionDivider')}</div>;
  if (isCompactSummaryEvent(ev)) return <CompactSummaryRow ev={ev} />;
  if (ev.kind !== 'message' || !ev.blocks?.length) {
    return <div className="msg meta">[{ev.kind}]</div>;
  }
  const roleLabel = ev.agent_message && ev.agent_author ? ev.agent_author : ev.role;
  return (
    <div className={`msg ${ev.role ?? ''}`}>
      <span className="msg-role">{roleLabel}</span>
      <div className="msg-body">
        {ev.blocks.map((b, i) =>
          b.type === 'text' ? (
            // Conversation body (user prompt·assistant answer) is also marked down in the same way as compressed memory
            // rendering — agent answers are usually in titles/lists/codefences, so plain text <p> can degrade readability.
            // Markdown is safe for React element creation (no raw HTML).
            <Markdown key={i} text={b.text ?? ''} />
          ) : (
            <p key={i} className="msg-block">
              [{b.type}
              {b.name ? `: ${b.name}` : ''}]
            </p>
          ),
        )}
      </div>
    </div>
  );
}

const MAX_DIFF_LINES = 40; // display limit; the complete source remains preserved in CIR
const str = (v: unknown): string => (typeof v === 'string' ? v : '');

// mdSlice — truncates text to render in markdown. If the truncation point is inside ``` fences,
// an unfinished fence would encompass the entire following content in <pre>, so it closes odd-numbered fences to balance.
function mdSlice(text: string, n: number): string {
  const cut = text.slice(0, n);
  const fences = (cut.match(/^\s*```/gm) ?? []).length;
  return fences % 2 === 1 ? cut + '\n```' : cut;
}

// File relative path (absolute path outside repo root is last segments only).
function shortPath(p: string): string {
  const seg = p.split('/');
  return seg.length > 4 ? '…/' + seg.slice(-3).join('/') : p;
}

// Diff line render (±prefix). kind: 'del' | 'add'
function DiffLines({ text, sign }: { text: string; sign: 'add' | 'del' }) {
  const t = useT();
  const lines = text.split('\n');
  const shown = lines.slice(0, MAX_DIFF_LINES);
  return (
    <>
      {shown.map((l, i) => (
        <div key={i} className={`diff-line ${sign}`}>
          {sign === 'add' ? '+' : '−'} {l}
        </div>
      ))}
      {lines.length > MAX_DIFF_LINES && <div className="diff-line more">… {t('context.moreLines', { count: lines.length - MAX_DIFF_LINES })}</div>}
    </>
  );
}

// ToolCallRow — Tool header + body. Edit/Write shows code modification logs as diff.
function ToolCallRow({ ev }: { ev: CIREvent }) {
  const t = useT();
  const name = ev.provider_tool_name || ev.tool_name || 'tool';
  const input = ev.input ?? {};
  const filePath = str(input.file_path);
  const headArg = filePath ? shortPath(filePath) : str(input.command) || str(input.pattern) || str(input.url) || '';

  let body: JSX.Element | null = null;
  if (name === 'Edit' && (input.old_string || input.new_string)) {
    body = (
      <div className="tool-diff">
        {str(input.old_string) && <DiffLines text={str(input.old_string)} sign="del" />}
        {str(input.new_string) && <DiffLines text={str(input.new_string)} sign="add" />}
      </div>
    );
  } else if (name === 'Write' && input.content) {
    body = (
      <div className="tool-diff">
        <DiffLines text={str(input.content)} sign="add" />
      </div>
    );
  } else if (name === 'Bash' && input.command) {
    body = <pre className="tool-cmd">$ {str(input.command)}</pre>;
  } else if (Object.keys(input).length > 0) {
    body = (
      <details className="tool-more">
        <summary>{t('context.viewInput')}</summary>
        <pre>{JSON.stringify(input, null, 2).slice(0, 4000)}</pre>
      </details>
    );
  }

  return (
    <div className="msg tool">
      <span className="msg-role">tool</span>
      <div className="msg-body">
        <p className="tool-head">
          <strong>{name}</strong>
          {headArg && <code>({headArg})</code>}
        </p>
        {body}
      </div>
    </div>
  );
}

// ToolResultRow — Result is collapsible (first line preview).
function ToolResultRow({ ev }: { ev: CIREvent }) {
  const out = typeof ev.output === 'string' ? ev.output : ev.output != null ? JSON.stringify(ev.output, null, 2) : '';
  if (!out.trim()) return <div className="msg meta">[tool_result]</div>;
  const firstLine = out.trimStart().split('\n')[0].slice(0, 100);
  return (
    <div className="msg tool">
      <span className="msg-role" />
      <div className="msg-body">
        <details className="tool-more">
          <summary>
            ↳ <code>{firstLine}</code>
            {out.length > firstLine.length && ' …'}
          </summary>
          <pre>{out.slice(0, 8000)}</pre>
        </details>
      </div>
    </div>
  );
}

// ReasoningRow — Plain text summary only (locked original text is not shown). Summary-less codex
// encrypted reasoning is shown as 0 — empty "[reasoning]" line is noise and omitted.
function ReasoningRow({ ev }: { ev: CIREvent }) {
  const summary = ev.redacted_summary ?? '';
  if (!summary.trim()) return null;
  return (
    <div className="msg meta">
      <span className="msg-role" />
      <div className="msg-body">
        <details className="tool-more reasoning">
          <summary>reasoning — {summary.split('\n')[0].slice(0, 80)}…</summary>
          {/* Reasoning summary also markdown text — rendered like chat body */}
          <Markdown text={mdSlice(summary, 8000)} />
        </details>
      </div>
    </div>
  );
}

// CompactSummaryRow — Summary generated by agent context compression. It can resemble a long user message, so a collapsible block distinguishes it (audit finding #13).
// cxt-synthesized seed digests carry the same CompactSummary marking but are not agent-written — they get their own label (#38).
function CompactSummaryRow({ ev }: { ev: CIREvent }) {
  const t = useT();
  const text = (ev.blocks ?? []).map((b) => b.text).join('\n');
  const isSeed = CXT_SEED_PREFIXES.some((p) => text.trimStart().startsWith(p));
  return (
    <div className="msg meta compact-summary">
      <span className="msg-role" />
      <div className="msg-body">
        <details className="tool-more">
          <summary>
            ◈ {t(isSeed ? 'context.cxtSeedSummary' : 'context.compactSummary')} — <code>{text.slice(0, 72)}…</code>
          </summary>
          <Markdown text={mdSlice(text, 16000)} />
        </details>
      </div>
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
