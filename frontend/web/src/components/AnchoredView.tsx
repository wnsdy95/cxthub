// AnchoredView — the sessions tab. It shows the caller's own session reports
// by commit, branch and worktree: on the left the worktrees and branches a
// session's uncommitted part was worked on, in the middle the commits its
// segments end at, on the right what the selected one holds. A session opens
// as its whole conversation with a divider where each segment begins.
import { useMemo, useState, type ReactNode } from 'react';
import type { AnchoredSelection } from '../route';
import type { AnchoredSessionSummary, Repo } from '../types';
import { useAnchoredCommit, useAnchoredSessions, useBranchLifetimes } from '../hooks';
import {
  ANCHORED_PROVIDERS, anchoredKey, anchoredSegmentCommits, anchoredShortId, anchoredShortSha, buildAnchoredIndex,
  type AnchoredBranchUse, type AnchoredCommitRow, type AnchoredIndex,
} from '../anchoredSessions';
import { when } from '../snapshotFormat';
import { useT } from '../i18n';
import { AppLink } from './AppLink';
import { AnchoredSessionView } from './AnchoredSessionView';

export type AnchoredHref = (selection?: AnchoredSelection) => string;
const COMMIT_STEP = 100;

export function AnchoredView({ repo, selection, href }: { repo: Repo; selection?: AnchoredSelection; href: AnchoredHref }) {
  const t = useT();
  const sessionsQ = useAnchoredSessions(repo.id);
  const lifetimesQ = useBranchLifetimes(repo.id);
  const index = useMemo(() => buildAnchoredIndex(sessionsQ.data ?? [], lifetimesQ.data ?? []), [sessionsQ.data, lifetimesQ.data]);
  // Commits that share the segment under the pointer are marked together.
  const [highlighted, setHighlighted] = useState<string[]>([]);

  if (sessionsQ.isLoading) return <div className="skel" role="status" aria-label={t('anchored.loading')} style={{ height: 120 }} />;
  if (sessionsQ.error) {
    return (
      <p role="alert" className="err">
        {sessionsQ.error.message} <button onClick={() => void sessionsQ.refetch()}>{t('context.retryRead')}</button>
      </p>
    );
  }
  const shown = selection ?? { kind: 'all' as const };
  return (
    <div className="anchor">
      <nav className="anchor-nav" aria-label={t('anchored.navAria')}>
        <AppLink className={`anchor-nav-item${shown.kind === 'all' ? ' on' : ''}`} href={href({ kind: 'all' })} aria-current={shown.kind === 'all' ? 'page' : undefined}>
          {t('anchored.allSessions')} <span className="count-badge">{index.sessions.size}</span>
        </AppLink>
        <h5 className="label">{t('anchored.worktrees')}</h5>
        {index.worktrees.length === 0 && <p className="anchor-nav-empty">{t('anchored.noWorktrees')}</p>}
        <ul>
          {index.worktrees.map(worktree => {
            const on = shown.kind === 'worktree' && shown.name === worktree.name && (shown.machine ?? '') === worktree.machine;
            return (
              <li key={worktree.key}>
                <AppLink className={`anchor-nav-item${on ? ' on' : ''}`} aria-current={on ? 'page' : undefined}
                  href={href({ kind: 'worktree', name: worktree.name, ...(worktree.machine ? { machine: worktree.machine } : {}) })}>
                  <span className="anchor-nav-name">{worktree.name}</span>
                  {worktree.machine && <em>{worktree.machine}</em>}
                  {worktree.removed && <span className="ref-badge archived">{t('anchored.removedWorktree')}</span>}
                </AppLink>
              </li>
            );
          })}
        </ul>
        <h5 className="label">{t('anchored.branches')}</h5>
        {index.branches.length === 0 && <p className="anchor-nav-empty">{t('anchored.noBranches')}</p>}
        <ul>
          {index.branches.map(branch => {
            const on = shown.kind === 'branch' && shown.name === branch.name;
            return (
              <li key={branch.name}>
                <AppLink className={`anchor-nav-item${on ? ' on' : ''}`} aria-current={on ? 'page' : undefined} href={href({ kind: 'branch', name: branch.name })}>
                  <span className="anchor-nav-name">{branch.name}</span>
                  {branch.earlier.length > 0 && <span className="ref-badge archived">{t('anchored.earlierUses', { count: branch.earlier.length })}</span>}
                </AppLink>
              </li>
            );
          })}
        </ul>
      </nav>
      <div className="anchor-body">
        <div className="anchor-bar">
          <span>{t('anchored.ownOnly')}</span>
          <button className="ghost" onClick={() => { void sessionsQ.refetch(); void lifetimesQ.refetch(); }} disabled={sessionsQ.isFetching}>
            {sessionsQ.isFetching ? t('anchored.refreshing') : t('anchored.refresh')}
          </button>
        </div>
        {lifetimesQ.error && <p role="alert" className="err">{lifetimesQ.error.message}</p>}
        {index.sessions.size === 0 ? (
          <div className="empty-box">
            {t('anchored.emptyLead')}
            <br />
            <code>cxt segments push</code>
            <br />
            {t('anchored.emptyTail')}
          </div>
        ) : shown.kind === 'session' ? (
          <AnchoredSessionView key={anchoredKey(shown.provider, shown.id)} repo={repo} provider={shown.provider} sessionId={shown.id}
            segment={shown.segment} rest={shown.rest} index={index} href={href} />
        ) : (
          <div className="anchor-split">
            <AnchoredCommits index={index} selected={shown.kind === 'commit' ? shown.sha : ''} highlighted={highlighted} href={href} />
            <section className="anchor-detail" aria-label={t('anchored.detailAria')}>
              {shown.kind === 'commit' && <CommitDetail key={shown.sha} repo={repo} index={index} sha={shown.sha} href={href} onHighlight={setHighlighted} />}
              {shown.kind === 'branch' && <BranchDetail index={index} name={shown.name} href={href} />}
              {shown.kind === 'worktree' && <WorktreeDetail index={index} name={shown.name} machine={shown.machine ?? ''} href={href} />}
              {shown.kind === 'all' && <AllSessions index={index} href={href} />}
            </section>
          </div>
        )}
      </div>
    </div>
  );
}

function AnchoredCommits({ index, selected, highlighted, href }: { index: AnchoredIndex; selected: string; highlighted: string[]; href: AnchoredHref }) {
  const t = useT();
  const [limit, setLimit] = useState(COMMIT_STEP);
  return (
    <section className="anchor-commits" aria-label={t('anchored.commits')}>
      <h5 className="label">
        {t('anchored.commits')} <span className="count-badge">{index.commits.length}</span>
      </h5>
      {index.commits.length === 0 && <p className="empty-box">{t('anchored.noCommits')}</p>}
      <ol className="anchor-commit-list">
        {index.commits.slice(0, limit).map(commit => {
          const on = commit.sha === selected;
          return (
            <li key={commit.sha}>
              <AppLink className={`anchor-commit${on ? ' on' : ''}${highlighted.includes(commit.sha) ? ' hl' : ''}`} href={href({ kind: 'commit', sha: commit.sha })}
                aria-current={on ? 'true' : undefined} data-commit={commit.sha}>
                <span className="anchor-commit-subject">{commit.subject || t('anchored.noSubject')}</span>
                <span className="anchor-commit-meta">
                  <code>{anchoredShortSha(commit.sha)}</code>
                  {commit.at && <time dateTime={commit.at}>{when(commit.at, true)}</time>}
                  <span className="count-badge">{t('anchored.segmentCount', { count: commit.links.length })}</span>
                </span>
              </AppLink>
            </li>
          );
        })}
      </ol>
      {index.commits.length > limit && (
        <button className="ghost" onClick={() => setLimit(limit + COMMIT_STEP)}>
          {t('anchored.showMore', { count: index.commits.length - limit })}
        </button>
      )}
    </section>
  );
}

function ProviderMark({ provider }: { provider: AnchoredSessionSummary['provider'] }) {
  return <span className="anchor-provider" style={{ color: ANCHORED_PROVIDERS[provider]?.ink }}>{ANCHORED_PROVIDERS[provider]?.name ?? provider}</span>;
}

/** One session in a list: who it is, how large, and where to open it. */
function SessionCard({ summary, selection, index, href, children }: {
  summary: AnchoredSessionSummary; selection: AnchoredSelection; index: AnchoredIndex; href: AnchoredHref; children?: ReactNode;
}) {
  const t = useT();
  const spawned = index.children.get(anchoredKey(summary.provider, summary.session_id))?.length ?? 0;
  return (
    <>
      <div className="anchor-card-head">
        <ProviderMark provider={summary.provider} />
        <AppLink href={href(selection)} title={summary.session_id}>
          <code>{anchoredShortId(summary.session_id)}</code>
        </AppLink>
        {summary.sub && <span className="ref-badge">{t('anchored.subSession')}</span>}
        {summary.uncommitted?.unplaced && <span className="ref-badge pending">{t('anchored.unplaced')}</span>}
        {!summary.content && <span className="ref-badge archived">{t('anchored.noContentBadge')}</span>}
        {summary.machine && <em>{summary.machine}</em>}
        {summary.started_at && <time dateTime={summary.started_at}>{when(summary.started_at, true)}</time>}
      </div>
      <p className="anchor-card-meta">
        {t('anchored.sessionCounts', { records: summary.records, commands: summary.commands, segments: summary.segments })}
        {spawned > 0 && <> · {t('anchored.spawnedCount', { count: spawned })}</>}
      </p>
      {children}
    </>
  );
}

function SessionLinks({ sessions, index, href }: { sessions: AnchoredSessionSummary[]; index: AnchoredIndex; href: AnchoredHref }) {
  return (
    <ul className="anchor-sub">
      {sessions.map(child => (
        <li key={anchoredKey(child.provider, child.session_id)}>
          <SessionCard summary={child} index={index} href={href} selection={{ kind: 'session', provider: child.provider, id: child.session_id }} />
        </li>
      ))}
    </ul>
  );
}

function CommitDetail({ repo, index, sha, href, onHighlight }: {
  repo: Repo; index: AnchoredIndex; sha: string; href: AnchoredHref; onHighlight: (shas: string[]) => void;
}) {
  const t = useT();
  const commit: AnchoredCommitRow | undefined = index.commitBySha.get(sha);
  const revisions = (commit?.links ?? []).map(link => `${anchoredKey(link.provider, link.session_id)}@${index.sessions.get(anchoredKey(link.provider, link.session_id))?.revision ?? 0}`).join(',');
  const segmentsQ = useAnchoredCommit(repo.id, sha, revisions);
  return (
    <>
      <h5 className="anchor-detail-title">
        {commit?.subject || t('anchored.noSubject')} <code>{anchoredShortSha(sha)}</code>
      </h5>
      {!commit && <p className="empty-box">{t('anchored.noContext')}</p>}
      <ul className="anchor-cards">
        {(commit?.links ?? []).map(link => {
          const key = anchoredKey(link.provider, link.session_id);
          const summary = index.sessions.get(key);
          if (!summary) return null;
          const range = segmentsQ.data?.find(entry => entry.provider === link.provider && entry.session_id === link.session_id)?.segment;
          const shared = anchoredSegmentCommits(summary, link.segment_no);
          const spawned = (index.children.get(key) ?? []).filter(child => child.spawned_in?.segment_no === link.segment_no);
          return (
            <li key={`${key}#${link.segment_no}`} className="anchor-card" tabIndex={0} data-segment={`${key}#${link.segment_no}`}
              onMouseEnter={() => onHighlight(shared)} onMouseLeave={() => onHighlight([])} onFocus={() => onHighlight(shared)} onBlur={() => onHighlight([])}>
              <SessionCard summary={summary} index={index} href={href} selection={{ kind: 'session', provider: link.provider, id: link.session_id, segment: link.segment_no }}>
                <p className="anchor-card-meta">
                  {t('anchored.segmentOf', { no: link.segment_no, total: summary.segments })} · {t(`anchored.rule_${link.rule}`)}
                  {range && <> · {t('anchored.commandCount', { count: range.commands })}</>}
                  {range?.started_at && <> · <time dateTime={range.started_at}>{when(range.started_at, true)}</time></>}
                  {range?.ended_at && <> – <time dateTime={range.ended_at}>{when(range.ended_at, true)}</time></>}
                </p>
                {shared.length > 1 && (
                  <p className="anchor-card-meta">
                    {t('anchored.sharedCommits')}{' '}
                    {shared.filter(other => other !== sha).map(other => (
                      <AppLink key={other} href={href({ kind: 'commit', sha: other })}><code>{anchoredShortSha(other)}</code></AppLink>
                    ))}
                  </p>
                )}
                {spawned.length > 0 && (
                  <>
                    <p className="anchor-card-meta">{t('anchored.spawnedHere')}</p>
                    <SessionLinks sessions={spawned} index={index} href={href} />
                  </>
                )}
              </SessionCard>
            </li>
          );
        })}
      </ul>
      {segmentsQ.error && <p role="alert" className="err">{segmentsQ.error.message}</p>}
    </>
  );
}

function UncommittedCards({ keys, index, href }: { keys: string[]; index: AnchoredIndex; href: AnchoredHref }) {
  const t = useT();
  if (keys.length === 0) return <p className="empty-box">{t('anchored.noContext')}</p>;
  return (
    <ul className="anchor-cards">
      {keys.map(key => {
        const summary = index.sessions.get(key);
        if (!summary) return null;
        const part = summary.uncommitted;
        return (
          <li key={key} className="anchor-card">
            <SessionCard summary={summary} index={index} href={href} selection={{ kind: 'session', provider: summary.provider, id: summary.session_id, rest: true }}>
              {part && (
                <p className="anchor-card-meta">
                  {t('anchored.afterLastCommit')} · {t('anchored.commandCount', { count: part.commands })}
                  {part.started_at && <> · <time dateTime={part.started_at}>{when(part.started_at, true)}</time></>}
                  {part.ended_at && <> – <time dateTime={part.ended_at}>{when(part.ended_at, true)}</time></>}
                </p>
              )}
            </SessionCard>
          </li>
        );
      })}
    </ul>
  );
}

function BranchUse({ use, index, href, earlier }: { use: AnchoredBranchUse; index: AnchoredIndex; href: AnchoredHref; earlier: boolean }) {
  const t = useT();
  const lifetime = use.lifetime;
  return (
    <section className="anchor-use">
      <h6>
        {earlier ? t('anchored.earlierUse') : t('anchored.currentUse')}{' '}
        {lifetime ? (
          <span className="anchor-card-meta">
            <time dateTime={lifetime.opened_at}>{when(lifetime.opened_at)}</time>
            {lifetime.closed_at && <> – <time dateTime={lifetime.closed_at}>{when(lifetime.closed_at)}</time></>}
          </span>
        ) : (
          <span className="anchor-card-meta">{t('anchored.notOnRemote')}</span>
        )}
      </h6>
      <UncommittedCards keys={use.sessions} index={index} href={href} />
    </section>
  );
}

function BranchDetail({ index, name, href }: { index: AnchoredIndex; name: string; href: AnchoredHref }) {
  const t = useT();
  const branch = index.branches.find(row => row.name === name);
  return (
    <>
      <h5 className="anchor-detail-title">{t('anchored.branchTitle')} <code>{name}</code></h5>
      <p className="anchor-card-meta">{t('anchored.branchLead')}</p>
      {!branch && <p className="empty-box">{t('anchored.noContext')}</p>}
      {branch && <BranchUse use={branch.current} index={index} href={href} earlier={false} />}
      {branch?.earlier.map(use => <BranchUse key={use.lifetime!.ledger_id} use={use} index={index} href={href} earlier />)}
    </>
  );
}

function WorktreeDetail({ index, name, machine, href }: { index: AnchoredIndex; name: string; machine: string; href: AnchoredHref }) {
  const t = useT();
  const worktree = index.worktrees.find(row => row.name === name && row.machine === machine);
  return (
    <>
      <h5 className="anchor-detail-title">
        {t('anchored.worktreeTitle')} <code>{name}</code> {machine && <em>{machine}</em>}
        {worktree?.removed && <span className="ref-badge archived">{t('anchored.removedWorktree')}</span>}
      </h5>
      <p className="anchor-card-meta">{t('anchored.worktreeLead')}</p>
      {worktree && worktree.branches.length > 0 && (
        <p className="anchor-card-meta">
          {t('anchored.branches')}:{' '}
          {worktree.branches.map(branch => (
            <AppLink key={branch} className="ref-badge branch" href={href({ kind: 'branch', name: branch })}>{branch}</AppLink>
          ))}
        </p>
      )}
      <UncommittedCards keys={worktree?.sessions ?? []} index={index} href={href} />
    </>
  );
}

function AllSessions({ index, href }: { index: AnchoredIndex; href: AnchoredHref }) {
  const t = useT();
  return (
    <>
      <h5 className="anchor-detail-title">{t('anchored.allSessions')}</h5>
      <p className="anchor-card-meta">{t('anchored.allLead')}</p>
      <ul className="anchor-cards">
        {index.roots.map(root => {
          const key = anchoredKey(root.provider, root.session_id);
          const children = index.children.get(key) ?? [];
          return (
            <li key={key} className="anchor-card">
              <SessionCard summary={root} index={index} href={href} selection={{ kind: 'session', provider: root.provider, id: root.session_id }}>
                {children.length > 0 && (
                  <details>
                    <summary>{t('anchored.spawnedCount', { count: children.length })}</summary>
                    <SessionLinks sessions={children} index={index} href={href} />
                  </details>
                )}
              </SessionCard>
            </li>
          );
        })}
      </ul>
    </>
  );
}
