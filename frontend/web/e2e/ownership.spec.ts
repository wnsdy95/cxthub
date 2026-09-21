import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';

test('real server enforces organization teams and enterprise policy through the browser', async ({ page, playwright }, testInfo) => {
 test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires the real development backend');
 const errors = capturePageErrors(page);
 const origin = 'http://127.0.0.1:4174';
 const headers = { Origin: origin, 'X-Cxt-CSRF': '1' };
 const owner = page.context().request;
 const member = await playwright.request.newContext({ baseURL: origin, extraHTTPHeaders: headers });
 const suffix = `${Date.now()}-${testInfo.retry}`;
 try {
  const login = await owner.post('/api/v1/auth/session', {headers:{...headers, Authorization:`Bearer dev:ownership-${suffix}@example.test:Ownership Owner`}});
  expect(login.ok(), await login.text()).toBeTruthy();
  const me = await (await owner.get('/api/v1/me')).json();
  expect((await owner.patch('/api/v1/me', {headers,data:{locale:'en'}})).ok()).toBeTruthy();
  expect((await member.post('/api/v1/auth/session', {headers:{Authorization:`Bearer dev:teammate-${suffix}@example.test:Team Member`}})).ok()).toBeTruthy();
  const teammate = await (await member.get('/api/v1/me')).json();
  const created = await owner.post('/api/v1/organizations', {headers,data:{name:'Acme Engineering',slug:`acme-${suffix}`}});
  expect(created.ok(), await created.text()).toBeTruthy();
  const org = await created.json();
  const add = await owner.patch(`/api/v1/organizations/${org.id}/members/${encodeURIComponent('@'+teammate.username)}`,{headers,data:{role:'member'}});
  expect(add.ok(), await add.text()).toBeTruthy();
  const createdRepo = await owner.post(`/api/v1/organizations/${org.id}/repositories`,{headers,data:{name:'Backend'}});
  expect(createdRepo.ok(),await createdRepo.text()).toBeTruthy();
  const repo = await createdRepo.json();
  expect(repo.effective_role).toBe('owner');
  expect(await (await member.get('/api/v1/repositories')).json()).toEqual([]);
  await page.goto(`/${org.slug}`);
  await expect(page.locator('.profile-name')).toHaveText('Acme Engineering');
  await page.getByRole('tab',{name:'Teams',exact:true}).click();
  await page.getByLabel('Team name',{exact:true}).fill('Backend');
  await page.getByLabel('Team slug',{exact:true}).fill('backend');
  await page.getByRole('button',{name:'Create team',exact:true}).click();
  await expect(page.locator('.management-detail h3')).toHaveText('Backend');
  await page.getByRole('combobox',{name:'Select an organization member'}).selectOption(teammate.id);
  await page.getByRole('button',{name:'Add member',exact:true}).click();
  await expect(page.locator('.management-rows').first()).toContainText('Team Member');
  await page.getByRole('combobox',{name:'Select a repository you administer'}).selectOption(repo.id);
  await page.getByRole('button',{name:'Grant access',exact:true}).click();
  await expect.poll(async()=> (await (await member.get('/api/v1/repositories')).json())[0]?.effective_role).toBe('member');
  expect((await member.get(`/api/v1/public/repositories/${org.slug}/${repo.slug}`)).ok()).toBeTruthy();
  await page.screenshot({path:testInfo.outputPath('organization-teams.png'),fullPage:true});
  await page.locator('.management-rows').first().getByRole('button',{name:'Remove',exact:true}).click();
  await expect.poll(async()=> (await (await member.get('/api/v1/repositories')).json()).length).toBe(0);
  expect((await member.get(`/api/v1/public/repositories/${org.slug}/${repo.slug}`)).status()).toBe(404);

  // Direct grants require an explicit removal decision; team removal alone
  // must not revoke an independently granted collaborator role.
  const invitation = await owner.post(`/api/v1/repositories/${repo.id}/invites`, {headers,data:{email:teammate.email,role:'puller',expires_in_days:1}});
  expect(invitation.ok(),await invitation.text()).toBeTruthy();
  const invite = await invitation.json();
  const accepted = await member.post(`/api/v1/invites/${invite.token}/accept`,{data:{}});
  expect(accepted.ok(),await accepted.text()).toBeTruthy();
  await page.getByRole('tab',{name:'People',exact:true}).click();
  await page.locator('.organization-member-row').filter({hasText:'Team Member'}).getByRole('button',{name:'Remove',exact:true}).click();
  const removal = page.locator('form').filter({has:page.getByRole('combobox',{name:'Direct repository access',exact:true})});
  await expect(removal.getByRole('button',{name:'Remove',exact:true})).toBeDisabled();
  await page.getByRole('combobox',{name:'Direct repository access',exact:true}).selectOption('revoke');
  await removal.getByRole('button',{name:'Remove',exact:true}).click();
  await expect(page.locator('.organization-member-row').filter({hasText:'Team Member'})).toHaveCount(0);
  await expect.poll(async()=> (await (await member.get('/api/v1/repositories')).json()).length).toBe(0);

  await page.goto(`/${me.username}`);
  const createForm = page.locator('details.organization-create').filter({has:page.locator('summary',{hasText:'Create enterprise'})});
  await createForm.locator('summary').click();
  await createForm.getByLabel('Enterprise name',{exact:true}).fill('Acme Group');
  await createForm.getByLabel('URL slug',{exact:true}).fill(`group-${suffix}`);
  await createForm.getByRole('button',{name:'Create enterprise',exact:true}).click();
  await expect(page.locator('.profile-name')).toHaveText('Acme Group');
  await expect(page).toHaveURL(new RegExp(`/enterprises/group-${suffix}$`));
  await page.getByRole('combobox',{name:'Select an organization you own'}).selectOption(org.id);
  await page.getByRole('button',{name:'Link organization',exact:true}).click();
  await expect(page.locator('.management-rows')).toContainText('Acme Engineering');
  await page.getByRole('button',{name:'Shared policies',exact:true}).click();
  await page.getByLabel('Allow public repositories',{exact:true}).uncheck();
  await page.getByRole('button',{name:'Save',exact:true}).click();
  await expect.poll(async()=> (await (await owner.get(`/api/v1/organizations/${org.id}/effective-policy`)).json()).allow_public_repositories).toBe(false);
  expect((await owner.patch(`/api/v1/repositories/${repo.id}`,{headers,data:{visibility:'public'}})).status()).toBe(403);
  await page.screenshot({path:testInfo.outputPath('enterprise-policies.png'),fullPage:true});
  expect(errors).toEqual([]);
 } finally { await member.dispose(); }
});
