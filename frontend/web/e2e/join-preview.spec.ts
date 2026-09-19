import { test, expect } from '@playwright/test';
import { createHash } from 'node:crypto';

function canonical(value: unknown): string {
  if (Array.isArray(value)) return '[' + value.map(canonical).join(',') + ']';
  if (value && typeof value === 'object') return '{' + Object.entries(value).sort(([a],[b]) => a.localeCompare(b)).map(([k,v]) => JSON.stringify(k)+':'+canonical(v)).join(',') + '}';
  return JSON.stringify(value);
}
const hash = (s:string) => 'sha256:'+createHash('sha256').update(s).digest('hex');

test('real server join preview survives query failure and requires renewed approval after a concurrent ref', async ({page}, info) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires actual query and command handlers');
  const api=page.context().request;
  const headers={Origin:'http://127.0.0.1:4174','X-Cxt-CSRF':'1'};
  expect((await api.post('/api/v1/auth/session',{headers:{...headers,Authorization:'Bearer dev:join-e2e@example.test:Join E2E'}})).ok()).toBe(true);
  const me=await (await api.get('/api/v1/me')).json();
  const wsResp=await api.post('/api/v1/workspaces',{headers,data:{name:`JoinPreview${info.retry}`}});
  expect(wsResp.ok(),await wsResp.text()).toBe(true);const ws=await wsResp.json();
  const remote=`http://cxthub.test/${me.username}/${ws.slug}`;
  const repo=hash(remote.replace('http://','').toLowerCase());
  const registered=await api.post('/api/v1/repos',{headers,data:{id:repo,remote_url:remote,default_branch:'main'}});
  expect(registered.ok(),await registered.text()).toBe(true);
  const base=`/api/v1/repos/${encodeURIComponent(repo)}`;
  const docs=['Parent','Head','Source','Tip'].map(name=>{
    const cir={envelope:{cir_version:'2',source_provider:'codex',source_model:'test',captured_at:'2026-09-20T00:00:00Z',cwd:'',git_branch:'main',session_origin_id:name,fidelity:'full'},events:[]};
    return {hash:hash(canonical(cir)),cir};
  });
  const [parent,head,source,tip]=docs.map(d=>d.hash);
  const snapshots=docs.map((d,i)=>({id:d.hash,doc_hash:d.hash,repo_id:repo,branch:'main',provider:'codex',fidelity:'full',message:d.cir.envelope.session_origin_id,
    parents:i===0?[]:i===3?[source]:[parent],created_at:`2026-09-20T00:00:0${i}Z`}));
  const objects=await api.post(base+'/push/objects',{headers,data:{docs,snapshots}});
  expect(objects.ok(),await objects.text()).toBe(true);
  expect((await api.post(base+`/snapshots/${encodeURIComponent(head)}/graft`,{headers,data:{parents:[tip],expected_seq:0}})).ok()).toBe(true);
  expect((await api.put(base+'/refs/branch/main',{headers,data:{target:head,expected_target:''}})).ok()).toBe(true);
  await page.goto(`/${me.username}/${ws.slug}`);
  const row=page.locator(`.graph-row[data-graph-snapshot="${source}"]`).first();
  await expect(row).toBeVisible();
  const route='**/join/preview?**';
  await page.route(route,r=>r.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:{code:'unavailable',message:'unavailable'}})}));
  await row.click({button:'right'});
  const modal=page.getByRole('dialog',{name:'Join session branch'});
  await expect(modal.getByText('The join plan could not be loaded. Try again.')).toBeVisible();
  await expect(modal.getByRole('button',{name:/Join all/})).toHaveCount(0);
  await page.unroute(route);
  await modal.getByRole('button',{name:'Refresh plan'}).click();
  await expect(modal.getByText('Current head')).toBeVisible();
  await modal.screenshot({path:info.outputPath('join-preview.png')});
  await modal.getByRole('button',{name:'Cancel'}).click();
  let release!: () => void;
  let delayedDone!: () => void;
  const held = new Promise<void>(resolve => { release=resolve; });
  const done = new Promise<void>(resolve => { delayedDone=resolve; });
  let first=true;
  await page.route(route,async r=>{
    if (new URL(r.request().url()).searchParams.get('snapshot')!==source || !first) {await r.continue();return;}
    first=false;
    const response=await r.fetch();
    await held;
    try {await r.fulfill({response});} finally {delayedDone();}
  });
  await row.click({button:'right'});
  await expect(modal.getByRole('status')).toContainText('Checking the current join plan');
  await modal.getByRole('button',{name:'Cancel'}).click();
  await page.locator(`.graph-row[data-graph-snapshot="${tip}"]`).first().click({button:'right'});
  await expect(modal.locator('code').first()).toHaveText(tip.replace('sha256:','').slice(0,10));
  release();await done;
  await expect(modal.locator('code').first()).toHaveText(tip.replace('sha256:','').slice(0,10));
  await modal.getByRole('button',{name:'Cancel'}).click();
  await page.unroute(route);
  await row.click({button:'right'});
  await expect(modal.getByRole('button',{name:/Join all/})).toBeVisible();
  // A real concurrent writer changes only a ref, not the selected head or source.
  expect((await api.put(base+'/refs/branch/parallel',{headers,data:{target:parent,expected_target:''}})).ok()).toBe(true);
  await modal.getByRole('button',{name:/Join all/}).click();
  await expect(modal.getByRole('alert')).toContainText('Review the current plan');
  const before=await (await api.get(base+'/refs')).json();
  expect(before.find((r:{name:string})=>r.name==='main').target).toBe(head);
  await modal.getByRole('button',{name:'Refresh plan'}).click();
  await expect(modal.getByRole('alert')).toHaveCount(0);
  await modal.getByRole('button',{name:/Join all/}).click();
  await expect(modal).toHaveCount(0);
  const after=await (await api.get(base+'/refs')).json();
  expect(after.find((r:{name:string})=>r.name==='main').target).toBe(tip);
  const preserved=await (await api.get(base+'/snapshots/'+encodeURIComponent(source))).json();
  expect(preserved.parents).toEqual([parent]);expect(preserved.graft_parents).toContain(head);
});
