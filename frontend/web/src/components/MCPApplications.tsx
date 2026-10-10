import {useMutation,useQuery,useQueryClient} from '@tanstack/react-query';
import {api} from '../api';
import {useT} from '../i18n';
export function MCPApplications(){
 const t=useT();const qc=useQueryClient();const apps=useQuery({queryKey:['mcpApplications'],queryFn:api.mcpApplications});
 const audit=useQuery({queryKey:['accountAudit'],queryFn:api.accountAudit});
 const revoke=useMutation({mutationFn:api.revokeMCPApplication,onSuccess:async()=>{await Promise.all([qc.invalidateQueries({queryKey:['mcpApplications']}),qc.invalidateQueries({queryKey:['accountAudit']})]);}});
 const error=apps.error??audit.error??revoke.error;
 return <section className="account-card" aria-labelledby="account-mcp-title"><div className="account-card-toolbar"><div><h3 id="account-mcp-title">{t('mcpApplications.title')}</h3><p className="hint">{t('mcpApplications.hint')}</p></div>
  <button type="button" className="ghost mini" disabled={apps.isFetching} onClick={()=>void apps.refetch()}>{t('mcpApplications.refresh')}</button></div>
  {error&&<p className="err" role="alert">{error.message}</p>}
  {apps.data?.length===0&&<p className="account-empty-state">{t('mcpApplications.empty')}</p>}
  <ul className="management-rows">{apps.data?.map(app=><li key={app.client_id}><span><strong>{app.name}</strong><small>{app.scope} · {app.client_id}</small></span><button type="button" className="ghost mini" disabled={revoke.isPending} onClick={()=>revoke.mutate(app.client_id)}>{t('mcpApplications.revoke')}</button></li>)}</ul>
  <details><summary>{t('mcpApplications.audit')}</summary><ul className="management-rows">{audit.data?.map(event=><li key={event.id}><span>{event.action}<small>{event.client_id}</small></span><time>{new Date(event.created_at).toLocaleString()}</time></li>)}</ul></details>
 </section>;
}
