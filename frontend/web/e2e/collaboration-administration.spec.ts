import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';

test('base access, namespace rename, repository move and audit use the real server', async ({page,playwright},testInfo)=>{
 test.skip(!process.env.CXT_E2E_FULLSTACK,'requires real backend');
 const errors=capturePageErrors(page);const origin='http://127.0.0.1:4174';
 const headers={Origin:origin,'X-Cxt-CSRF':'1'};const owner=page.context().request;
 const guest=await playwright.request.newContext({baseURL:origin,extraHTTPHeaders:headers});const suffix=`${Date.now()}-${testInfo.retry}`;
 try{
  for(const [api,email] of [[owner,`admin-owner-${suffix}@example.test`],[guest,`admin-guest-${suffix}@example.test`]] as const){expect((await api.post('/api/v1/auth/session',{headers:{...headers,Authorization:`Bearer dev:${email}:Administration`}})).ok()).toBeTruthy();}
  await owner.patch('/api/v1/me',{headers,data:{locale:'en'}});
  const member=await(await guest.get('/api/v1/me')).json();
  const source=await(await owner.post('/api/v1/organizations',{headers,data:{name:'Source Organization',slug:`source-${suffix}`}})).json();
  const target=await(await owner.post('/api/v1/organizations',{headers,data:{name:'Target Organization',slug:`target-${suffix}`}})).json();
  expect((await owner.patch(`/api/v1/organizations/${source.id}/members/${member.id}`,{headers,data:{role:'member'}})).ok()).toBeTruthy();
  const repo=await(await owner.post(`/api/v1/organizations/${source.id}/repositories`,{headers,data:{name:'API'}})).json();
  expect(await(await guest.get('/api/v1/repositories')).json()).toEqual([]);
  await page.goto(`/${source.slug}`);
  await page.getByRole('tab',{name:'Policies',exact:true}).click();
  await page.getByRole('combobox',{name:'Base repository permission',exact:true}).selectOption('puller');
  await page.getByRole('button',{name:'Save',exact:true}).click();
  await expect.poll(async()=>(await(await guest.get('/api/v1/repositories')).json())[0]?.effective_role).toBe('puller');
  await page.getByRole('combobox',{name:'Base repository permission',exact:true}).selectOption('');
  await page.getByRole('button',{name:'Save',exact:true}).click();
  await expect.poll(async()=>(await(await guest.get('/api/v1/repositories')).json()).length).toBe(0);
  await page.getByRole('tab',{name:'Settings',exact:true}).click();
  const next=`renamed-${suffix}`;
  await page.getByLabel('New URL slug',{exact:true}).fill(next);
  await page.getByLabel(`Rename the current space ${source.slug}`,{exact:true}).check();
  await page.getByRole('button',{name:'Rename space URL',exact:true}).click();
  await expect(page).toHaveURL(new RegExp(`/${next}$`));
  const old=await owner.get(`/api/v1/public/repositories/${source.slug}/${repo.slug}`);expect(old.ok(),await old.text()).toBeTruthy();
  await page.goto(`/${next}/${repo.slug}?tab=settings`);
  await page.getByRole('combobox',{name:'Destination owner',exact:true}).selectOption(target.slug);
  await page.getByLabel(`Move ${next}/${repo.slug} to the selected owner`,{exact:true}).check();
  await page.getByRole('button',{name:'Move repository',exact:true}).click();
  await expect(page).toHaveURL(new RegExp(`/${target.slug}/${repo.slug}\\?tab=settings$`));
  const moved=(await(await owner.get(`/api/v1/repositories`)).json()).find((item:{id:string})=>item.id===repo.id);
  expect(moved.id).toBe(repo.id);expect(moved.owner_namespace_id).toBe(target.namespace_id);
  await page.goto(`/${target.slug}`);
  await page.getByRole('tab',{name:'Audit log',exact:true}).click();
  await expect(page.locator('.organization-audit-list')).toContainText('repository.transferred');
  await expect(page.locator('.organization-audit-list')).toContainText('req_');
  const [download]=await Promise.all([page.waitForEvent('download'),page.getByRole('button',{name:'Export loaded records (JSONL)',exact:true}).click()]);
  expect(download.suggestedFilename()).toBe('organization-audit.jsonl');
  const exported=await owner.get(`/api/v1/organizations/${target.id}/audit/export?limit=1`);
  expect(exported.ok(),await exported.text()).toBeTruthy();expect(JSON.parse((await exported.text()).trim()).action).toBe('repository.transferred');
  await page.screenshot({path:testInfo.outputPath('organization-audit.png'),fullPage:true});expect(errors).toEqual([]);
 }finally{await guest.dispose();}
});
