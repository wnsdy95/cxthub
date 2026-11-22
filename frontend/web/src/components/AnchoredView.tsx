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
import type { AnchoredSelection } from '../route';
import type { AnchoredNote, AnchoredNoteTarget, AnchoredSessionSummary, RemotePull, Repo } from '../types';
import {
  useAnchoredCommit, useAnchoredNotes, useAnchoredSessions, useBranchLifetimes, useDeleteAnchoredNote, useMe, useRemoteGit,
  useReportedWorktrees, useSaveAnchoredNote,
} from '../hooks';
import {
  ANCHORED_PROVIDERS, anchoredBranchCommits, anchoredKey, anchoredNotesFor, anchoredSegmentCommits, anchoredShortId, anchoredShortSha, buildAnchoredIndex,
  type AnchoredBranchUse, type AnchoredCommitRow, type AnchoredIndex, type AnchoredRemote,
} from '../anchoredSessions';
import { layoutGitGraph, type GitRow } from '../anchoredGraph';
import { when } from '../snapshotFormat';
import { useT } from '../i18n';
import { AppLink } from './AppLink';
import { AnchoredSessionView } from './AnchoredSessionView';

export type AnchoredHref = (selection?: AnchoredSelection) => string;
/** Whether the reader is a member: one who has sessions and worktrees of their own and may write notes. */
const Member = createContext(true);
const COMMIT_STEP = 100;
const LANE = 14;
const ROW = 46;
const LANE_COLORS = ['#16181d', '#2e7d5b', '#8250df', '#b4452c', '#0969da', '#bf8700'];

export function AnchoredView({ repo, member = true, selection, href }: {
  repo: Repo; selection?: AnchoredSelection; href: AnchoredHref;
  /** False for a reader who only views the repository: a viewer, or a visitor who is not signed in. Such a reader is shown what is pushed. */
  member?: boolean;
}) {
  const t = useT();
  const sessionsQ = useAnchoredSessions(repo.id);
  const lifetimesQ = useBranchLifetimes(repo.id);
  const worktreesQ = useReportedWorktrees(repo.id);
  const notesQ = useAnchoredNotes(repo.id);
  const remoteQ = useRemoteGit(repo.id);
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
  // The commits of the selected branch or pull request are marked in the graph.
  const marked = shown.kind === 'branch' ? anchoredBranchCommits(index, shown.name).shas
    : shown.kind === 'pull' ? index.pulls.find(pull => pull.number === shown.number)?.commits ?? [] : [];
  const refresh = () => {
    void sessionsQ.refetch(); void lifetimesQ.refetch(); void worktreesQ.refetch(); void notesQ.refetch(); void remoteQ.refetch();
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
        <div className="anchor-bar">
          <span>
            {t(member ? 'anchored.ownOnly' : 'anchored.openOnly')}{' '}
            {remoteQ.error ? t('anchored.remoteError') : synced ? <>{t('anchored.remoteReadAt')} <time dateTime={synced}>{when(synced, true)}</time></> : remoteQ.isLoading ? null : t('anchored.remoteNotRead')}
          </span>
          <button className="ghost" onClick={refresh} disabled={sessionsQ.isFetching}>
            {sessionsQ.isFetching ? t('anchored.refreshing') : t('anchored.refresh')}
          </button>
        </div>
        {lifetimesQ.error && <p role="alert" className="err">{lifetimesQ.error.message}</p>}
        {index.sessions.size === 0 && (member ? (
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
            segment={shown.segment} rest={shown.rest} index={index} href={href} />
        ) : shown.kind === 'held' ? (
          <HeldDetail index={index} href={href} />
        ) : (
          <div className="anchor-split">
            <AnchoredCommits index={index} selected={shown.kind === 'commit' ? shown.sha : ''} highlighted={highlighted} marked={marked} href={href}
              more={remoteQ.hasNextPage ? () => void remoteQ.fetchNextPage() : undefined} loading={remoteQ.isFetchingNextPage} />
            <section className="anchor-detail" aria-label={t('anchored.detailAria')}>
              {shown.kind === 'commit' && <CommitDetail key={shown.sha} repo={repo} index={index} notes={notes} sha={shown.sha} href={href} onHighlight={setHighlighted} />}
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

/** The lanes of one row: the lines passing by, those meeting at the commit, and those leaving it. */
function Lanes({ row, width, missing }: { row: GitRow<AnchoredCommitRow>; width: number; missing: boolean }) {
  const mid = ROW / 2, x = laneX(row.lane);
  const lines: ReactNode[] = [];
  row.incoming.forEach((waiting, lane) => {
    if (waiting === null) return;
    if (waiting !== row.node.sha) lines.push(<line key={`pass${lane}`} x1={laneX(lane)} y1={0} x2={laneX(lane)} y2={ROW} stroke={laneColor(lane)} />);
    else if (lane === row.lane) lines.push(<line key="in" x1={x} y1={0} x2={x} y2={mid} stroke={laneColor(lane)} />);
    else lines.push(<path key={`join${lane}`} d={`M ${laneX(lane)} 0 C ${laneX(lane)} ${mid} ${x} ${mid * 0.4} ${x} ${mid}`} stroke={laneColor(lane)} fill="none" />);
  });
  if (row.outgoing[row.lane] != null) lines.push(<line key="out" x1={x} y1={mid} x2={x} y2={ROW} stroke={laneColor(row.lane)} />);
  else if (row.cut) lines.push(<line key="cut" x1={x} y1={mid} x2={x} y2={ROW} stroke={laneColor(row.lane)} strokeDasharray="2 3" />);
  for (const lane of row.branchesOut) {
    lines.push(<path key={`fork${lane}`} d={`M ${x} ${mid} C ${laneX(lane)} ${mid * 1.6} ${laneX(lane)} ${mid} ${laneX(lane)} ${ROW}`} stroke={laneColor(lane)} fill="none" />);
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

function AnchoredCommits({ index, selected, highlighted, marked, href, more, loading }: {
  index: AnchoredIndex; selected: string; highlighted: string[]; marked: string[]; href: AnchoredHref; more?: () => void; loading: boolean;
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
          return (
            <li key={commit.sha}>
              <AppLink className={`anchor-commit${on ? ' on' : ''}${highlighted.includes(commit.sha) ? ' hl' : ''}${marked.includes(commit.sha) ? ' in' : ''}`}
                href={href({ kind: 'commit', sha: commit.sha })} aria-current={on ? 'true' : undefined} data-commit={commit.sha} data-lane={row.lane}>
                <Lanes row={row} width={width} missing={missing} />
                <span className="anchor-commit-text">
                  <span className="anchor-commit-subject">
                    {commit.heads.map(name => <span key={name} className="ref-badge branch">{name}</span>)}
                    {commit.merged !== undefined && <span className="ref-badge">#{commit.merged}</span>}
                    {commit.subject || t('anchored.noSubject')}
                  </span>
                  <span className="anchor-commit-meta">
                    <code>{anchoredShortSha(commit.sha)}</code>
                    {commit.at && <time dateTime={commit.at}>{when(commit.at, true)}</time>}
                    {commit.links.length > 0
                      ? <span className="count-badge">{t('anchored.segmentCount', { count: commit.links.length })}</span>
                      : <span className="anchor-none">{t('anchored.noContext')}</span>}
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

function CommitDetail({ repo, index, notes, sha, href, onHighlight }: {
  repo: Repo; index: AnchoredIndex; notes: AnchoredNote[]; sha: string; href: AnchoredHref; onHighlight: (shas: string[]) => void;
}) {
  const t = useT();
  const commit: AnchoredCommitRow | undefined = index.commitBySha.get(sha);
  // The reports name the commit a session made; a copy on the remote leads back to it.
  const made = [...new Set((commit?.links ?? []).map(link => link.copyOf ?? sha))];
  const revisions = (commit?.links ?? []).map(link => `${anchoredKey(link.provider, link.session_id)}@${index.sessions.get(anchoredKey(link.provider, link.session_id))?.revision ?? 0}`).join(',');
  // A commit no segment ends at has nothing to read.
  const segmentsQ = useAnchoredCommit(repo.id, made[0] ?? null, revisions);
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
      {(commit?.links.length ?? 0) === 0 && <p className="empty-box">{t('anchored.noContext')}</p>}
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

/** The caller's own work before a push: commits the remote does not hold, and what is not committed yet. */
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
