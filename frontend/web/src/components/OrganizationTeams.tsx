import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useOrganizationMembers, useOrganizationRepositories } from '../hooks';
import { useT } from '../i18n';
import { ROLES, type Role } from '../roles';
import type { Team, TeamMembership } from '../types';

export function OrganizationTeams({ organizationId, canCreate }: { organizationId: string; canCreate: boolean }) {
 const t = useT();
 const qc = useQueryClient();
 const teams = useQuery({ queryKey: ['teams', organizationId], queryFn: () => api.listTeams(organizationId) });
 const [selected, setSelected] = useState('');
 const team = teams.data?.find((item) => item.id === selected) ?? teams.data?.[0];
 const teamId = team?.id ?? '';
 const people = useOrganizationMembers(organizationId);
 const repositories = useOrganizationRepositories(organizationId);
 const members = useQuery({ queryKey: ['teamMembers', organizationId, teamId], queryFn: () => api.listTeamMembers(organizationId, teamId), enabled: Boolean(teamId) });
 const grants = useQuery({ queryKey: ['teamRepositories', organizationId, teamId], queryFn: () => api.listTeamRepositories(organizationId, teamId), enabled: Boolean(teamId) });
 const [name, setName] = useState('');
 const [slug, setSlug] = useState('');
 const [description, setDescription] = useState('');
 const [person, setPerson] = useState('');
 const [memberRole, setMemberRole] = useState<TeamMembership['role']>('member');
 const [repositoryId, setRepositoryId] = useState('');
 const [grantRole, setGrantRole] = useState<Role>('member');
 useEffect(() => { setPerson(''); setRepositoryId(''); }, [teamId]);
 const mutation = useMutation({ mutationFn: (run: () => Promise<unknown>) => run(), onSuccess: async () => {
  await Promise.all([
   qc.invalidateQueries({ queryKey: ['teams', organizationId] }),
   qc.invalidateQueries({ queryKey: ['teamMembers', organizationId] }),
   qc.invalidateQueries({ queryKey: ['teamRepositories', organizationId] }),
   qc.invalidateQueries({ queryKey: ['repositories'] }),
   qc.invalidateQueries({ queryKey: ['organization-repositories', organizationId] }),
  ]);
 } });
 const error = teams.error ?? people.error ?? repositories.error ?? members.error ?? grants.error ?? mutation.error;
 const busy = mutation.isPending;
 const personLabel = (id: string) => {
  const user = people.data?.find((item) => item.user_id === id)?.user;
  return user?.nickname || user?.name || user?.username || id;
 };
 return <section className="organization-section team-management">
  <p className="hint">{t('teams.note')}</p>
  {error && <p className="err" role="alert">{error.message}</p>}
  {canCreate && <form className="management-form" onSubmit={(event) => {
   event.preventDefault(); mutation.mutate(async () => { const created = await api.createTeam(organizationId, name, slug, description); setSelected(created.id); setName(''); setSlug(''); setDescription(''); });
  }}>
   <label>{t('teams.name')}<input value={name} onChange={(event) => setName(event.target.value)} required maxLength={100} /></label>
   <label>{t('teams.slug')}<input value={slug} onChange={(event) => setSlug(event.target.value)} placeholder="backend" maxLength={64} /></label>
   <label>{t('teams.description')}<input value={description} onChange={(event) => setDescription(event.target.value)} maxLength={1000} /></label>
   <button disabled={busy || !name.trim()}>{t('teams.create')}</button>
  </form>}
  {teams.isLoading ? <p className="hint">…</p> : teams.data?.length === 0 ? <p className="empty-box">{t('teams.empty')}</p> : <div className="management-grid">
   <nav className="management-list" aria-label={t('teams.title')}>
    {teams.data?.map((item) => <button key={item.id} className={`ghost${teamId === item.id ? ' on' : ''}`} aria-current={teamId === item.id ? 'page' : undefined} onClick={() => setSelected(item.id)}><strong>{item.name}</strong><small>{item.slug}</small></button>)}
   </nav>
   {team && <div className="management-detail">
    <div className="panel-head"><h3>{team.name}</h3>{team.can_delete && <button className="ghost mini danger" disabled={busy} onClick={() => mutation.mutate(() => api.deleteTeam(organizationId, teamId))}>{t('teams.delete')}</button>}</div>
    {team.description && <p>{team.description}</p>}
    {team.can_manage && <TeamProfileEditor key={team.id} organizationId={organizationId} team={team} />}
    <section><h4>{t('teams.members')}</h4>
     {team.can_manage && <form className="management-form" onSubmit={(event) => { event.preventDefault(); mutation.mutate(() => api.setTeamMember(organizationId, teamId, person, memberRole)); }}>
      <select aria-label={t('teams.chooseMember')} value={person} onChange={(event) => setPerson(event.target.value)} required><option value="">{t('teams.chooseMember')}</option>{people.data?.filter((item) => !members.data?.some((member) => member.user_id === item.user_id)).map((item) => <option key={item.user_id} value={item.user_id}>{personLabel(item.user_id)}</option>)}</select>
      <select aria-label={t('dashboard.roleChangeAria')} value={memberRole} onChange={(event) => setMemberRole(event.target.value as TeamMembership['role'])}><option value="member">{t('teams.member')}</option><option value="maintainer">{t('teams.maintainer')}</option></select>
      <button disabled={busy || !person}>{t('teams.addMember')}</button>
     </form>}
     <ul className="management-rows">{members.data?.map((member) => <li key={member.user_id}><span>{personLabel(member.user_id)}{member.source === 'github' && <small className="ref-badge">GitHub</small>}</span>{team.can_manage ? <select aria-label={`${personLabel(member.user_id)} ${t('dashboard.roleChangeAria')}`} value={member.role} disabled={busy} onChange={(event) => { const role = event.target.value as TeamMembership['role']; mutation.mutate(() => api.setTeamMember(organizationId, teamId, member.user_id, role)); }}><option value="member">{t('teams.member')}</option><option value="maintainer">{t('teams.maintainer')}</option></select> : <span>{member.role}</span>}{team.can_manage && member.source !== 'github' && <button className="ghost mini" disabled={busy} onClick={() => mutation.mutate(() => api.removeTeamMember(organizationId, teamId, member.user_id))}>{t('common.remove')}</button>}</li>)}</ul>
    </section>
    <section><h4>{t('teams.repository')}</h4>
     {team.can_manage && <form className="management-form" onSubmit={(event) => { event.preventDefault(); mutation.mutate(() => api.setTeamRepository(organizationId, teamId, repositoryId, grantRole)); }}>
      <select aria-label={t('teams.chooseRepository')} value={repositoryId} onChange={(event) => setRepositoryId(event.target.value)} required><option value="">{t('teams.chooseRepository')}</option>{repositories.data?.filter((item) => item.effective_role === 'owner').map((item) => <option key={item.id} value={item.id}>{item.owner_username}/{item.slug}</option>)}</select>
      <select aria-label={t('dashboard.roleChangeAria')} value={grantRole} onChange={(event) => setGrantRole(event.target.value as Role)}>{ROLES.map((role) => <option key={role} value={role}>{role}</option>)}</select><button disabled={busy || !repositoryId}>{t('teams.grant')}</button>
     </form>}
     <ul className="management-rows">{grants.data?.map((grant) => {
      const repository = repositories.data?.find((item) => item.id === grant.repository_id);
      return <li key={grant.repository_id}><span>{repository ? `${repository.owner_username}/${repository.slug}` : grant.repository_id}</span><span className="role">{grant.role}</span>{team.can_manage && repository?.effective_role === 'owner' && <button className="ghost mini" disabled={busy} onClick={() => mutation.mutate(() => api.removeTeamRepository(organizationId, teamId, grant.repository_id))}>{t('common.remove')}</button>}</li>;
     })}</ul>
    </section>
   </div>}
  </div>}
 </section>;
}

function TeamProfileEditor({ organizationId, team }: { organizationId: string; team: Team }) {
 const t = useT();
 const qc = useQueryClient();
 const [baseline, setBaseline] = useState({ name: team.name, description: team.description ?? '' });
 const [name, setName] = useState(baseline.name);
 const [description, setDescription] = useState(baseline.description);
 const reload = useMutation({ mutationFn: () => qc.fetchQuery({ queryKey: ['teams', organizationId], queryFn: () => api.listTeams(organizationId), staleTime: 0 }), onSuccess: teams => {
  const current = teams.find(item => item.id === team.id);
  if (current) { const next = { name: current.name, description: current.description ?? '' }; setBaseline(next); setName(next.name); setDescription(next.description); save.reset(); }
 } });
 const save = useMutation({
  mutationFn: () => api.updateTeam(organizationId, team.id, baseline, { name, description }),
  onSuccess: async (updated) => {
   const next = { name: updated.name, description: updated.description ?? '' };
   setBaseline(next); setName(next.name); setDescription(next.description);
   await qc.invalidateQueries({ queryKey: ['teams', organizationId] });
  },
 });
 return <details><summary>{t('teams.edit')}</summary>
  <form className="management-form" onSubmit={(event) => { event.preventDefault(); save.mutate(); }}>
   <label>{t('teams.name')}<input value={name} onChange={(event) => setName(event.target.value)} maxLength={100} required /></label>
   <label>{t('teams.description')}<input value={description} onChange={(event) => setDescription(event.target.value)} maxLength={1000} /></label>
   <button disabled={save.isPending || !name.trim() || (name === baseline.name && description === baseline.description)}>{t('common.save')}</button>
  </form>
  {(save.error || reload.error) && <><p role="alert" className="err">{(reload.error ?? save.error)?.message}</p><button type="button" className="ghost" disabled={save.isPending || reload.isPending} onClick={() => reload.mutate()}>{t('teams.reload')}</button></>}

 </details>;
}
