import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useMe, useOrganizations } from '../hooks';
import { useT } from '../i18n';
import { navigate, repositoryPath } from '../route';
import type { Repository } from '../types';

export function RenameSpace({kind,id,slug}:{kind:'organization'|'enterprise';id:string;slug:string}) {
 const t=useT();const qc=useQueryClient();const [next,setNext]=useState(slug);const [confirmed,setConfirmed]=useState(false);
 const mutation=useMutation({mutationFn:()=>api.renameSpace(kind,id,slug,next),onSuccess:async result=>{await qc.invalidateQueries();navigate(result.path);}});
 return <section className="danger-zone"><h3>{t('namespace.rename')}</h3><p className="hint">{t('namespace.renameHint')}</p>
  <form className="management-form" onSubmit={event=>{event.preventDefault();mutation.mutate();}}>
   <label>{t('namespace.slug')}<input value={next} onChange={event=>{setNext(event.target.value);setConfirmed(false);}} pattern="[a-z0-9][a-z0-9-]*" maxLength={64} required /></label>
   <label><input type="checkbox" checked={confirmed} onChange={event=>setConfirmed(event.target.checked)} />{t('namespace.confirmRename',{slug})}</label>
   <button disabled={!confirmed||!next.trim()||next===slug||mutation.isPending}>{t('namespace.rename')}</button>
  </form>{mutation.error&&<p className="err" role="alert">{mutation.error.message}</p>}
 </section>;
}
export function TransferNamespace({repository}:{repository:Repository}) {
 const t=useT();const qc=useQueryClient();const me=useMe().data;const organizations=useOrganizations();
 const [destination,setDestination]=useState('');const [confirmed,setConfirmed]=useState(false);
 const mutation=useMutation({mutationFn:()=>api.transferRepositoryNamespace(repository,destination),onSuccess:async result=>{await qc.invalidateQueries();navigate(repositoryPath(result,'settings'));}});
 const source=organizations.data?.find(org=>org.namespace_id===repository.owner_namespace_id);
 const canTransfer=source?source.effective_role==='owner':me?.username===repository.owner_username;
 if(!canTransfer)return null;
 const choices=[...(me&&me.username!==repository.owner_username?[{slug:me.username,name:me.username}]:[]),...(organizations.data??[]).filter(org=>org.effective_role==='owner'&&org.namespace_id!==repository.owner_namespace_id)];
 return <section className="danger-zone"><h3>{t('namespace.transfer')}</h3><p className="hint">{t('namespace.transferHint')}</p>
  <div className="management-form">
   <label>{t('namespace.destination')}<select value={destination} onChange={event=>{setDestination(event.target.value);setConfirmed(false);}}><option value="">{t('namespace.choose')}</option>{choices.map(item=><option key={item.slug} value={item.slug}>{item.name} / {item.slug}</option>)}</select></label>
   <label><input type="checkbox" checked={confirmed} onChange={event=>setConfirmed(event.target.checked)}/>{t('namespace.confirmTransfer',{path:`${repository.owner_username}/${repository.slug}`})}</label>
   <button type="button" onClick={()=>mutation.mutate()} disabled={!destination||!confirmed||mutation.isPending}>{t('namespace.transfer')}</button>
  </div>{(mutation.error??organizations.error)&&<p className="err" role="alert">{(mutation.error??organizations.error)?.message}</p>}
 </section>;
}
