import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import type { GitAssociationFilter, GitAssociationKind } from '../types';

export function GitAssociations({ repoId }: { repoId: string }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [kind, setKind] = useState<GitAssociationKind>('integration');
  const [filter, setFilter] = useState<GitAssociationFilter>('pending');
  const [cursor, setCursor] = useState('');
  const status = useQuery({
    queryKey: ['git-associations', repoId, kind, filter, cursor],
    queryFn: ({ signal }) => api.gitAssociations(repoId, kind, filter, cursor, signal),
    enabled: open,
    staleTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchOnMount: false,
    refetchInterval: false,
  });
  const states: Record<string, string> = {
    waiting: t('promotion.waiting'), retrying: t('promotion.retrying'),
    processing: t('gitAssociations.processing'), attention: t('promotion.attention'),
    completed: t('gitAssociations.completed'), discovered: t('gitAssociations.discovered'),
    admitted: t('gitAssociations.admitted'),
  };
  const discoveryStates: Record<string, string> = {
    no_origin: t('gitAssociations.noOrigin'), unavailable: t('gitAssociations.unavailable'),
    not_started: t('gitAssociations.notStarted'), scanning: t('gitAssociations.scanning'),
    checkpoint_recorded: t('gitAssociations.checkpoint'),
  };
  const headStates: Record<string, string> = {
    unavailable: t('gitAssociations.unavailable'), not_started: t('gitAssociations.notStarted'),
    waiting: t('promotion.waiting'), running: t('promotion.running'),
    retrying: t('promotion.retrying'), completed: t('gitScans.checked'),
  };
  const reasons: Record<string, string> = {
    waiting_admission: t('gitAssociations.waitingAdmission'),
    lease_expired: t('gitAssociations.expiredLease'),
    awaiting_bound_head: t('gitAssociations.awaitingHead'),
    evidence_pending: t('gitAssociations.evidencePending'),
    range_discontinuous: t('gitAssociations.discontinuous'),
    source_absent: t('gitAssociations.sourceAbsent'),
    already_present: t('gitAssociations.alreadyPresent'),
    evidence_limit: t('gitAssociations.evidenceLimit'),
    integrity_check_failed: t('promotion.integrity'),
    authorization_or_state_changed: t('gitAssociations.stateChanged'),
    provider_scope_changed: t('gitAssociations.providerChanged'),
    temporary_failure: t('promotion.temporary'),
    projection_limit: t('gitAssociations.projectionLimit'),
    temporary_provider_or_storage_failure: t('promotion.temporary'),
  };
  const label = (labels: Record<string, string>, value: string) => Object.hasOwn(labels, value) ? labels[value] : t('gitAssociations.unknown');
  const reason = (value: string) => Object.hasOwn(reasons, value) ? reasons[value] : t('gitAssociations.unknownReason');
  const data = status.isError ? undefined : status.data;
  return <details className="git-associations" open={open} onToggle={event => setOpen(event.currentTarget.open)}>
    <summary>{t('gitAssociations.title')}</summary>
    {open && <>
      <p>{t('gitAssociations.scope')}</p>
      <p>{t('gitAssociations.historical')}</p>
      <div className="git-association-controls">
        <label>{t('gitAssociations.kind')}
          <select value={kind} onChange={event => { setKind(event.target.value as GitAssociationKind); setCursor(''); }}>
            <option value="publication">{t('gitAssociations.publications')}</option>
            <option value="integration">{t('gitAssociations.integrations')}</option>
            <option value="memory">{t('gitAssociations.memories')}</option>
          </select>
        </label>
        <label>{t('gitAssociations.filter')}
          <select value={filter} onChange={event => { setFilter(event.target.value as GitAssociationFilter); setCursor(''); }}>
            <option value="pending">{t('gitAssociations.pending')}</option>
            <option value="all">{t('common.showAll')}</option>
          </select>
        </label>
        <button disabled={status.isFetching} onClick={() => void status.refetch()}>{t('gitAssociations.refresh')}</button>
      </div>
      <p>{t('gitAssociations.readOnly')}</p>
      {status.isFetching && <p role="status">{t('gitAssociations.loading')}</p>}
      {status.isError && <p role="alert" className="err">{t('gitAssociations.loadError')} <button onClick={() => void status.refetch()}>{t('context.retryRead')}</button></p>}
      {data && <>
        <p className="git-association-last-read">{t('gitAssociations.lastRead')}: <time dateTime={new Date(status.dataUpdatedAt).toISOString()}>{new Date(status.dataUpdatedAt).toISOString()}</time></p>
        <p>{t('gitAssociations.readTimeScope')}</p>
        <dl className="git-association-facts">
          <dt>{t('gitAssociations.discovery')}</dt><dd>{label(discoveryStates, data.discovery.state)}</dd>
          <dt>{t('gitAssociations.checkpointVersion')}</dt><dd><code>{data.discovery.version}</code></dd>
          <dt>{t('gitAssociations.reconciliation')}</dt><dd>{label(headStates, data.reconciliation.state)}{data.reconciliation.page > 0 && <> · {t('gitAssociations.headPage', { page: data.reconciliation.page })}</>}
            {data.reconciliation.reason && <p>{reason(data.reconciliation.reason)}</p>}
          </dd>
        </dl>
        <p>{data.current_applicability === 'not_evaluated' ? t('gitAssociations.applicability') : t('gitAssociations.unknownApplicability')}</p>
        {!data.items.length && <p className="git-association-empty">{t('gitAssociations.empty')}</p>}
        <ul>{data.items.map(item => <li key={item.id}>
          <strong>{label(states, item.state)}</strong>
          {item.reason && <p>{reason(item.reason)}</p>}
          <dl className="git-association-facts">
            <dt>{t('gitAssociations.rowID')}</dt><dd><code>{item.id}</code></dd>
            <dt>{t('gitAssociations.route')}</dt><dd>
              <span>{item.source_branch} <code>{item.source_branch_id}</code></span> → <span>{item.destination_branch || t('gitAssociations.noDestination')} {item.destination_branch_id && <code>{item.destination_branch_id}</code>}</span>
            </dd>
            <dt>{t('gitAssociations.capture')}</dt><dd><code>{item.capture}</code></dd>
            <dt>{t('gitAssociations.originalHead')}</dt><dd><code>{item.original_head}</code></dd>
            <dt>{t('gitAssociations.publicationID')}</dt><dd><code>{item.publication_id}</code></dd>
            {item.admission_id && <><dt>{t('gitAssociations.admissionID')}</dt><dd><code>{item.admission_id}</code></dd></>}
            {item.contribution_id && <><dt>{t('gitAssociations.contributionID')}</dt><dd><code>{item.contribution_id}</code></dd></>}
            <dt>{t('gitAssociations.attempts')}</dt><dd>{item.attempts}</dd>
            {item.next_attempt && <><dt>{t('gitAssociations.nextAttempt')}</dt><dd><time dateTime={item.next_attempt}>{item.next_attempt}</time></dd></>}
          </dl>
        </li>)}</ul>
        <div className="git-association-controls">
          {cursor && <button disabled={status.isFetching} onClick={() => setCursor('')}>{t('gitAssociations.firstPage')}</button>}
          {data.next_cursor && <button disabled={status.isFetching} onClick={() => setCursor(data.next_cursor!)}>{t('common.next')}</button>}
          <span>{t('common.itemsPerPage', { n: 30 })}</span>
        </div>
      </>}
    </>}
  </details>;
}
