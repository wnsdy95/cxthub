import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';
import { invitePath, navigate } from '../route';
import type { CollaborationInvitation, OrganizationRole } from '../types';

function useInvitationAction() {
 const qc = useQueryClient();
 return useMutation({ mutationFn: ({ id, action }: { id: string; action: 'accept' | 'decline' | 'revoke' | 'resend' }) => api.actOnCollaborationInvitation(id, action),
  onSuccess: async () => { await Promise.all(['invitations', 'invitation', 'organizations', 'enterprises', 'repositories', 'organization-members', 'enterpriseMembers', 'organization-audit', 'enterpriseAudit'].map(key => qc.invalidateQueries({queryKey:[key]}))); },
 });
}

export function InvitationManager({ kind, spaceId, owner }: { kind: 'organization' | 'enterprise'; spaceId: string; owner: boolean }) {
 const t = useT(); const qc = useQueryClient();
 const query = useQuery({queryKey:['invitations',kind,spaceId],queryFn:()=>api.listCollaborationInvitations(kind,spaceId)});
 const [recipient,setRecipient] = useState(''); const [role,setRole] = useState<OrganizationRole>('member');
 const create = useMutation({mutationFn:()=>api.createCollaborationInvitation(kind,spaceId,recipient,role),onSuccess:async()=>{setRecipient('');await qc.invalidateQueries({queryKey:['invitations',kind,spaceId]});}});
 const action = useInvitationAction();
 const error = query.error ?? create.error ?? action.error;
 return <section className="collaboration-invitations"><h3>{t('invitations.title')}</h3>
  <p className="hint">{t('invitations.note')}</p>
  <form className="management-form" onSubmit={event=>{event.preventDefault();create.mutate();}}>
   <label>{t('invitations.recipient')}<input required maxLength={254} value={recipient} onChange={event=>setRecipient(event.target.value)} placeholder="alice@example.com" /></label>
   <label>{t('invitations.role')}<select value={role} onChange={event=>setRole(event.target.value as OrganizationRole)}>{(owner ? ['member','admin','owner'] : ['member']).map(item=><option key={item}>{item}</option>)}</select></label>
   <button disabled={create.isPending || !recipient.trim()}>{t('invitations.create')}</button>
  </form>
  {error && <p className="err" role="alert">{error.message}</p>}
  <button className="ghost mini" onClick={()=>void Promise.all([query.refetch(), qc.invalidateQueries({queryKey: [kind === 'organization' ? 'organization-members' : 'enterpriseMembers', spaceId]})])} disabled={query.isFetching}>{t('invitations.refresh')}</button>
  <ul className="management-rows">{query.data?.map(invite=><li key={invite.id}>
   <InvitationDescription invite={invite} />
   {invite.status === 'pending' && <CopyInvitation invite={invite} />}
   {(invite.status === 'pending' || invite.status === 'expired') && (owner || invite.role === 'member') && <>
    <button className="ghost mini" disabled={action.isPending} onClick={()=>action.mutate({id:invite.id,action:'resend'})}>{t('invitations.resend')}</button>
    <button className="ghost mini" disabled={action.isPending} onClick={()=>action.mutate({id:invite.id,action:'revoke'})}>{t('invitations.revoke')}</button>
   </>}
  </li>)}</ul>
 </section>;
}
function InvitationDescription({invite}:{invite:CollaborationInvitation}) {
 const t=useT();
 return <span><strong>{invite.space_name}</strong> · {invite.email} · {invite.role}<small>{t(`invitations.${invite.status}`)} · {t('invitations.expires',{date:new Date(invite.expires_at).toLocaleString()})}</small>{invite.email_status && <small className="invitation-email-status">{t(`invitations.email_${invite.email_status}`)}{invite.email_reason && invite.email_status === 'attention' && ` · ${invite.email_reason}`}{!invite.email_enabled && ['queued','sending','retrying'].includes(invite.email_status) && ` · ${t('invitations.email_paused')}`}</small>}</span>;
}
function CopyInvitation({invite}:{invite:CollaborationInvitation}) {
 const t=useT(); const [copied,setCopied]=useState(false); const [error,setError]=useState('');
 return <span><button className="ghost mini" onClick={async()=>{try {await navigator.clipboard.writeText(new URL(invitePath(invite.id),location.origin).href);setCopied(true);setError('');} catch(e) {setError(String(e));}}}>{t(copied?'invitations.copied':'invitations.copy')}</button>{error && <small role="alert">{error}</small>}</span>;
}
export function InvitationInbox() {
 const t=useT();
 const query=useQuery({queryKey:['invitations','inbox'],queryFn:api.invitationInbox,refetchOnWindowFocus:true});
 return <section className="profile-section-block"><div className="profile-section-head"><h2 className="profile-section">{t('invitations.inbox')}</h2><button className="ghost mini" disabled={query.isFetching} onClick={()=>void query.refetch()}>{t('invitations.refresh')}</button></div>
  {query.error && <p className="err" role="alert">{query.error.message}</p>}
  {query.data?.length === 0 && <p className="hint">{t('invitations.empty')}</p>}
  <ul className="management-rows">{query.data?.map(invite=><li key={invite.id}><InvitationDescription invite={invite}/><button onClick={()=>navigate(invitePath(invite.id))}>{t('invitations.accept')}</button></li>)}</ul>
 </section>;
}
export function InvitationPage({id}:{id:string}) {
 const t=useT(); const query=useQuery({queryKey:['invitation',id],queryFn:()=>api.getCollaborationInvitation(id)});
 const action=useInvitationAction(); const invite=query.data; const error=query.error??action.error;
 return <main className="invitation-page"><h1>{t('invitations.title')}</h1><p>{t('invitations.account')}</p>
  {error && <p className="err" role="alert">{error.message}</p>}
  {query.isLoading && <p>…</p>}
  {invite && <><InvitationDescription invite={invite}/>
   {invite.status==='pending' && <div className="management-form"><button className="primary" disabled={action.isPending} onClick={()=>action.mutate({id,action:'accept'})}>{t('invitations.accept')}</button><button disabled={action.isPending} onClick={()=>action.mutate({id,action:'decline'})}>{t('invitations.decline')}</button></div>}
   {invite.status==='accepted' && <p role="status">{t('invitations.joined')} <button onClick={()=>navigate(invite.space_path)}>{t('invitations.open')}</button></p>}
  </>}
 </main>;
}
