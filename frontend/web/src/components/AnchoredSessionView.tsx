// AnchoredSessionView — one session: who spawned it, the parts it is cut into,
// the sessions spawned inside each part, the agents it exchanged calls with,
// and the whole conversation with a divider where each part begins.
import { useEffect, useMemo, useRef, useState } from 'react';
import type { AnchoredSelection } from '../route';
import type { AnchoredProvider, Repo } from '../types';
import { useAnchoredEvents, useAnchoredSession } from '../hooks';
import {
  ANCHORED_PROVIDERS, anchoredKey, anchoredPartIndex, anchoredPartners, anchoredParts, anchoredShortId, anchoredShortSha, anchoredStream,
  type AnchoredIndex, type AnchoredPart, type AnchoredPartner,
} from '../anchoredSessions';
import { DocumentReadScope } from '../documentReadScope';
import { when } from '../snapshotFormat';
import { useT } from '../i18n';
import { AppLink } from './AppLink';
import { EventStream } from './EventStream';

type Href = (selection?: AnchoredSelection) => string;

export function AnchoredSessionView({ repo, provider, sessionId, segment, rest, index, href }: {
  repo: Repo; provider: AnchoredProvider; sessionId: string; segment?: number; rest?: boolean; index: AnchoredIndex; href: Href;
}) {
  const t = useT();
  const sessionQ = useAnchoredSession(repo.id, provider, sessionId, index.sessions.get(anchoredKey(provider, sessionId))?.revision);
  const body = sessionQ.data?.body;
  const parts = useMemo(() => (body ? anchoredParts(body) : []), [body]);
  const partners = useMemo(() => anchoredPartners(sessionId, body?.agent_calls ?? []), [sessionId, body]);
  if (sessionQ.isLoading) return <div className="skel" role="status" aria-label={t('anchored.loading')} style={{ height: 120 }} />;
  if (sessionQ.error || !body) {
    return (
      <p role="alert" className="err">
        {sessionQ.error?.message ?? t('anchored.sessionMissing')} <button onClick={() => void sessionQ.refetch()}>{t('context.retryRead')}</button>
      </p>
    );
  }
  const key = anchoredKey(provider, sessionId);
  const summary = index.sessions.get(key);
  const children = index.children.get(key) ?? [];
  const first = anchoredPartIndex(parts, segment, rest);
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
      </header>

      <section className="anchor-parts" aria-label={t('anchored.parts')}>
        <h5 className="label">{t('anchored.parts')}</h5>
        <ol>
          {parts.map((part, position) => {
            const spawned = children.filter(child => (part.kind === 'segment' ? child.spawned_in?.segment_no === part.no : child.spawned_in?.uncommitted));
            return (
              <li key={part.kind + part.no} className={position === first ? 'on' : undefined}>
                <AppLink href={href(session(part))}>{part.kind === 'segment' ? t('anchored.segmentNo', { no: part.no }) : t('anchored.afterLastCommit')}</AppLink>
                <span className="anchor-card-meta"> · {t('anchored.commandCount', { count: part.range.commands })}</span>
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
        {first > 0 && (
          <p className="anchor-card-meta">
            <AppLink href={href(session())}>{t('anchored.fromStart')}</AppLink>
          </p>
        )}
        {body.content ? (
          <AnchoredEvents key={first} repoId={repo.id} hash={body.content.doc} total={body.content.events} parts={parts} first={first} />
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
    <div className="session-divider anchor-divider" role="separator" data-part={part.kind === 'segment' ? part.no : 'rest'}>
      {part.kind === 'segment' ? t('anchored.segmentNo', { no: part.no }) : t('anchored.afterLastCommit')}
      {part.commits.map(commit => <code key={commit.sha}>{anchoredShortSha(commit.sha)}</code>)}
    </div>
  );
}

function AnchoredEvents({ repoId, hash, total, parts, first }: { repoId: string; hash: string; total: number; parts: AnchoredPart[]; first: number }) {
  const t = useT();
  const start = parts[first]?.firstEvent ?? 0;
  const query = useAnchoredEvents(repoId, hash, start, start < total);
  const events = useMemo(() => query.data?.pages.flatMap(page => page.events) ?? [], [query.data]);
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
  }, [hasNextPage, isFetching, fetchNextPage, events.length]);
  return (
    <DocumentReadScope.Provider value={['anchored-events', repoId, hash, start]}>
      {anchoredStream(events, parts, first).map(item => item.kind === 'divider'
        ? <PartDivider key={`divider:${item.part.kind}:${item.part.no}`} part={item.part} />
        : <EventStream key={`events:${item.part.kind}:${item.part.no}`} events={item.events} repoId={repoId} hash={hash} />)}
      {isFetching && <p role="status">{t('context.loadingEvents')}</p>}
      {query.error && (
        <p role="alert" className="err">
          {query.error.message} <button onClick={() => void (events.length ? fetchNextPage() : query.refetch())}>{t('context.retryRead')}</button>
        </p>
      )}
      {hasNextPage && !isFetching && !query.error && (
        <button ref={more} className="ghost" onClick={() => void fetchNextPage()}>{t('anchored.loadMore')}</button>
      )}
    </DocumentReadScope.Provider>
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
