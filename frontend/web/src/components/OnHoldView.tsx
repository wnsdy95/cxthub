// OnHoldView — still pending tasks tab that hasn't reached the shared timeline (pushed branch ref).
// Contains two types:
//   1) Unsync commits (unsync): Commits made locally but not yet pushed — a chain of commits — context tab and
//      similar commit list by author. Moves to context tab upon git push.
//   2) Pending orphan sessions (pending): Sessions that don't connect to any tip (server ref/unsync included). Resolved by git commit. Saved manually until deleted.
// Pending following an unsync tip is rendered as the "following thread" tail of the commit viewer.
// Layout is similar to the context tab: branch filter bar + list/viewer + right rail (About→Settings→Secrets→Commit Graph→AI Configuration).
import { useEffect, useMemo, useState } from 'react';
import type { Repo, Workspace, Pending } from '../types';
import {
  useDoc,
  usePendings,
  useUnsyncs,
  useDismissPending,
  useUndismissPending,
  useMe,
  useFork,
  useRepoView,
} from '../hooks';
import { atLeast, canWriteAsset, type Role } from '../roles';
import { AIIcon, PROVIDER_META, PROVIDER_LOGOS, PROVIDER_INK } from './AIBar';
import { AIBar } from './AIBar';
import { CommitGraph } from './CommitGraph';
import { About, TeamSettings, SecretsPanel } from './About';
import { EventStream, short, when, commonEventPrefix, type ViewMode } from './ContextView';
import { sharedReachable, unsyncChains, orphanPendings } from '../onhold';
import { usePaged, PageControl } from './Pagination';
import { useT, Rich } from '../i18n';

export function OnHoldView({ repo, ws, role }: { repo: Repo; ws: Workspace | null; role: Role | null }) {
  const t = useT();
  const me = useMe().data;
  // Repo derivative state is the same assembly point (useRepoView) as the context tab — excluding stash, badges, and graph.
  // If the source forks, the "badge count = tab row count" guarantee from the input phase breaks (review front #2).
  const { refs, snapshots: allSnapshots, badges, graphSnapshots, committedSnapshots, uncommittedIds, localAhead } = useRepoView(repo.id);
  const pendings = usePendings(repo.id).data ?? [];
  const unsyncs = useUnsyncs(repo.id).data ?? [];
  const dismissPending = useDismissPending();
  const undismissPending = useUndismissPending();
  // Chain collapse/expand — convenience feature, no data impact. Header always visible, no recovery issues.
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  useEffect(() => {
    setCollapsed(new Set());
  }, [repo.id]);
  const toggleCollapsed = (key: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const byId = useMemo(() => new Map(allSnapshots.map((s) => [s.id, s])), [allSnapshots]);
  const branches = useMemo(() => refs.filter((r) => r.kind === 'branch').map((r) => r.name).sort(), [refs]);

  // Determination is a common definition in onhold.ts — must match the context tab badge count.
  const shared = useMemo(() => sharedReachable(refs, allSnapshots), [refs, allSnapshots]);
  const chains = useMemo(() => unsyncChains(unsyncs, allSnapshots, shared), [unsyncs, allSnapshots, shared]);
  const orphans = useMemo(() => orphanPendings(pendings, refs, allSnapshots, chains), [pendings, refs, allSnapshots, chains]);

  // Branch/member filters — pending items can span multiple branches/authors, default is all.
  const [branchSel, setBranchSel] = useState<string>('*');
  const [memberSel, setMemberSel] = useState<string>('*');
  // Member identifier key: email priority (stable identifier), fallback to username/name.
  const memberKey = (a?: { name: string; email: string }, user?: string) => a?.email || user || a?.name || '';
  const memberLabel = (a?: { name: string; email: string }, user?: string) => a?.name || user || a?.email || '?';
  // Dropdown options are composed of actual authors of draft items (irrelevant to filters, remains stable).
  const members = useMemo(() => {
    const m = new Map<string, string>();
    for (const c of chains) {
      for (const u of c.tips) {
        const k = memberKey(u.author, u.user);
        if (k) m.set(k, memberLabel(u.author, u.user));
      }
    }
    for (const p of orphans) {
      const k = memberKey(p.author);
      if (k) m.set(k, memberLabel(p.author));
    }
    return [...m.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  }, [chains, orphans]);
  // Cluster filter: if any tip pointer meets the condition, the entire chunk is shown.
  // (Chained commits are not split — this follows the grouping principle of this tab).
  const chainsShown = useMemo(
    () =>
      chains
        .filter((c) => branchSel === '*' || c.tips.some((u) => u.branch === branchSel))
        .filter((c) => memberSel === '*' || c.tips.some((u) => memberKey(u.author, u.user) === memberSel)),
    [chains, branchSel, memberSel],
  );
  const orphansShown = useMemo(
    () =>
      orphans
        .filter((p) => branchSel === '*' || p.branch === branchSel)
        .filter((p) => memberSel === '*' || memberKey(p.author) === memberSel),
    [orphans, branchSel, memberSel],
  );
  const holdTotal = useMemo(
    () => chainsShown.reduce((n, c) => n + c.chain.length, 0) + orphansShown.length,
    [chainsShown, orphansShown],
  );
  // "View N at a time" (default 5) — paginates chain clusters and orphan sessions as a single stream.
  // Chains are not split, so pagination is by item count (clusters/sessions) rather than commit count.
  const holdItems = useMemo(
    () => [
      ...chainsShown.map((c) => ({ kind: 'chain' as const, c })),
      ...orphansShown.map((p) => ({ kind: 'orphan' as const, p })),
    ],
    [chainsShown, orphansShown],
  );
  const paged = usePaged(holdItems, `${branchSel}|${memberSel}`);
  const visChains = paged.visible.flatMap((it) => (it.kind === 'chain' ? [it.c] : []));
  const visOrphans = paged.visible.flatMap((it) => (it.kind === 'orphan' ? [it.p] : []));

  const [selSnap, setSelSnap] = useState<string | null>(null);
  const [selPending, setSelPending] = useState<string | null>(null);
  const selected = selSnap ? byId.get(selSnap) ?? null : null;
  const selectedPending: Pending | null =
    (selPending ? orphans.find((p) => p.session_id === selPending) : null) ?? null;
  // Hidden targets: session-specific fallback selection or if the selected snapshot is the target of an uncommitted session.
  const dismissablePending: Pending | null =
    selectedPending ?? (selected ? orphans.find((p) => p.target === selected.id) ?? null : null);
  const doc = useDoc(repo.id, selected?.doc_hash ?? selectedPending?.target ?? null).data;
  // Pending tail from the unsync tip (only when the selected commit is that tip).
  const tailPending = useMemo(() => {
    if (!selected?.session_id) return null;
    return pendings.find((p) => p.session_id === selected.session_id && p.target !== selected.id) ?? null;
  }, [selected, pendings]);
  const tailDoc = useDoc(repo.id, tailPending?.target ?? null).data;
  const tailStart = useMemo(() => {
    if (!doc || !tailDoc) return 0;
    return commonEventPrefix(tailDoc.cir.events, doc.cir.events);
  }, [doc, tailDoc]);
  const [viewMode, setViewMode] = useState<ViewMode>('all');
  // Fork (checkout): can create a new branch from unpushed commits — the fork base is a git commit,
  // so running git branch <name> locally aligns the branch with this commit and connects this context.
  const forkMut = useFork();
  const [forkOpen, setForkOpen] = useState(false);
  const [forkName, setForkName] = useState('');

  return (
    <div className="ctx ctx-cols">
      <div className="ctx-main">
        <div className="ctx-bar">
          <select aria-label={t('common.branch')} value={branchSel} onChange={(e) => setBranchSel(e.target.value)}>
            <option value="*">{t('common.allBranches')}</option>
            {branches.map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </select>
          <select aria-label={t('common.member')} value={memberSel} onChange={(e) => setMemberSel(e.target.value)}>
            <option value="*">{t('common.allMembers')}</option>
            {members.map(([k, label]) => (
              <option key={k} value={k}>
                {label}
              </option>
            ))}
          </select>
          <span className="ctx-count">{t('onhold.holdCount', { count: holdTotal })}</span>
        </div>

        {holdItems.length > 0 && <PageControl paged={paged} />}

        {chainsShown.length === 0 && orphansShown.length === 0 && (
          <div className="empty-box"><Rich>{t('onhold.empty')}</Rich></div>
        )}

        {visChains.length > 0 && (
          <>
            <span className="label">{t('onhold.pendingPush')}</span>
            <ul className="commits">
              {visChains.map(({ tips, chain }) => {
                const key = chain[0]?.id ?? tips[0]?.target ?? '';
                const label = tips
                  .map((u) => `${u.user || u.author?.name || '?'} · ${u.branch}`)
                  .join(' + ');
                return (
                <li key={key}>
                  <div className="session-divider pending-divider">
                    <button
                      className="dl-btn"
                      onClick={() => toggleCollapsed(key)}
                      aria-expanded={!collapsed.has(key)}
                      title={collapsed.has(key) ? t('onhold.expandCommits') : t('onhold.collapseCommits')}
                    >
                      {collapsed.has(key) ? '▸' : '▾'}
                    </button>
                    {label} — {t('onhold.chainPending', { count: chain.length })} · {when(tips[0]?.updated_at ?? chain[0]?.created_at)}
                  </div>
                  {!collapsed.has(key) && (
                    <ul className="commits">
                      {chain.map((s) => (
                        <li key={s.id}>
                          <button
                            className={`commit-row${s.id === selSnap ? ' on' : ''}`}
                            onClick={() => {
                              setSelPending(null);
                              setSelSnap(s.id === selSnap ? null : s.id);
                            }}
                          >
                            <code>{short(s.id)}</code>
                            <span className="commit-msg">{s.message || t('common.noMessage')}</span>
                            <span className="ref-badge pending">{t('onhold.pendingBadge')}</span>
                            {s.memory_hash && (
                              <span className="ref-badge memory" title={t('context.memoryBadgeTitle')}>
                                ◆
                              </span>
                            )}
                            <em>
                              {s.author?.name || s.author?.email || s.provider} · {when(s.created_at)}
                            </em>
                          </button>
                        </li>
                      ))}
                    </ul>
                  )}
                </li>
                );
              })}
            </ul>
          </>
        )}

        {visOrphans.length > 0 && (
          <>
            <span className="label">{t('onhold.uncommittedSection')}</span>
            <ul className="commits">
              {visOrphans.map((p) => (
                <li key={p.session_id}>
                  <button
                    className={`commit-row${p.session_id === selPending || (selSnap !== null && p.target === selSnap) ? ' on' : ''}`}
                    onClick={() => {
                      // Unify viewer: Open graph selection and related screens (snapshot header + ⎇ branch + hide) if the target snapshot exists. Fall back to session-specific screen only if the object has not yet been fetched.
                      if (byId.has(p.target)) {
                        setSelPending(null);
                        setSelSnap(p.target === selSnap ? null : p.target);
                      } else {
                        setSelSnap(null);
                        setSelPending(p.session_id === selPending ? null : p.session_id);
                      }
                    }}
                  >
                    <code>{short(p.target)}</code>
                    <span className="commit-msg">{t('onhold.inProgress')} · {p.branch || t('onhold.unknownBranch')}</span>
                    <span className="ref-badge pending">● uncommitted</span>
                    <AIIcon
                      logo={PROVIDER_LOGOS[p.provider] ?? null}
                      color={PROVIDER_META[p.provider]?.color ?? '#b6bcc6'}
                      title={p.provider}
                    />
                    <em>
                      {p.author?.name || p.author?.email || p.provider} · {when(p.updated_at)}
                    </em>
                  </button>
                </li>
              ))}
            </ul>
          </>
        )}

        {pendings.some((p) => p.dismissed) && (
          <details className="dismissed-pendings">
            <summary className="label">
              {t('onhold.dismissedSection', { n: pendings.filter((p) => p.dismissed).length })}
            </summary>
            <ul className="commits">
              {pendings
                .filter((p) => p.dismissed)
                .map((p) => (
                  <li key={p.session_id}>
                    <div className="commit-row dismissed">
                      <code>{short(p.target)}</code>
                      <span className="commit-msg">
                        {t('onhold.inProgress')} · {p.branch || t('onhold.unknownBranch')}
                      </span>
                      {atLeast(role, 'member') && (
                        <button
                          className="ghost mini"
                          disabled={undismissPending.isPending}
                          onClick={() => undismissPending.mutate({ repoId: repo.id, sessionId: p.session_id })}
                        >
                          {t('onhold.undismiss')}
                        </button>
                      )}
                    </div>
                  </li>
                ))}
            </ul>
          </details>
        )}

        {(selected || selectedPending) && (
          <div
            className="viewer"
            style={{ ['--assistant-ink' as string]: PROVIDER_INK[(selected?.provider ?? selectedPending?.provider) as string] ?? 'var(--text)' } as React.CSSProperties}
          >
            <div className="viewer-head">
              <code>{short(selected?.id ?? selectedPending!.target)}</code>{' '}
              {selected
                ? selected.message
                : t('onhold.sessionUnlinked', { id: selectedPending!.session_id.slice(0, 8) })}
              <span className="dl-btns">
                <select
                  className="view-mode"
                  aria-label={t('context.viewModeAria')}
                  value={viewMode}
                  onChange={(e) => setViewMode(e.target.value as ViewMode)}
                >
                  <option value="all">{t('context.viewModeAll')}</option>
                  <option value="prompts">{t('common.promptOnly')}</option>
                  <option value="chat">{t('context.viewModeChat')}</option>
                </select>
                {/* Shared commit = fork, local predecessor (unpushed ∪ uncommitted) tip = checkout -b (entire chain branch),
                    // Intermediate node in predecessor chain = no branch action (same rules as ContextView). */}
                {selected &&
                  atLeast(role, 'member') &&
                  (!localAhead.ids.has(selected.id) ? (
                    <button className={`dl-btn${forkOpen ? ' on' : ''}`} onClick={() => setForkOpen((v) => !v)}>
                      {t('common.forkHere')}
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
                {/* Offer the same hide action whenever the selected snapshot is an uncommitted
                    session target, regardless of whether it was selected from the graph or list. */}
                {dismissablePending && atLeast(role, 'member') && (
                  <button
                    className="dl-btn"
                    disabled={dismissPending.isPending}
                    onClick={() => {
                      if (confirm(t('onhold.confirmDismissPending'))) {
                        dismissPending.mutate({ repoId: repo.id, sessionId: dismissablePending.session_id });
                        setSelPending(null);
                        setSelSnap(null);
                      }
                    }}
                  >
                    {t('onhold.dismissPending')}
                  </button>
                )}
              </span>
            </div>
            {forkOpen && selected && (
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
                        onSuccess: () => {
                          setForkOpen(false);
                          setForkName('');
                        },
                      },
                    )
                  }
                >
                  {forkMut.isPending ? t('common.forking') : t('common.fork')}
                </button>
                <em>{t('onhold.forkHint', { name: forkName.trim() || '<name>' })}</em>
                {forkMut.error && <span className="err">{forkMut.error.message}</span>}
              </div>
            )}
            {doc ? (
              <>
                <EventStream
                  key={`hold-${selected?.id ?? selectedPending!.target}`}
                  events={doc.cir.events}
                  mode={viewMode}
                />
                {selected && tailPending && tailDoc && tailDoc.cir.events.length > tailStart && (
                  <>
                    <div className="session-divider pending-divider">
                      {t('onhold.continuingConvo', { when: when(tailPending.updated_at) })}
                    </div>
                    <EventStream
                      key={`hold-tail-${tailPending.target}`}
                      events={tailDoc.cir.events.slice(tailStart)}
                      offset={tailStart}
                      mode={viewMode}
                    />
                  </>
                )}
              </>
            ) : (
              <div className="skel" style={{ height: 60 }} />
            )}
          </div>
        )}
      </div>

      {/* Right rail — same as context tab: About → Team Settings → Secrets → Commit Graph → AI Configuration */}
      <aside className="ctx-side">
        <About repo={repo} canEdit={canWriteAsset(role, undefined)} />
        {atLeast(role, 'puller') && (
          <TeamSettings repoId={repo.id} canWrite={canWriteAsset(role, ws?.settings_policy)} />
        )}
        {atLeast(role, 'puller') && (
          <SecretsPanel repoId={repo.id} canWrite={canWriteAsset(role, ws?.secrets_policy)} />
        )}
        <span className="label">{t('common.commitGraphTotal', { count: committedSnapshots.length })}</span>
        <CommitGraph
          snapshots={graphSnapshots}
          selectedId={selSnap}
          onSelect={(id) => {
            setSelPending(null);
            setSelSnap(id);
          }}
          badges={badges}
          refs={refs}
          uncommitted={uncommittedIds}
          pinBranch={repo.default_branch || 'main'}
          repoId={atLeast(role, 'member') ? repo.id : null}
        />
        <AIBar snapshots={committedSnapshots} />
      </aside>
    </div>
  );
}
