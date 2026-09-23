import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';

for (const kind of ['organization', 'enterprise'] as const) {
 test(`${kind} invitation needs recipient consent and works from inbox and link`, async ({page,browser},testInfo)=>{
  test.skip(!process.env.CXT_E2E_FULLSTACK,'requires real backend');
  const errors=capturePageErrors(page); const origin='http://127.0.0.1:4174';
  const headers={Origin:origin,'X-Cxt-CSRF':'1'}; const owner=page.context().request;
  const recipientContext=await browser.newContext({baseURL:origin}); const recipient=recipientContext.request;
  const suffix=`${Date.now()}-${testInfo.retry}`;
  try {
   for (const [client,email,name] of [[owner,`invite-owner-${suffix}@example.test`,'Invite Owner'],[recipient,`invite-guest-${suffix}@example.test`,'Invite Guest']] as const) {
    expect((await client.post(`${origin}/api/v1/auth/session`,{headers:{...headers,Authorization:`Bearer dev:${email}:${name}`}})).ok()).toBeTruthy();
    expect((await client.patch(`${origin}/api/v1/me`,{headers,data:{locale:'en'}})).ok()).toBeTruthy();
   }
   const guest=await(await recipient.get('/api/v1/me')).json();
   const created=await owner.post(`/api/v1/${kind}s`,{headers,data:{name:'Invitation Space',slug:`invite-${kind}-${suffix}`}});
   expect(created.ok(),await created.text()).toBeTruthy();const space=await created.json();
   await page.goto(kind==='organization'?`/${space.slug}`:`/enterprises/${space.slug}`);
   if(kind==='organization') await page.getByRole('tab',{name:'People',exact:true}).click();
   else await page.getByRole('button',{name:'Administration members',exact:true}).click();
   await page.getByLabel('Email or @username').fill('@'+guest.username);
   await page.getByRole('button',{name:'Create invitation',exact:true}).click();
   await expect(page.locator('.collaboration-invitations .management-rows')).toContainText(guest.email);
   await expect(page.locator('.collaboration-invitations .management-rows')).toContainText('Pending');
   await expect(page.locator('.invitation-email-status')).toHaveText('Email not requested · share the link');
   const members=await(await owner.get(`/api/v1/${kind}s/${space.id}/members`)).json();
   expect(members.some((m:{user_id:string})=>m.user_id===guest.id)).toBe(false);
   const inbox=await(await recipient.get('/api/v1/me/invitations')).json();expect(inbox).toHaveLength(1);
   const other=await recipientContext.newPage();const guestErrors=capturePageErrors(other);
   await other.goto(`/${guest.username}`);
   await expect(other.getByRole('heading',{name:'Invitation inbox',exact:true})).toBeVisible();
   await other.getByRole('button',{name:'Accept invitation',exact:true}).click();
   await expect(other).toHaveURL(new RegExp(`/invite/${inbox[0].id}$`));
   await expect(other.locator('main')).toContainText('Invitation Space');
   // Merely opening the URL must not accept.
   expect((await(await recipient.get('/api/v1/me/invitations')).json())).toHaveLength(1);
   await other.getByRole('button',{name:'Accept invitation',exact:true}).click();
   await expect(other.getByRole('status')).toContainText('Membership confirmed');
   expect((await(await recipient.get('/api/v1/me/invitations')).json())).toHaveLength(0);
   const after=await(await owner.get(`/api/v1/${kind}s/${space.id}/members`)).json();
   expect(after.find((m:{user_id:string})=>m.user_id===guest.id)?.role).toBe('member');
   await other.getByRole('button',{name:'Open space',exact:true}).click();
   await expect(other.locator('.profile-name')).toHaveText('Invitation Space');
   await page.getByRole('button',{name:'Refresh invitations',exact:true}).click();
   await expect(page.locator('.collaboration-invitations .management-rows')).toContainText('Accepted');
   if (kind === 'organization') await expect(page.locator('.organization-member-list')).toContainText(guest.username);
   // Rendering contract: provider acceptance must not claim inbox delivery.
   let mailState='queued';
   await page.route(`**/api/v1/${kind}s/${space.id}/invitations`,async route=>{
    const response=await route.fetch();const rows=await response.json();
    await route.fulfill({response,json:rows.map((row:object)=>({...row,email_enabled:true,email_status:mailState}))});
   });
   for(const [state,label] of [['queued','Email queued'],['retrying','Email will retry automatically'],['accepted','Resend accepted the email · inbox delivery not confirmed'],['attention','Email needs attention · check server configuration, then renew invitation']]){
    mailState=state;await page.getByRole('button',{name:'Refresh invitations',exact:true}).click();
    await expect(page.locator('.invitation-email-status')).toHaveText(label);
   }
   await page.screenshot({path:testInfo.outputPath(`${kind}-invitations.png`),fullPage:true});
   expect(errors).toEqual([]);expect(guestErrors).toEqual([]);
  } finally {await recipientContext.close();}
 });
}
