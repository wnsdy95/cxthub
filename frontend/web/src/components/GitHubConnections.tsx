import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import type { GitHubOwner, GitHubEdit } from '../github';
import { navigate } from '../route';
import { useT } from '../i18n';
import { Logo } from './Logo';

function statusKey(status: string) {
 const keys = { connected: 'github.connected', pending: 'github.pending', retrying: 'github.retrying', access_removed: 'github.accessRemoved', disconnected: 'github.disconnected', team_access_required: 'github.teamAccessRequired', not_connected: 'github.notConnected' } as const;
 return keys[status as keyof typeof keys] ?? 'github.pending';
}
export function GitHubConnectionsLink({ namespace }: { namespace?: string }) {
 const t = useT();
 return <button type="button" className="ghost" onClick={() => navigate('/connect/github' + (namespace ? '?namespace=' + encodeURIComponent(namespace) : ''))}>{t('github.title')}</button>;
}
export function GitHubConnectionsPage() {
 const t = useT();
 const query = useQuery({ queryKey: ['github-connections'], queryFn: api.githubOverview,
  refetchInterval: q => q.state.data?.owners.some(o => o.connection?.enabled && (o.connection.status === 'pending' || Date.parse(o.connection.next_sync) <= Date.now() + 30_000)) ? 30_000 : false });
 const identity = useMutation({ mutationFn: () => api.githubStart(), onSuccess: v => location.assign(v.url) });
 const params = new URLSearchParams(location.search), selected = params.get('namespace'), result = params.get('result');
 return <div className="page github-connections"><header className="topbar"><span className="brand sm"><Logo /></span><button className="ghost" onClick={() => navigate('/')}>{t('common.home')}</button></header>
  <main className="github-connections-body"><h1>{t('github.title')}</h1><p className="hint">{t('github.intro')}</p>
   {result && <p role="status" className={result === 'failed' ? 'err' : 'hint'}>{result === 'connected' ? t('github.callbackSuccess') : result === 'approval-required' ? t('github.approvalRequired') : t('github.callbackFailed')}</p>}
   {query.isPending && <p role="status">{t('common.loading')}</p>}
   {query.error && <p role="alert" className="err">{query.error.message}</p>}
   {query.data && !query.data.enabled && <p role="status">{t('github.notConfigured')}</p>}
   {query.data?.enabled && <section className="github-identity"><h2>{t('github.identity')}</h2><p className="hint">{t('github.identityHint')}</p>
    {query.data.identity ? <strong>@{query.data.identity.login}</strong> : <button type="button" disabled={identity.isPending} onClick={() => identity.mutate()}>{t('github.verifyIdentity')}</button>}
    {identity.error && <p role="alert" className="err">{identity.error.message}</p>}
   </section>}
   {query.data?.owners.filter(o => !selected || o.namespace.id === selected).map(owner => <OwnerConnection key={owner.namespace.id} owner={owner} />)}
   {selected && <button className="ghost" onClick={() => navigate('/connect/github')}>{t('github.allOwners')}</button>}
  </main>
 </div>;
}
function OwnerConnection({ owner }: { owner: GitHubOwner }) {
 const t = useT(), qc = useQueryClient(), c = owner.connection;
 const [external, setExternal] = useState(''), [local, setLocal] = useState('');
 const [team, setTeam] = useState(''), [remoteTeam, setRemoteTeam] = useState(''), [sync, setSync] = useState(false);
 const refresh = () => qc.invalidateQueries({ queryKey: ['github-connections'] });
 const edit = useMutation({ mutationFn: (patch: Omit<GitHubEdit, 'generation'>) => api.githubEdit(owner.namespace.id, { ...patch, generation: c?.generation ?? 0 }), onSuccess: refresh, onError: refresh });
 const connect = useMutation({ mutationFn: () => api.githubStart(owner.namespace.id, c?.installation.id ?? 0), onSuccess: v => location.assign(v.url) });
 const busy = edit.isPending || connect.isPending;
 const repo = owner.context_repos.find(r => r.id === local);
 const available = c?.enabled && (c.status === 'connected' || c.status === 'team_access_required');
 return <section className="github-owner" aria-label={owner.namespace.slug}>
  <div className="github-owner-heading"><h2>/{owner.namespace.slug}</h2><span className="ref-badge">{t(statusKey(c?.status ?? 'not_connected'))}</span></div>
  {c && <p className="hint">GitHub: {c.installation.login}{c.checked_at && !c.checked_at.startsWith('0001') && <> · {t('github.checked')} {new Date(c.checked_at).toLocaleString()}</>}</p>}
  <div className="github-actions">
   {(!c?.enabled || c.status === 'access_removed') && <button type="button" disabled={busy} onClick={() => connect.mutate()}>{t('github.connect')}</button>}
   {c?.enabled && <><button type="button" className="ghost" disabled={busy} onClick={() => edit.mutate({ action: 'refresh' })}>{t('github.refresh')}</button><button type="button" className="ghost" disabled={busy} onClick={() => { if (confirm(t('github.disconnectConfirm'))) edit.mutate({ action: 'disconnect' }); }}>{t('github.disconnect')}</button></>}
  </div>
  {(connect.error || edit.error) && <p className="err" role="alert">{(connect.error || edit.error)?.message}</p>}
  {c?.enabled && <>
   <h3>{t('github.repositories')}</h3><p className="hint">{t('github.repositoriesHint')}</p>
   <ul className="github-binding-list">{c.bindings.map(b => <li key={b.context_repo_id}><span>{c.repositories.find(r => r.id === b.external_id)?.full_name ?? `GitHub #${b.external_id}`} → {owner.repositories.find(r => r.id === b.repository_id)?.slug ?? b.repository_id}</span><button type="button" className="ghost mini" disabled={busy} onClick={() => edit.mutate({ action: 'unbind', binding: b })}>{t('github.unlink')}</button></li>)}</ul>
   {available && <div className="github-controls">
    <label>{t('github.remoteRepository')}<select value={external} onChange={e => setExternal(e.target.value)}><option value="">{t('github.choose')}</option>{c.repositories.filter(r => !c.bindings.some(b => b.external_id === r.id)).map(r => <option key={r.id} value={r.id}>{r.full_name}</option>)}</select></label>
    <label>{t('github.localRepository')}<select value={local} onChange={e => setLocal(e.target.value)}><option value="">{t('github.choose')}</option>{owner.context_repos.filter(r => !c.bindings.some(b => b.context_repo_id === r.id)).map(r => <option key={r.id} value={r.id}>{owner.repositories.find(local => local.id === r.repository_id)?.slug} · {r.git_remote_url}</option>)}</select></label>
    <button type="button" disabled={busy || !external || !repo?.repository_id} onClick={() => repo?.repository_id && edit.mutate({ action: 'bind', binding: { repository_id: repo.repository_id, context_repo_id: repo.id, external_id: Number(external) } })}>{t('github.bind')}</button>
   </div>}
   {owner.namespace.kind === 'organization' && <>
    <h3>{t('github.teams')}</h3><p className="hint">{t('github.teamsHint')}</p>
    {c.unresolved_members > 0 && <p className="hint">{t('github.unresolved', { count: c.unresolved_members })}</p>}
    <ul className="github-binding-list">{c.mappings.map(m => <li key={m.team_id}><span>{c.teams.find(team => team.id === m.external_id)?.name ?? `GitHub #${m.external_id}`} → {owner.teams.find(team => team.id === m.team_id)?.name} · {t(m.sync_members ? 'github.syncOn' : 'github.syncOff')}</span><button type="button" className="ghost mini" disabled={busy} onClick={() => edit.mutate({ action: 'unmap-team', mapping: m })}>{t('github.unlink')}</button></li>)}</ul>
    {available && <div className="github-controls">
     <label>{t('github.remoteTeam')}<select value={remoteTeam} onChange={e => setRemoteTeam(e.target.value)}><option value="">{t('github.choose')}</option>{c.teams.map(team => <option key={team.id} value={team.id}>{team.name}</option>)}</select></label>
     <label>{t('github.localTeam')}<select value={team} onChange={e => setTeam(e.target.value)}><option value="">{t('github.choose')}</option>{owner.teams.map(team => <option key={team.id} value={team.id}>{team.name}</option>)}</select></label>
     <label className="github-sync-choice"><input type="checkbox" checked={sync} onChange={e => setSync(e.target.checked)} />{t('github.syncMembers')}</label>
     <button type="button" disabled={busy || !team || !remoteTeam} onClick={() => edit.mutate({ action: 'map-team', mapping: { team_id: team, external_id: Number(remoteTeam), sync_members: sync } })}>{t('github.bind')}</button>
    </div>}
   </>}
  </>}
 </section>;
}
export function EnterpriseGitHubConnections({ id }: { id: string }) {
 const t = useT();
 const q = useQuery({ queryKey: ['enterprise-github', id], queryFn: () => api.githubEnterprise(id), retry: false });
 return <details className="github-enterprise"><summary>{t('github.title')}</summary><p className="hint">{t('github.enterpriseHint')}</p>
  {q.error && <p className="err" role="alert">{q.error.message}</p>}
  {q.data?.map(o => <div className="github-owner-heading" key={o.organization_id}><span>/{o.slug} · {t(statusKey(o.status))}</span>{o.can_manage && <GitHubConnectionsLink namespace={o.namespace_id} />}</div>)}
 </details>;
}
