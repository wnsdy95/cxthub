// AnchoredSessionView — one session: who spawned it, the parts it is cut into,
// the sessions spawned inside each part, the agents it exchanged calls with,
// and the whole conversation with a divider where each part begins.
import { useEffect, useMemo, useRef, useState } from 'react';
import type { AnchoredSelection } from '../route';
import type { AnchoredProvider, Repo } from '../types';
import { api } from '../api';
import { useAnchoredEvents, useAnchoredSession, useAnchoredSessionCalls, useSetAnchoredSessionArchived } from '../hooks';
import {
  ANCHORED_PROVIDERS, anchoredKey, anchoredOpenStarts, anchoredPartIndex, anchoredPartners, anchoredParts, anchoredShortId, anchoredShortSha,
  anchoredStart, anchoredStream, type AnchoredIndex, type AnchoredPart, type AnchoredPartner,
} from '../anchoredSessions';
import { DocumentReadScope } from '../documentReadScope';
import { DocumentEventReaderContext, type DocumentEventReader } from '../documentEventReader';
import { when } from '../snapshotFormat';
import { useT } from '../i18n';
import { AppLink } from './AppLink';
import { EventStream } from './EventStream';
import { CONVERSATION_MODES, DEFAULT_CONVERSATION_MODE, type ConversationMode } from '../conversationView';

type Href = (selection?: AnchoredSelection) => string;

export function AnchoredSessionView({ repo, provider, sessionId, segment, rest, index, href, manage = false }: {
  repo: Repo; provider: AnchoredProvider; sessionId: string; segment?: number; rest?: boolean; index: AnchoredIndex; href: Href;
  /** True for a reader who manages the repository and so archives and restores any member's session. */
  manage?: boolean;
}) {
  const t = useT();
  // An archived session is in no list of the index but the one of archived sessions.
  const listed = index.sessions.get(anchoredKey(provider, sessionId)) ?? index.archived.get(anchoredKey(provider, sessionId));
  const archive = useSetAnchoredSessionArchived();
  const [confirming, setConfirming] = useState(false);
  // The conversation opens with only what was said; the work behind an answer opens on demand.
  const [mode, setMode] = useState<ConversationMode>(DEFAULT_CONVERSATION_MODE);
  const sessionQ = useAnchoredSession(repo.id, provider, sessionId, listed?.revision);
  const body = sessionQ.data?.body;
  const parts = useMemo(() => (body ? anchoredParts(body) : []), [body]);
  // Another member's session is read as far as it is open; its agent calls stay with its uploader.
  const shared = Boolean(listed?.shared);
  // The calls a session received sit in other sessions' reports; the server gathers both directions.
  // Until that answer arrives, the calls this session made are shown from its own report.
  const reports = useMemo(() => [...index.sessions.values()].reduce((sum, summary) => sum + summary.revision, index.sessions.size), [index.sessions]);
  const callsQ = useAnchoredSessionCalls(repo.id, shared ? null : provider, shared ? null : sessionId, String(reports));
  const partners = useMemo(() => anchoredPartners(sessionId, callsQ.data ?? body?.agent_calls ?? []), [sessionId, callsQ.data, body]);
  if (sessionQ.isLoading) return <div className="skel" role="status" aria-label={t('anchored.loading')} style={{ height: 120 }} />;
  if (sessionQ.error || !body) {
    return (
      <p role="alert" className="err">
        {sessionQ.error?.message ?? t('anchored.sessionMissing')} <button onClick={() => void sessionQ.refetch()}>{t('context.retryRead')}</button>
      </p>
    );
  }
  const key = anchoredKey(provider, sessionId);
  const summary = listed;
  const archivedAt = sessionQ.data?.archived_at ?? listed?.archived_at;
  // Its uploader archives and restores a session, and so does whoever manages the repository.
  const mayArchive = Boolean(listed) && (!shared || manage);
  const setArchived = (archived: boolean) => archive.mutate({ repoId: repo.id, provider, sessionId, archived }, { onSuccess: () => setConfirming(false) });
  const children = index.children.get(key) ?? [];
  const first = anchoredPartIndex(parts, segment, rest);
  // There is a conversation to show from the first part on.
  const readable = Boolean(body.content) && anchoredStart(parts, first) !== undefined;
  const session = (part?: AnchoredPart): AnchoredSelection => ({
    kind: 'session', provider, id: sessionId,
    ...(part?.kind === 'segment' ? { segment: part.no } : part?.kind === 'uncommitted' ? { rest: true as const } : {}),
  });
  return (
    <article className="anchor-session">
      <header className="anchor-session-head">
        <p className="anchor-crumb">
          <AppLink href={href({ kind: 'all' })}>{t('anchored.allSessions')}</AppLink> /{' '}
          <span className="anchor-provider" style={{ color: ANCHORED_PROVIDERS[provider]?.ink }}>{ANCHORED_PROVIDERS[provider]?.name ?? provider}</span>{' '}
          <code>{sessionId}</code>
        </p>
        {shared && (
          <p className="anchor-card-meta anchor-shared">{t('anchored.sharedBy', { name: listed?.uploader_name || t('anchored.aMember') })}</p>
        )}
        <p className="anchor-card-meta">
          {body.machine && <><em>{body.machine}</em> · </>}
          {body.started_at && <time dateTime={body.started_at}>{when(body.started_at)}</time>}
          {body.ended_at && <> – <time dateTime={body.ended_at}>{when(body.ended_at)}</time></>}
          {(body.started_at || body.ended_at) && ' · '}
          {t('anchored.sessionCounts', { records: body.records, commands: body.commands, segments: body.segments.length })}
        </p>
        {body.parent && (
          <p className="anchor-card-meta">
            {t('anchored.spawnedBy')}{' '}
            {index.sessions.has(anchoredKey(provider, body.parent)) ? (
              <AppLink href={href({ kind: 'session', provider, id: body.parent,
                ...(summary?.spawned_in?.segment_no ? { segment: summary.spawned_in.segment_no } : summary?.spawned_in?.uncommitted ? { rest: true as const } : {}) })}>
                <code>{anchoredShortId(body.parent)}</code>
              </AppLink>
            ) : (
              <code title={t('anchored.parentNotReported')}>{anchoredShortId(body.parent)}</code>
            )}
            {summary?.spawned_in && <> · {summary.spawned_in.segment_no ? t('anchored.segmentNo', { no: summary.spawned_in.segment_no }) : t('anchored.afterLastCommit')} · {t(`anchored.spawn_${summary.spawned_in.rule}`)}</>}
          </p>
        )}
        {body.copied_records > 0 && <p className="anchor-card-meta">{t('anchored.copiedRecords', { count: body.copied_records })}</p>}
        {archivedAt && (
          <p className="anchor-card-meta anchor-archived-note">
            <span className="ref-badge archived">{t('anchored.archivedBadge')}</span>{' '}
            {t('anchored.archivedNote')} <time dateTime={archivedAt}>{when(archivedAt)}</time>
          </p>
        )}
        {mayArchive && (confirming ? (
          <p className="anchor-archive-confirm" role="group" aria-label={t(archivedAt ? 'anchored.restoreSession' : 'anchored.archiveSession')}>
            {t(archivedAt ? 'anchored.restoreWarning' : 'anchored.archiveWarning')}{' '}
            <button type="button" disabled={archive.isPending} onClick={() => setArchived(!archivedAt)}>
              {archive.isPending ? t('anchored.archiveSaving') : t(archivedAt ? 'anchored.restoreSession' : 'anchored.archiveSession')}
            </button>{' '}
            <button type="button" className="ghost" disabled={archive.isPending} onClick={() => setConfirming(false)}>{t('common.cancel')}</button>
          </p>
        ) : (
          <p className="anchor-archive-ask">
            <button type="button" className="ghost" onClick={() => { archive.reset(); setConfirming(true); }}>
              {t(archivedAt ? 'anchored.restoreSession' : 'anchored.archiveSession')}
            </button>
          </p>
        ))}
        {archive.error && <p role="alert" className="err">{archive.error.message}</p>}
      </header>

      <section className="anchor-parts" aria-label={t('anchored.parts')}>
        <h5 className="label">{t('anchored.parts')}</h5>
        <ol>
          {parts.map((part, position) => {
            const spawned = children.filter(child => (part.kind === 'segment' ? child.spawned_in?.segment_no === part.no : child.spawned_in?.uncommitted));
            return (
              <li key={part.kind + part.no} className={position === first ? 'on' : part.hidden ? 'hidden' : undefined}>
                {part.hidden ? (
                  <>
                    <span>{t('anchored.segmentNo', { no: part.no })}</span>
                    <span className="ref-badge pending">{t('anchored.notOpen')}</span>
                  </>
                ) : (
                  <>
                    <AppLink href={href(session(part))}>{part.kind === 'segment' ? t('anchored.segmentNo', { no: part.no }) : t('anchored.afterLastCommit')}</AppLink>
                    {!part.partial && <span className="anchor-card-meta"> · {t('anchored.commandCount', { count: part.range.commands })}</span>}
                    {part.partial && <span className="ref-badge pending">{t('anchored.partlyOpen')}</span>}
                  </>
                )}
                {part.commits.map(commit => (
                  <AppLink key={commit.sha} className="anchor-part-commit" href={href({ kind: 'commit', sha: commit.sha })}>
                    <code>{anchoredShortSha(commit.sha)}</code> {commit.subject}
                  </AppLink>
                ))}
                {part.places.map(place => (
                  <span key={place.worktree + '\n' + (place.branch ?? '')} className="anchor-part-place">
                    <AppLink href={href({ kind: 'worktree', name: place.worktree, ...(body.machine ? { machine: body.machine } : {}) })}>{place.worktree}</AppLink>
                    {place.branch && <AppLink className="ref-badge branch" href={href({ kind: 'branch', name: place.branch })}>{place.branch}</AppLink>}
                    {place.removed && <span className="ref-badge archived">{t('anchored.removedWorktree')}</span>}
                  </span>
                ))}
                {part.unplaced && <span className="ref-badge pending">{t('anchored.unplaced')}</span>}
                {spawned.length > 0 && (
                  <span className="anchor-part-spawned">
                    {t('anchored.spawnedHere')}{' '}
                    {spawned.map(child => (
                      <AppLink key={child.session_id} href={href({ kind: 'session', provider: child.provider, id: child.session_id })} title={child.session_id}>
                        <code>{anchoredShortId(child.session_id)}</code>
                      </AppLink>
                    ))}
                  </span>
                )}
              </li>
            );
          })}
        </ol>
        {children.some(child => !child.spawned_in) && (
          <p className="anchor-card-meta">
            {t('anchored.spawnedUnplaced')}{' '}
            {children.filter(child => !child.spawned_in).map(child => (
              <AppLink key={child.session_id} href={href({ kind: 'session', provider: child.provider, id: child.session_id })} title={child.session_id}>
                <code>{anchoredShortId(child.session_id)}</code>
              </AppLink>
            ))}
          </p>
        )}
      </section>

      {partners.length > 0 && <AgentCalls partners={partners} provider={provider} index={index} href={href} />}

      <section className="viewer anchor-conversation" aria-label={t('anchored.conversation')}
        style={{ ['--assistant-ink' as string]: ANCHORED_PROVIDERS[provider]?.ink ?? 'var(--text)' } as React.CSSProperties}>
        {(first > 0 || readable) && (
          <p className="anchor-card-meta anchor-conversation-head">
            {first > 0 && <AppLink href={href(session())}>{t('anchored.fromStart')}</AppLink>}
            {readable && (
              <select className="view-mode" aria-label={t('anchored.viewMode')} title={t('anchored.viewModeHint')} value={mode}
                onChange={event => setMode(event.target.value as ConversationMode)}>
                {CONVERSATION_MODES.map(value => <option key={value} value={value}>{t(`anchored.viewMode_${value}`)}</option>)}
              </select>
            )}
          </p>
        )}
        {readable && body.content ? (
          <AnchoredEvents key={first} repoId={repo.id} provider={provider} sessionId={sessionId} revision={listed?.revision ?? 0}
            total={body.content.events} parts={parts} first={first} mode={mode} />
        ) : (
          <p className="empty-box">{t('anchored.noContent')}</p>
        )}
      </section>
    </article>
  );
}

function PartDivider({ part }: { part: AnchoredPart }) {
  const t = useT();
  return (
    <div className={part.hidden ? 'session-divider anchor-divider hidden' : 'session-divider anchor-divider'} role="separator" data-part={part.kind === 'segment' ? part.no : 'rest'}>
      {part.kind === 'segment' ? t('anchored.segmentNo', { no: part.no }) : t('anchored.afterLastCommit')}
      {part.commits.map(commit => <code key={commit.sha}>{anchoredShortSha(commit.sha)}</code>)}
      {part.hidden && <span> · {t('anchored.notOpenYet')}</span>}
    </div>
  );
}

function AnchoredEvents({ repoId, provider, sessionId, revision, total, parts, first, mode }: {
  repoId: string; provider: AnchoredProvider; sessionId: string; revision: number; total: number; parts: AnchoredPart[]; first: number; mode: ConversationMode;
}) {
  const t = useT();
  const start = anchoredStart(parts, first) ?? total;
  const starts = useMemo(() => anchoredOpenStarts(parts), [parts]);
  const query = useAnchoredEvents(repoId, provider, sessionId, revision, start, starts, start < total);
  const runs = useMemo(() => query.data?.pages ?? [], [query.data]);
  // The conversation is read by the session, never by its document's hash; this key stands for it in the cache.
  const hash = `anchored:${provider}/${sessionId}@${revision}`;
  const reader = useMemo<DocumentEventReader>(() => ({
    key: hash,
    event: (position, signal) => api.anchoredEvent(repoId, provider, sessionId, position, signal),
    fragment: (position, offset, signal) => api.anchoredEventFragment(repoId, provider, sessionId, position, offset, signal),
  }), [hash, repoId, provider, sessionId]);
  const more = useRef<HTMLButtonElement>(null);
  const { hasNextPage, isFetching, fetchNextPage } = query;
  // The next page is read when the end of what is shown scrolls into view.
  useEffect(() => {
    const node = more.current;
    if (!node || !hasNextPage || isFetching || typeof IntersectionObserver === 'undefined') return;
    const observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) void fetchNextPage();
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, [hasNextPage, isFetching, fetchNextPage, runs.length]);
  return (
    <DocumentEventReaderContext.Provider value={reader}>
    <DocumentReadScope.Provider value={['anchored-events', repoId, hash, start]}>
      {anchoredStream(runs, parts, first).map(item => item.kind === 'divider'
        ? <PartDivider key={`divider:${item.part.kind}:${item.part.no}`} part={item.part} />
        : <EventStream key={`events:${item.part.kind}:${item.part.no}`} events={item.events} repoId={repoId} hash={hash} mode={mode}
          continues={item.part.firstEvent > 0} />)}
      {isFetching && <p role="status">{t('context.loadingEvents')}</p>}
      {query.error && (
        <p role="alert" className="err">
          {query.error.message} <button onClick={() => void (runs.length ? fetchNextPage() : query.refetch())}>{t('context.retryRead')}</button>
        </p>
      )}
      {hasNextPage && !isFetching && !query.error && (
        <button ref={more} className="ghost" onClick={() => void fetchNextPage()}>{t('anchored.loadMore')}</button>
      )}
    </DocumentReadScope.Provider>
    </DocumentEventReaderContext.Provider>
  );
}

/** Recorded calls between agents: first the other party, then the calls exchanged with it. */
function AgentCalls({ partners, provider, index, href }: { partners: AnchoredPartner[]; provider: AnchoredProvider; index: AnchoredIndex; href: Href }) {
  const t = useT();
  const [selected, setSelected] = useState('');
  const partner = partners.find(entry => entry.id === selected);
  return (
    <section className="anchor-calls" aria-label={t('anchored.agentCalls')}>
      <h5 className="label">{t('anchored.agentCalls')}</h5>
      <ul className="anchor-partners">
        {partners.map(entry => (
          <li key={entry.id}>
            <button className={`dl-btn${entry.id === selected ? ' on' : ''}`} aria-pressed={entry.id === selected} onClick={() => setSelected(entry.id === selected ? '' : entry.id)} title={entry.id}>
              {anchoredShortId(entry.id)} · {t('anchored.callCounts', { sent: entry.sent, received: entry.received })}
            </button>
          </li>
        ))}
      </ul>
      {partner && (
        <>
          <p className="anchor-card-meta">
            {index.sessions.has(anchoredKey(provider, partner.id))
              ? <AppLink href={href({ kind: 'session', provider, id: partner.id })}>{t('anchored.openPartner')} <code>{anchoredShortId(partner.id)}</code></AppLink>
              : t('anchored.partnerNotReported')}
          </p>
          <ol className="anchor-call-list">
            {partner.calls.map((call, position) => (
              <li key={position}>
                <span className="ref-badge">{call.from === partner.id ? t('anchored.callReceived') : t('anchored.callSent')}</span>
                <code>{call.tool}</code>
                {call.status && <em>{call.status}</em>}
                {call.at && <time dateTime={call.at}>{when(call.at, true)}</time>}
              </li>
            ))}
          </ol>
        </>
      )}
    </section>
  );
}
