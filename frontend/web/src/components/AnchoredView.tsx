// AnchoredView — the sessions tab. On the left the caller's own work that is
// not pushed yet, the caller's worktrees, the branches and the pull requests;
// in the middle the commit graph as the
// server read it from the hosting provider, with the commits only a report
// names; on the right what the selected one holds: the segments of the
// caller's own sessions and of teammates' sessions as far as they are pushed,
// and the notes written where no session explains it.
// A session opens as its whole conversation with a divider where each
// segment begins.
import { createContext, useContext, useMemo, useState, type ReactNode } from 'react';
import { navigate, type AnchoredSelection } from '../route';
import type { AnchoredNote, AnchoredNoteTarget, AnchoredSearchHit, AnchoredSessionSummary, AnchoredSummary, PushCollector, RemotePull, Repo } from '../types';
import {
  useAnchoredCommit, useAnchoredCommitSummaries, useAnchoredSearch, useAnchoredNotes, useAnchoredSessions, useBranchLifetimes, useDeleteAnchoredNote, useFollowViewRevision,
  useGitHubConnection, useMe, usePushOrigins, useRemoteGit, useReportedWorktrees, useSaveAnchoredNote,
} from '../hooks';
import {
  ANCHORED_PROVIDERS, anchoredBranchCommits, anchoredCompare, anchoredKey, anchoredNotesFor, anchoredSegmentCommits, anchoredShortId, anchoredShortSha, buildAnchoredIndex,
  type AnchoredBranchUse, type AnchoredCommitRow, type AnchoredIndex, type AnchoredRemote,
} from '../anchoredSessions';
import { layoutGitGraph, type GitRow } from '../anchoredGraph';
import { commitOrigin, ownFailingCollectors, pushOriginsByHead, type CommitOrigin } from '../pushOrigins';
import { when } from '../timeFormat';
import { useT } from '../i18n';
import { AppLink } from './AppLink';
import { AnchoredSessionView } from './AnchoredSessionView';
import { Markdown } from './Markdown';

type AnchoredHref = (selection?: AnchoredSelection) => string;
/** Whether the reader is a member: one who has sessions and worktrees of their own and may write notes. */
const Member = createContext(true);
const COMMIT_STEP = 100;
const LANE = 14;
const ROW = 46;
const LANE_COLORS = ['#16181d', '#2e7d5b', '#8250df', '#b4452c', '#0969da', '#bf8700'];

export function AnchoredView({ repo, member = true, manage = false, selection, href }: {
  repo: Repo; selection?: AnchoredSelection; href: AnchoredHref;
  /** False for a reader who only views the repository: a viewer, or a visitor who is not signed in. Such a reader is shown what is pushed. */
  member?: boolean;
  /** True for a reader who manages the repository: such a reader archives and restores any member's session. */
  manage?: boolean;
}) {
  const t = useT();
  useFollowViewRevision(repo.id);
  const sessionsQ = useAnchoredSessions(repo.id);
  const lifetimesQ = useBranchLifetimes(repo.id);
  const worktreesQ = useReportedWorktrees(repo.id);
  const notesQ = useAnchoredNotes(repo.id);
  const remoteQ = useRemoteGit(repo.id);
  // Where the pushes came from. Without an answer the commits carry no label.
  const originsQ = usePushOrigins(repo.id);
  const origins = useMemo(() => pushOriginsByHead(originsQ.data), [originsQ.data]);
  // Whether the server reads the repository on GitHub through the GitHub App. A member is told when it does not.
  const githubQ = useGitHubConnection(member ? repo.id : null);
  const remote = useMemo<AnchoredRemote | undefined>(() => {
    const pages = remoteQ.data?.pages;
    if (!pages?.length) return undefined;
    return { read: Boolean(pages[0].synced_at), defaultBranch: pages[0].default_branch ?? '', branches: pages[0].branches ?? [], pulls: pages[0].pulls ?? [], commits: pages.flatMap(page => page.commits) };
  }, [remoteQ.data]);
  const index = useMemo(() => buildAnchoredIndex(sessionsQ.data ?? [], lifetimesQ.data ?? [], { worktrees: worktreesQ.data, remote }),
    [sessionsQ.data, lifetimesQ.data, worktreesQ.data, remote]);
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
  // The list of work before a push is a member's own; anyone else is shown every session instead.
  const shown = !selection || (selection.kind === 'held' && !member) ? { kind: 'all' as const } : selection;
  const notes = notesQ.data ?? [];
  const synced = remoteQ.data?.pages[0]?.synced_at;
  // The commit another one is compared with stays chosen while other commits are opened.
  const base = shown.kind === 'commit' ? shown.base : undefined;
  // The commits of the selected branch or pull request, or those between two compared commits, are marked in the graph.
  const marked = shown.kind === 'branch' ? anchoredBranchCommits(index, shown.name).shas
    : shown.kind === 'pull' ? index.pulls.find(pull => pull.number === shown.number)?.commits ?? []
    : shown.kind === 'commit' && base && base !== shown.sha ? anchoredCompare(index, base, shown.sha).shas : [];
  const refresh = () => {
    void sessionsQ.refetch(); void lifetimesQ.refetch(); void worktreesQ.refetch(); void notesQ.refetch(); void remoteQ.refetch(); void originsQ.refetch();
  };
  return (
    <Member.Provider value={member}>
    <div className="anchor">
      <nav className="anchor-nav" aria-label={t('anchored.navAria')}>
        <AppLink className={`anchor-nav-item${shown.kind === 'all' ? ' on' : ''}`} href={href({ kind: 'all' })} aria-current={shown.kind === 'all' ? 'page' : undefined}>
          {t('anchored.allSessions')} <span className="count-badge">{index.sessions.size}</span>
        </AppLink>
        {member && (
          <AppLink className={`anchor-nav-item${shown.kind === 'held' ? ' on' : ''}`} href={href({ kind: 'held' })} aria-current={shown.kind === 'held' ? 'page' : undefined}>
            {t('anchored.held')} <span className="count-badge">{index.held.commits.length + index.held.uncommitted.length}</span>
            <span className="ref-badge pending">{t('anchored.heldOnlyYou')}</span>
          </AppLink>
        )}
        {index.archived.size > 0 && (
          <AppLink className={`anchor-nav-item${shown.kind === 'archived' ? ' on' : ''}`} href={href({ kind: 'archived' })} aria-current={shown.kind === 'archived' ? 'page' : undefined}>
            {t('anchored.archivedSessions')} <span className="count-badge">{index.archived.size}</span>
          </AppLink>
        )}
        {member && <h5 className="label">{t('anchored.worktrees')}</h5>}
        {member && index.worktrees.length === 0 && <p className="anchor-nav-empty">{t('anchored.noWorktrees')}</p>}
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
        {index.pulls.length > 0 && (
          <>
            <h5 className="label">{t('anchored.pulls')}</h5>
            <ul>
              {index.pulls.map(pull => {
                const on = shown.kind === 'pull' && shown.number === pull.number;
                return (
                  <li key={pull.number}>
                    <AppLink className={`anchor-nav-item${on ? ' on' : ''}`} aria-current={on ? 'page' : undefined} href={href({ kind: 'pull', number: pull.number })} title={pull.title}>
                      <code>#{pull.number}</code>
                      <span className="anchor-nav-name">{pull.title}</span>
                      {pull.state !== 'open' && <em>{t(`anchored.pullState_${pull.state}`)}</em>}
                    </AppLink>
                  </li>
                );
              })}
            </ul>
          </>
        )}
      </nav>
      <div className="anchor-body">
        {githubQ.data?.state === 'not_connected' && (
          <p role="alert" className="err" data-github-connection="not_connected">
            {t('anchored.githubNotConnected')}{' '}
            {githubQ.data.connect_url && <a href={githubQ.data.connect_url}>{t('anchored.githubConnect')}</a>}
          </p>
        )}
        {ownFailingCollectors(originsQ.data).map(collector => (
          <p key={collector.machine ?? ''} role="alert" className="err" data-own-collector={collector.machine ?? ''}>
            {t('anchored.ownCollectorFailing', { machine: collector.machine || t('anchored.originThisComputer') })}{collector.run_error ? ` ${collector.run_error}` : ''}{' '}
            {t('anchored.ownCollectorHint')}
          </p>
        ))}
        <div className="anchor-bar">
          <span>
            {t(member ? 'anchored.ownOnly' : 'anchored.openOnly')}{' '}
            {remoteQ.error ? t('anchored.remoteError') : synced ? <>{t('anchored.remoteReadAt')} <time dateTime={synced}>{when(synced, true)}</time></> : remoteQ.isLoading ? null : t('anchored.remoteNotRead')}
          </span>
          <form className="anchor-search" role="search" onSubmit={event => {
            event.preventDefault();
            const asked = String(new FormData(event.currentTarget).get('q') ?? '').trim();
            navigate(href(asked ? { kind: 'search', query: asked } : undefined));
          }}>
            <input key={shown.kind === 'search' ? shown.query : ''} type="search" name="q" defaultValue={shown.kind === 'search' ? shown.query : ''} maxLength={256}
              placeholder={t('anchored.searchPlaceholder')} aria-label={t('anchored.search')} />
            <button className="ghost" type="submit">{t('anchored.search')}</button>
          </form>
          <button className="ghost" onClick={refresh} disabled={sessionsQ.isFetching}>
            {sessionsQ.isFetching ? t('anchored.refreshing') : t('anchored.refresh')}
          </button>
        </div>
        {lifetimesQ.error && <p role="alert" className="err">{lifetimesQ.error.message}</p>}
        {index.sessions.size === 0 && index.archived.size === 0 && (member ? (
          <div className="empty-box">
            {t('anchored.emptyLead')}
            <br />
            <code>cxt segments push</code>
            <br />
            {t('anchored.emptyTail')}
          </div>
        ) : (
          <div className="empty-box">{t('anchored.emptyOpen')}</div>
        ))}
        {shown.kind === 'session' ? (
          <AnchoredSessionView key={anchoredKey(shown.provider, shown.id)} repo={repo} provider={shown.provider} sessionId={shown.id}
            segment={shown.segment} rest={shown.rest} index={index} href={href} manage={manage} />
        ) : shown.kind === 'held' ? (
          <HeldDetail index={index} href={href} />
        ) : shown.kind === 'archived' ? (
          <ArchivedDetail index={index} href={href} />
        ) : shown.kind === 'search' ? (
          <SearchResults key={shown.query} repo={repo} query={shown.query} href={href} />
        ) : (
          <div className="anchor-split">
            <AnchoredCommits index={index} origins={origins} selected={shown.kind === 'commit' ? shown.sha : ''} base={base} highlighted={highlighted} marked={marked} href={href}
              more={remoteQ.hasNextPage ? () => void remoteQ.fetchNextPage() : undefined} loading={remoteQ.isFetchingNextPage} />
            <section className="anchor-detail" aria-label={t('anchored.detailAria')}>
              {shown.kind === 'commit' && <CommitDetail key={shown.sha} repo={repo} index={index} notes={notes} sha={shown.sha} base={base} href={href} onHighlight={setHighlighted}
                origin={commitOrigin(origins, index.commitBySha.get(shown.sha)?.pushHead)} />}
              {shown.kind === 'branch' && <BranchDetail key={shown.name} repo={repo} index={index} notes={notes} name={shown.name} href={href} />}
              {shown.kind === 'worktree' && <WorktreeDetail key={`${shown.machine ?? ''}\n${shown.name}`} repo={repo} index={index} notes={notes} name={shown.name} machine={shown.machine ?? ''} href={href} />}
              {shown.kind === 'pull' && <PullDetail index={index} number={shown.number} href={href} />}
              {shown.kind === 'all' && <AllSessions index={index} href={href} />}
            </section>
          </div>
        )}
      </div>
    </div>
    </Member.Provider>
  );
}

const laneX = (lane: number) => lane * LANE + LANE / 2;
const laneColor = (lane: number) => LANE_COLORS[lane % LANE_COLORS.length];

/** The dash of a line that only joins a squash or rebase merge to the branch it came from. */
const JOINED_DASH = '4 3';
const dash = (soft: boolean | undefined) => (soft ? JOINED_DASH : undefined);

/**
 * The lanes of one row: the lines passing by, those meeting at the commit, and
 * those leaving it. A dotted line is no Git parent: it leads from a commit a
 * pull request was merged as to the newest commit of the branch it came from.
 */
function Lanes({ row, width, missing }: { row: GitRow<AnchoredCommitRow>; width: number; missing: boolean }) {
  const mid = ROW / 2, x = laneX(row.lane);
  const lines: ReactNode[] = [];
  row.incoming.forEach((waiting, lane) => {
    if (waiting === null) return;
    const top = row.incomingSoft[lane];
    if (waiting !== row.node.sha) {
      // A line passing by can turn solid here, where a commit of this row reaches the same commit by a parent of its own.
      const bottom = row.outgoingSoft[lane] ?? top;
      if (top === bottom) lines.push(<line key={`pass${lane}`} x1={laneX(lane)} y1={0} x2={laneX(lane)} y2={ROW} stroke={laneColor(lane)} strokeDasharray={dash(top)} />);
      else {
        lines.push(<line key={`pass${lane}`} x1={laneX(lane)} y1={0} x2={laneX(lane)} y2={mid} stroke={laneColor(lane)} strokeDasharray={dash(top)} />);
        lines.push(<line key={`pass${lane}b`} x1={laneX(lane)} y1={mid} x2={laneX(lane)} y2={ROW} stroke={laneColor(lane)} strokeDasharray={dash(bottom)} />);
      }
    } else if (lane === row.lane) lines.push(<line key="in" x1={x} y1={0} x2={x} y2={mid} stroke={laneColor(lane)} strokeDasharray={dash(top)} />);
    else lines.push(<path key={`join${lane}`} d={`M ${laneX(lane)} 0 C ${laneX(lane)} ${mid} ${x} ${mid * 0.4} ${x} ${mid}`} stroke={laneColor(lane)} fill="none" strokeDasharray={dash(top)} />);
  });
  if (row.outgoing[row.lane] != null) lines.push(<line key="out" x1={x} y1={mid} x2={x} y2={ROW} stroke={laneColor(row.lane)} strokeDasharray={dash(row.outgoingSoft[row.lane])} />);
  else if (row.cut) lines.push(<line key="cut" x1={x} y1={mid} x2={x} y2={ROW} stroke={laneColor(row.lane)} strokeDasharray="2 3" />);
  for (const lane of row.branchesOut) {
    lines.push(<path key={`fork${lane}`} d={`M ${x} ${mid} C ${laneX(lane)} ${mid * 1.6} ${laneX(lane)} ${mid} ${laneX(lane)} ${ROW}`} stroke={laneColor(lane)} fill="none"
      strokeDasharray={dash(row.softOut.includes(lane))} data-joined={row.softOut.includes(lane) || undefined} />);
  }
  return (
    <svg className="anchor-lanes" width={width} height={ROW} aria-hidden="true">
      {lines}
      {missing
        ? <circle cx={x} cy={mid} r={3.5} fill="var(--bg)" stroke={laneColor(row.lane)} strokeWidth={1.5} strokeDasharray="2.5 2" />
        : <circle cx={x} cy={mid} r={4} fill={laneColor(row.lane)} />}
    </svg>
  );
}

function AnchoredCommits({ index, origins, selected, base, highlighted, marked, href, more, loading }: {
  index: AnchoredIndex; selected: string; highlighted: string[]; marked: string[]; href: AnchoredHref; more?: () => void; loading: boolean;
  /** Where each push came from, by its newest commit. */
  origins: Map<string, CommitOrigin>;
  /** The commit chosen to compare with: every commit opened from the list is compared with it. */
  base?: string;
}) {
  const t = useT();
  const [limit, setLimit] = useState(COMMIT_STEP);
  const head = index.branches.find(branch => branch.name === index.defaultBranch)?.head;
  const { rows, laneCount } = useMemo(() => layoutGitGraph(index.commits.slice(0, limit), head), [index.commits, limit, head]);
  const width = Math.max(laneCount, 1) * LANE;
  const hidden = index.commits.length - limit;
  return (
    <section className="anchor-commits" aria-label={t('anchored.commits')}>
      <h5 className="label">
        {t('anchored.commits')} <span className="count-badge">{index.commits.length}</span>
      </h5>
      {index.commits.length === 0 && <p className="empty-box">{t('anchored.noCommits')}</p>}
      <ol className="anchor-commit-list">
        {rows.map(row => {
          const commit = row.node;
          const on = commit.sha === selected;
          // Only once the remote was read can a commit be said to be missing there.
          const missing = index.remoteRead && !commit.remote;
          // A commit without a session says where it was pushed from, when that is known.
          const origin = commit.links.length === 0 ? commitOrigin(origins, commit.pushHead) : undefined;
          return (
            <li key={commit.sha}>
              <AppLink className={`anchor-commit${on ? ' on' : ''}${highlighted.includes(commit.sha) ? ' hl' : ''}${marked.includes(commit.sha) ? ' in' : ''}`}
                href={href({ kind: 'commit', sha: commit.sha, ...(base ? { base } : {}) })} aria-current={on ? 'true' : undefined} data-commit={commit.sha} data-lane={row.lane}>
                <Lanes row={row} width={width} missing={missing} />
                <span className="anchor-commit-text">
                  <span className="anchor-commit-subject">
                    {commit.heads.map(name => <span key={name} className="ref-badge branch">{name}</span>)}
                    {commit.merged !== undefined && <span className="ref-badge">#{commit.merged}</span>}
                    {commit.sha === base && <span className="ref-badge pending">{t('anchored.compareBase')}</span>}
                    {commit.subject || t('anchored.noSubject')}
                  </span>
                  <span className="anchor-commit-meta">
                    <code>{anchoredShortSha(commit.sha)}</code>
                    {commit.at && <time dateTime={commit.at}>{when(commit.at, true)}</time>}
                    {commit.links.length > 0
                      ? <span className="count-badge">{t('anchored.segmentCount', { count: commit.links.length })}</span>
                      : <span className="anchor-none">{t('anchored.noContext')}</span>}
                    {origin && <span className="anchor-none" data-origin={origin.push.state}>{t(`anchored.origin_${origin.push.state}`)}</span>}
                    {missing && <span className="anchor-none">{t('anchored.notPushed')}</span>}
                  </span>
                </span>
              </AppLink>
            </li>
          );
        })}
      </ol>
      {(hidden > 0 || more) && (
        <button className="ghost" disabled={loading} onClick={() => { setLimit(limit + COMMIT_STEP); if (hidden <= COMMIT_STEP) more?.(); }}>
          {hidden > 0 ? t('anchored.showMore', { count: hidden }) : t('anchored.showOlder')}
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
        {summary.shared && <span className="ref-badge joined">{summary.uploader_name || t('anchored.aMember')}</span>}
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

/** The notes written about one target, and the form to write one where no session explains it. */
function Notes({ repo, notes, target, canAdd: nothingExplains }: { repo: Repo; notes: AnchoredNote[]; target: AnchoredNoteTarget; canAdd: boolean }) {
  const t = useT();
  const me = useMe().data;
  // A note is written by a member, where no session explains the target.
  const canAdd = useContext(Member) && nothingExplains;
  const save = useSaveAnchoredNote();
  const remove = useDeleteAnchoredNote();
  const [editing, setEditing] = useState<string | null>(null);
  const [text, setText] = useState('');
  const own = anchoredNotesFor(notes, target);
  if (own.length === 0 && !canAdd) return null;
  const open = (id: string, body: string) => { save.reset(); remove.reset(); setEditing(id); setText(body); };
  const form = (noteId?: string) => (
    <form className="anchor-note-form" onSubmit={event => {
      event.preventDefault();
      save.mutate({ repoId: repo.id, noteId, target, body: text }, { onSuccess: () => setEditing(null) });
    }}>
      <textarea aria-label={t('anchored.noteText')} value={text} onChange={event => setText(event.target.value)} rows={4} maxLength={16000} required />
      <p className="anchor-card-meta">{t('anchored.noteMasked')}</p>
      <div className="anchor-note-actions">
        <button type="submit" disabled={save.isPending || !text.trim()}>{save.isPending ? t('common.saving') : t('common.save')}</button>
        <button type="button" className="ghost" disabled={save.isPending} onClick={() => setEditing(null)}>{t('common.cancel')}</button>
      </div>
      {save.error && <p role="alert" className="err">{save.error.message}</p>}
    </form>
  );
  return (
    <section className="anchor-notes" aria-label={t('anchored.notes')}>
      {own.map(note => (
        <article key={note.id} className="anchor-note" data-note={note.id}>
          <header className="anchor-card-head">
            <span className="ref-badge joined">{t('anchored.manualNote')}</span>
            <em>{note.author === me?.id ? t('anchored.you') : note.author_name || t('anchored.aMember')}</em>
            <time dateTime={note.updated_at}>{when(note.updated_at)}</time>
          </header>
          {editing === note.id ? form(note.id) : (
            <>
              <p className="anchor-note-body">{note.body}</p>
              {/* Only its author changes or removes a note. */}
              {note.author === me?.id && (
                <div className="anchor-note-actions">
                  <button className="ghost mini" onClick={() => open(note.id, note.body)}>{t('common.edit')}</button>
                  <button className="ghost mini" disabled={remove.isPending} onClick={() => remove.mutate({ repoId: repo.id, noteId: note.id })}>{t('common.delete')}</button>
                </div>
              )}
            </>
          )}
        </article>
      ))}
      {remove.error && <p role="alert" className="err">{remove.error.message}</p>}
      {canAdd && editing !== 'new' && <button className="ghost" onClick={() => open('new', '')}>{t('anchored.writeNote')}</button>}
      {editing === 'new' && form()}
    </section>
  );
}

/** Commits in a detail pane: each with the segments that end at it, or that it has none. */
function CommitRefs({ shas, index, href }: { shas: string[]; index: AnchoredIndex; href: AnchoredHref }) {
  const t = useT();
  return (
    <ul className="anchor-refs">
      {shas.map(sha => {
        const commit = index.commitBySha.get(sha);
        return (
          <li key={sha}>
            <AppLink href={href({ kind: 'commit', sha })}><code>{anchoredShortSha(sha)}</code></AppLink>{' '}
            <span>{commit?.subject}</span>{' '}
            {commit?.links.length
              ? commit.links.map(link => (
                <AppLink key={`${link.provider}:${link.session_id}#${link.segment_no}`} className="ref-badge" title={link.session_id}
                  href={href({ kind: 'session', provider: link.provider, id: link.session_id, segment: link.segment_no })}>
                  {anchoredShortId(link.session_id)} · {t('anchored.segmentNo', { no: link.segment_no })}
                </AppLink>
              ))
              : <span className="anchor-none">{t('anchored.noContext')}</span>}
          </li>
        );
      })}
    </ul>
  );
}

function PullLink({ pull, href }: { pull: RemotePull; href: AnchoredHref }) {
  return <AppLink className="ref-badge" href={href({ kind: 'pull', number: pull.number })} title={pull.title}>#{pull.number}</AppLink>;
}

/** What lies between the commit chosen to compare with and the opened one: its commits, newest first, each with the segments that explain it. */
function Compare({ index, base, sha, href }: { index: AnchoredIndex; base: string; sha: string; href: AnchoredHref }) {
  const t = useT();
  if (base === sha) return <p className="anchor-card-meta anchor-compare-pick">{t('anchored.comparePick')}</p>;
  const range = anchoredCompare(index, base, sha);
  return (
    <section className="anchor-use anchor-compare" aria-label={t('anchored.compare')}>
      <h6>
        {t('anchored.compare')}: <AppLink href={href({ kind: 'commit', sha: base, base })}><code>{anchoredShortSha(base)}</code></AppLink> → <code>{anchoredShortSha(sha)}</code>
        {range.comparable && <span className="anchor-card-meta"> · {t('anchored.compareCount', { count: range.shas.length })}</span>}
      </h6>
      {!range.comparable ? <p className="anchor-card-meta">{t('anchored.compareUnknown')}</p>
        : range.shas.length === 0 ? <p className="anchor-card-meta">{t('anchored.compareNone')}</p>
          : <CommitRefs shas={range.shas} index={index} href={href} />}
      {range.comparable && !range.complete && <p className="anchor-card-meta">{t('anchored.compareIncomplete')}</p>}
    </section>
  );
}

/** Whose computer a collector runs on, as far as the reader is told. */
function collectorName(collector: PushCollector | undefined, t: ReturnType<typeof useT>): string {
  const parts = [collector?.by, collector?.machine].filter(Boolean);
  return parts.length > 0 ? parts.join(' · ') : t('anchored.originSomeComputer');
}

/** Why a commit has no session: where it was pushed from, in a sentence. */
function OriginNote({ origin }: { origin: CommitOrigin }) {
  const t = useT();
  const state = origin.push.state;
  return (
    <p className="anchor-card-meta" data-origin-note={state}>
      {t(`anchored.originDetail_${state}`, { who: state === 'unknown' ? origin.waiting.map(collector => collectorName(collector, t)).join(', ') : collectorName(origin.collector, t) })}
      {origin.push.claimed_at && <> {t('anchored.originTimes')} <time dateTime={origin.push.at}>{when(origin.push.at, true)}</time> → <time dateTime={origin.push.claimed_at}>{when(origin.push.claimed_at, true)}</time></>}
      {state === 'cxt_error' && origin.collector?.run_error ? <> {origin.collector.run_error}</> : null}
    </p>
  );
}

function CommitDetail({ repo, index, notes, sha, base, href, onHighlight, origin }: {
  repo: Repo; index: AnchoredIndex; notes: AnchoredNote[]; sha: string; href: AnchoredHref; onHighlight: (shas: string[]) => void;
  /** Where the commit was pushed from, when the server knows. */
  origin?: CommitOrigin;
  /** The commit this one is compared with, when one was chosen. */
  base?: string;
}) {
  const t = useT();
  const commit: AnchoredCommitRow | undefined = index.commitBySha.get(sha);
  // The reports name the commit a session made; a copy on the remote leads back to it.
  const made = [...new Set((commit?.links ?? []).map(link => link.copyOf ?? sha))];
  const revisions = (commit?.links ?? []).map(link => `${anchoredKey(link.provider, link.session_id)}@${index.sessions.get(anchoredKey(link.provider, link.session_id))?.revision ?? 0}`).join(',');
  // A commit no segment ends at has nothing to read.
  const segmentsQ = useAnchoredCommit(repo.id, made[0] ?? null, revisions);
  const summariesQ = useAnchoredCommitSummaries(repo.id, made[0] ?? null, revisions);
  const merged = commit?.merged !== undefined ? index.pulls.find(pull => pull.number === commit.merged) : undefined;
  return (
    <>
      <h5 className="anchor-detail-title">
        {commit?.subject || t('anchored.noSubject')} <code>{anchoredShortSha(sha)}</code>
      </h5>
      {commit && (
        <p className="anchor-card-meta">
          {commit.author && <><em>{commit.author}</em> · </>}
          {commit.at && <time dateTime={commit.at}>{when(commit.at)}</time>}
          {index.remoteRead && !commit.remote && <> · {t('anchored.notPushed')}</>}
          {commit.heads.map(name => <AppLink key={name} className="ref-badge branch" href={href({ kind: 'branch', name })}>{name}</AppLink>)}
          {commit.pulls.map(number => index.pulls.find(pull => pull.number === number)).map(pull => pull && <PullLink key={pull.number} pull={pull} href={href} />)}
        </p>
      )}
      <p className="anchor-compare-bar">
        {base
          ? <AppLink className="ghost" href={href({ kind: 'commit', sha })}>{t('anchored.compareClear')}</AppLink>
          : <AppLink className="ghost" href={href({ kind: 'commit', sha, base: sha })}>{t('anchored.compareSet')}</AppLink>}
      </p>
      {base && <Compare index={index} base={base} sha={sha} href={href} />}
      <ForkBox sha={sha} sessions={(commit?.links ?? []).map(link => ({ provider: link.provider, id: link.session_id, at: link.copyOf ?? sha }))} />
      {(commit?.links.length ?? 0) === 0 && <p className="empty-box">{t('anchored.noContext')}</p>}
      {(commit?.links.length ?? 0) === 0 && origin && <OriginNote origin={origin} />}
      <ul className="anchor-cards">
        {(commit?.links ?? []).map(link => {
          const key = anchoredKey(link.provider, link.session_id);
          const summary = index.sessions.get(key);
          if (!summary) return null;
          const range = link.copyOf && link.copyOf !== made[0] ? undefined
            : segmentsQ.data?.find(entry => entry.provider === link.provider && entry.session_id === link.session_id)?.segment;
          const shared = anchoredSegmentCommits(summary, link.segment_no);
          const spawned = (index.children.get(key) ?? []).filter(child => child.spawned_in?.segment_no === link.segment_no);
          return (
            <li key={`${key}#${link.segment_no}`} className="anchor-card" tabIndex={0} data-segment={`${key}#${link.segment_no}`}
              onMouseEnter={() => onHighlight(shared)} onMouseLeave={() => onHighlight([])} onFocus={() => onHighlight(shared)} onBlur={() => onHighlight([])}>
              <SessionCard summary={summary} index={index} href={href} selection={{ kind: 'session', provider: link.provider, id: link.session_id, segment: link.segment_no }}>
                <p className="anchor-card-meta">
                  {t('anchored.segmentOf', { no: link.segment_no, total: summary.segments })} · {t(`anchored.rule_${link.rule}`)}
                  {link.copyOf && <> · {t('anchored.copyOf')} <code>{anchoredShortSha(link.copyOf)}</code></>}
                  {range && <> · {t('anchored.commandCount', { count: range.commands })}</>}
                  {range?.started_at && <> · <time dateTime={range.started_at}>{when(range.started_at, true)}</time></>}
                  {range?.ended_at && <> – <time dateTime={range.ended_at}>{when(range.ended_at, true)}</time></>}
                </p>
                {shared.length > 1 && (
                  <p className="anchor-card-meta">
                    {t('anchored.sharedCommits')}{' '}
                    {shared.filter(other => other !== (link.copyOf ?? sha)).map(other => (
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
                <SummaryBox summary={summariesQ.data?.find(entry => entry.provider === link.provider && entry.session_id === link.session_id && entry.commit === (link.copyOf ?? sha))} />
              </SessionCard>
            </li>
          );
        })}
      </ul>
      {segmentsQ.error && <p role="alert" className="err">{segmentsQ.error.message}</p>}
      {merged && (
        <section className="anchor-use">
          <h6>{t('anchored.mergedPull')} <PullLink pull={merged} href={href} /> <span className="anchor-card-meta">{merged.title}</span></h6>
          <CommitRefs shas={[...merged.commits].reverse()} index={index} href={href} />
        </section>
      )}
      <Notes repo={repo} notes={notes} target={{ kind: 'commit', commit: sha }} canAdd={(commit?.links.length ?? 0) === 0} />
    </>
  );
}

/**
 * Starting new work from a commit. A browser cannot make a branch or write a session file on the reader's computer, so
 * this shows the two commands that do: a Git branch at the commit, and, for each session a segment of which ends
 * here, a new session of the tool that continues that conversation up to the commit. A session is named with the commit
 * its own report names, which for a copy on the remote is the commit it was copied from.
 */
function ForkBox({ sha, sessions }: { sha: string; sessions: { provider: string; id: string; at: string }[] }) {
  const t = useT();
  const seen = new Set<string>();
  const continued = sessions.filter(session => {
    const key = `${session.provider}:${session.id}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  return (
    <details className="anchor-fork" data-fork={sha}>
      <summary className="ghost">{t('anchored.fork')}</summary>
      <p className="anchor-card-meta">{t('anchored.forkLead')}</p>
      <p className="anchor-card-meta">{t('anchored.forkBranch')}</p>
      <pre className="anchor-fork-command"><code>{`cxt segments fork --at ${sha} --branch <name>`}</code></pre>
      {continued.length > 0 && <p className="anchor-card-meta">{t('anchored.forkConversation')}</p>}
      {continued.map(session => (
        <pre key={`${session.provider}:${session.id}`} className="anchor-fork-command"><code>{`cxt segments fork ${session.provider} ${session.id} --at ${session.at}`}</code></pre>
      ))}
    </details>
  );
}

/** What the session's CLI wrote down when the commit was made: what was done, what is worth keeping, what is left. */
function SummaryBox({ summary }: { summary?: AnchoredSummary }) {
  const t = useT();
  if (!summary) return null;
  const { summary: text, key_facts: facts, open_tasks: tasks } = summary.body;
  return (
    <details className="memory-box anchor-summary" data-summary={`${summary.provider}:${summary.session_id}`}>
      <summary className="label">◆ {t('anchored.summary')}</summary>
      {text.trim() && <section className="memory-section"><Markdown text={text} /></section>}
      {facts.length > 0 && (
        <section className="memory-section">
          <h4 className="memory-section-label">{t('anchored.summaryFacts')}</h4>
          <ul>{facts.map((fact, at) => <li key={at}>{fact}</li>)}</ul>
        </section>
      )}
      {tasks.length > 0 && (
        <section className="memory-section">
          <h4 className="memory-section-label">{t('anchored.summaryTasks')}</h4>
          <ul className="tasks">{tasks.map((task, at) => <li key={at}>{task}</li>)}</ul>
        </section>
      )}
    </details>
  );
}

/** Where a hit leads: the pull request, branch, commit, part of a session, or what a note was written about. */
function hitSelection(hit: AnchoredSearchHit): AnchoredSelection | undefined {
  if (hit.kind === 'pull' && hit.pull) return { kind: 'pull', number: hit.pull };
  if (hit.kind === 'branch' && hit.branch) return { kind: 'branch', name: hit.branch };
  if (hit.kind === 'commit' && hit.commit) return { kind: 'commit', sha: hit.commit };
  if (hit.kind === 'segment' && hit.provider && hit.session_id) {
    return { kind: 'session', provider: hit.provider, id: hit.session_id, ...(hit.segment_no ? { segment: hit.segment_no } : hit.rest ? { rest: true as const } : {}) };
  }
  const target = hit.kind === 'note' ? hit.target : undefined;
  if (target?.kind === 'commit') return { kind: 'commit', sha: target.commit };
  if (target?.kind === 'branch') return { kind: 'branch', name: target.branch };
  if (target?.kind === 'worktree') return { kind: 'worktree', name: target.worktree, ...(target.machine ? { machine: target.machine } : {}) };
  return undefined;
}

/** What the query was found in. Every hit begins with what it is: a pull request, a branch, a commit, a segment or a note. */
function SearchResults({ repo, query, href }: { repo: Repo; query: string; href: AnchoredHref }) {
  const t = useT();
  const searchQ = useAnchoredSearch(repo.id, query);
  const hits = searchQ.data?.hits ?? [];
  return (
    <section className="anchor-detail" aria-label={t('anchored.searchResults', { query })}>
      <h5 className="anchor-detail-title">{t('anchored.searchResults', { query })}</h5>
      {query.trim().length < 2 && <p className="empty-box">{t('anchored.searchShort')}</p>}
      {searchQ.isLoading && <div className="skel" role="status" aria-label={t('anchored.loading')} style={{ height: 60 }} />}
      {searchQ.error && <p role="alert" className="err">{searchQ.error.message}</p>}
      {searchQ.data && hits.length === 0 && <p className="empty-box">{t('anchored.searchNone')}</p>}
      <ul className="anchor-refs anchor-hits">
        {hits.map((hit, at) => {
          const selection = hitSelection(hit);
          const where = hit.kind === 'pull' ? `#${hit.pull}`
            : hit.kind === 'commit' && hit.commit ? anchoredShortSha(hit.commit)
            : hit.kind === 'segment' && hit.session_id ? `${anchoredShortId(hit.session_id)} · ${hit.segment_no ? t('anchored.segmentNo', { no: hit.segment_no }) : t('anchored.afterLastCommit')}`
            : hit.kind === 'note' && hit.target ? hit.target.kind === 'commit' ? anchoredShortSha(hit.target.commit) : hit.target.kind === 'branch' ? hit.target.branch : hit.target.worktree
            : '';
          return (
            <li key={`${hit.kind}:${at}`} data-hit={hit.kind}>
              <span className={`ref-badge hit-${hit.kind}`}>{t(`anchored.hit_${hit.kind}`)}</span>{' '}
              {selection ? <AppLink href={href(selection)}><code>{where || hit.title}</code></AppLink> : <code>{where || hit.title}</code>}{' '}
              {where && hit.title && <span>{hit.title}</span>}
              {hit.at && <> <time dateTime={hit.at}>{when(hit.at, true)}</time></>}
              {hit.snippet && <p className="anchor-card-meta">{hit.snippet}</p>}
            </li>
          );
        })}
      </ul>
      {searchQ.data?.truncated && <p className="anchor-card-meta">{t('anchored.searchTruncated')}</p>}
    </section>
  );
}

/** The caller's own work before a push: commits the remote does not hold, and what is not committed yet. */
/** The archived sessions: the only place they are listed. Each is opened from here and read as far as the reader may. */
function ArchivedDetail({ index, href }: { index: AnchoredIndex; href: AnchoredHref }) {
  const t = useT();
  const sessions = [...index.archived.values()];
  return (
    <section className="anchor-detail" aria-label={t('anchored.archivedSessions')}>
      <h5 className="anchor-detail-title">{t('anchored.archivedSessions')}</h5>
      <p className="anchor-card-meta">{t('anchored.archivedLead')}</p>
      {sessions.length === 0 ? <p className="anchor-nav-empty">{t('anchored.archivedEmpty')}</p> : (
        <ul className="anchor-archived">
          {sessions.map(session => (
            <li key={anchoredKey(session.provider, session.session_id)}>
              <AppLink href={href({ kind: 'session', provider: session.provider, id: session.session_id })}>
                <span className="anchor-provider" style={{ color: ANCHORED_PROVIDERS[session.provider]?.ink }}>{ANCHORED_PROVIDERS[session.provider]?.name ?? session.provider}</span>{' '}
                <code>{anchoredShortId(session.session_id)}</code>
              </AppLink>
              <span className="anchor-card-meta">
                {session.shared && <> · {session.uploader_name || t('anchored.aMember')}</>}
                {session.archived_at && <> · {t('anchored.archivedOn')} <time dateTime={session.archived_at}>{when(session.archived_at)}</time></>}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function HeldDetail({ index, href }: { index: AnchoredIndex; href: AnchoredHref }) {
  const t = useT();
  const { commits, uncommitted } = index.held;
  return (
    <section className="anchor-detail" aria-label={t('anchored.held')}>
      <h5 className="anchor-detail-title">{t('anchored.held')}</h5>
      <p className="anchor-card-meta">{t('anchored.heldLead')}</p>
      {!index.remoteRead && <p className="anchor-card-meta">{t('anchored.remoteNotRead')}</p>}
      {commits.length + uncommitted.length === 0 && <p className="empty-box">{t('anchored.heldEmpty')}</p>}
      {commits.length > 0 && (
        <section className="anchor-use">
          <h6>{t('anchored.heldCommits')} <span className="count-badge">{commits.length}</span></h6>
          <CommitRefs shas={commits} index={index} href={href} />
        </section>
      )}
      {uncommitted.length > 0 && (
        <section className="anchor-use">
          <h6>{t('anchored.heldUncommitted')} <span className="count-badge">{uncommitted.length}</span></h6>
          <UncommittedCards keys={uncommitted} index={index} href={href} places />
        </section>
      )}
    </section>
  );
}

function UncommittedCards({ keys, index, href, places = false }: {
  keys: string[]; index: AnchoredIndex; href: AnchoredHref;
  /** Also says where each part was worked on, for a list that is not of one place. */
  places?: boolean;
}) {
  const t = useT();
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
              {places && part && part.places.length > 0 && (
                <p className="anchor-card-meta">
                  {part.places.map((place, at) => (
                    <span key={at}>
                      {place.worktree && <>{t('anchored.worktreeTitle')} <code>{place.worktree}</code> </>}
                      {place.branch && <AppLink className="ref-badge branch" href={href({ kind: 'branch', name: place.branch })}>{place.branch}</AppLink>}{' '}
                    </span>
                  ))}
                </p>
              )}
            </SessionCard>
          </li>
        );
      })}
    </ul>
  );
}

function BranchUse({ repo, name, use, index, notes, href, earlier, explained }: {
  repo: Repo; name: string; use: AnchoredBranchUse; index: AnchoredIndex; notes: AnchoredNote[]; href: AnchoredHref; earlier: boolean; explained: boolean;
}) {
  const t = useT();
  const lifetime = use.lifetime;
  const none = use.sessions.length === 0 && !explained;
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
      {none && <p className="empty-box">{t('anchored.noContext')}</p>}
      <UncommittedCards keys={use.sessions} index={index} href={href} />
      <Notes repo={repo} notes={notes} target={{ kind: 'branch', branch: name, ...(lifetime ? { ledger_id: lifetime.ledger_id } : {}) }} canAdd={none} />
    </section>
  );
}

function BranchDetail({ repo, index, notes, name, href }: { repo: Repo; index: AnchoredIndex; notes: AnchoredNote[]; name: string; href: AnchoredHref }) {
  const t = useT();
  const branch = index.branches.find(row => row.name === name);
  const commits = anchoredBranchCommits(index, name);
  const explained = commits.shas.some(sha => (index.commitBySha.get(sha)?.links.length ?? 0) > 0);
  return (
    <>
      <h5 className="anchor-detail-title">
        {t('anchored.branchTitle')} <code>{name}</code> {branch?.pulls.map(pull => <PullLink key={pull.number} pull={pull} href={href} />)}
      </h5>
      {commits.shas.length > 0 && (
        <section className="anchor-use">
          <h6>{commits.pull ? t('anchored.pullCommits', { number: commits.pull.number }) : t('anchored.branchCommits', { base: index.defaultBranch })}</h6>
          <CommitRefs shas={commits.shas} index={index} href={href} />
          {!commits.complete && <p className="anchor-card-meta">{t('anchored.commitsIncomplete')}</p>}
        </section>
      )}
      <p className="anchor-card-meta">{t('anchored.branchLead')}</p>
      {!branch && <p className="empty-box">{t('anchored.noContext')}</p>}
      {branch && <BranchUse repo={repo} name={name} use={branch.current} index={index} notes={notes} href={href} earlier={false} explained={explained} />}
      {branch?.earlier.map(use => <BranchUse key={use.lifetime!.ledger_id} repo={repo} name={name} use={use} index={index} notes={notes} href={href} earlier explained={false} />)}
    </>
  );
}

function WorktreeDetail({ repo, index, notes, name, machine, href }: {
  repo: Repo; index: AnchoredIndex; notes: AnchoredNote[]; name: string; machine: string; href: AnchoredHref;
}) {
  const t = useT();
  const worktree = index.worktrees.find(row => row.name === name && row.machine === machine);
  const sessions = worktree?.sessions ?? [];
  return (
    <>
      <h5 className="anchor-detail-title">
        {t('anchored.worktreeTitle')} <code>{name}</code> {machine && <em>{machine}</em>}
        {worktree?.removed && <span className="ref-badge archived">{t('anchored.removedWorktree')}</span>}
      </h5>
      {worktree?.current && (
        <p className="anchor-card-meta">
          {t('anchored.checkedOut')} <AppLink className="ref-badge branch" href={href({ kind: 'branch', name: worktree.current })}>{worktree.current}</AppLink>
        </p>
      )}
      <p className="anchor-card-meta">{t('anchored.worktreeLead')}</p>
      {worktree && worktree.branches.length > 0 && (
        <p className="anchor-card-meta">
          {t('anchored.branches')}:{' '}
          {worktree.branches.map(branch => (
            <AppLink key={branch} className="ref-badge branch" href={href({ kind: 'branch', name: branch })}>{branch}</AppLink>
          ))}
        </p>
      )}
      {sessions.length === 0 && <p className="empty-box">{t('anchored.noContext')}</p>}
      <UncommittedCards keys={sessions} index={index} href={href} />
      <Notes repo={repo} notes={notes} target={{ kind: 'worktree', worktree: name, ...(machine ? { machine } : {}) }} canAdd={sessions.length === 0} />
    </>
  );
}

function PullDetail({ index, number, href }: { index: AnchoredIndex; number: number; href: AnchoredHref }) {
  const t = useT();
  const pull = index.pulls.find(candidate => candidate.number === number);
  if (!pull) return <p className="empty-box">{t('anchored.pullUnknown', { number })}</p>;
  return (
    <>
      <h5 className="anchor-detail-title">
        {t('anchored.pullTitle', { number })} <span className="ref-badge">{t(`anchored.pullState_${pull.state}`)}</span>
      </h5>
      <p className="anchor-card-meta">
        {pull.title} ·{' '}
        {pull.fork ? <code>{pull.head_ref}</code> : <AppLink className="ref-badge branch" href={href({ kind: 'branch', name: pull.head_ref })}>{pull.head_ref}</AppLink>}
        {' → '}
        <AppLink className="ref-badge branch" href={href({ kind: 'branch', name: pull.base_ref })}>{pull.base_ref}</AppLink>
        {pull.merge_sha && <> · {t('anchored.mergedAs')} <AppLink href={href({ kind: 'commit', sha: pull.merge_sha })}><code>{anchoredShortSha(pull.merge_sha)}</code></AppLink></>}
      </p>
      <section className="anchor-use">
        <h6>{t('anchored.pullCommits', { number })}</h6>
        {pull.commits.length === 0 && <p className="empty-box">{t('anchored.noCommits')}</p>}
        <CommitRefs shas={[...pull.commits].reverse()} index={index} href={href} />
        {pull.truncated && <p className="anchor-card-meta">{t('anchored.commitsIncomplete')}</p>}
      </section>
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
