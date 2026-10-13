import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';
import type { Repository, User } from '../src/types';

test('accepted repository invitations appear on the member profile and disappear after access is removed', async ({ page, playwright }, testInfo) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires isolated PostgreSQL and real API');
  const errors = capturePageErrors(page);
  const origin = 'http://127.0.0.1:4174';
  const headers = { Origin: origin, 'X-Cxt-CSRF': '1' };
  const suffix = `${Date.now()}-${testInfo.retry}`;
  const member = page.context().request;
  const owner = await playwright.request.newContext({ baseURL: origin, extraHTTPHeaders: headers });
  const anonymous = await playwright.request.newContext({ baseURL: origin });
  try {
    for (const [client, email] of [[member, `profile-member-${suffix}@example.test`], [owner, `profile-owner-${suffix}@example.test`]] as const) {
      const response = await client.post('/api/v1/auth/session', { headers: { ...headers, Authorization: `Bearer dev:${email}:Profile` } });
      expect(response.ok(), await response.text()).toBe(true);
    }
    expect((await member.patch('/api/v1/me', { headers, data: { locale: 'en' } })).ok()).toBe(true);
    const memberUser: User = await (await member.get('/api/v1/me')).json();
    const repositoryResponse = await owner.post('/api/v1/repositories', { data: { name: 'project' } });
    expect(repositoryResponse.ok(), await repositoryResponse.text()).toBe(true);
    const repository: Repository = await repositoryResponse.json();
    await page.goto(`/${memberUser.username}`);
    await expect(page.getByText('No repositories yet.', { exact: true })).toBeVisible();

    const invitationResponse = await owner.post(`/api/v1/repositories/${repository.id}/invites`, { data: { email: memberUser.email, role: 'member', expires_in_days: 1 } });
    expect(invitationResponse.ok(), await invitationResponse.text()).toBe(true);
    const invitation = await invitationResponse.json();
    await page.goto(`/invite/${encodeURIComponent(invitation.token)}`);
    await expect(page).toHaveURL(`${origin}/${repository.owner_username}/${repository.slug}`);
    await page.getByRole('button', { name: 'Profile menu', exact: true }).click();
    await page.getByRole('link', { name: 'My profile', exact: true }).click();
    await expect(page.locator('.repository-card-name')).toHaveText(`${repository.owner_username}/project`);
    await expect(page.locator('.profile-section-head .count-badge')).toHaveText('1');

    const ownResponse = await member.post('/api/v1/repositories', { headers, data: { name: 'project' } });
    expect(ownResponse.ok(), await ownResponse.text()).toBe(true);
    await page.reload();
    await expect(page.locator('.repository-card-name')).toHaveText([`${repository.owner_username}/project`, `${memberUser.username}/project`]);
    await expect(page.locator('.profile-section-head .count-badge')).toHaveText('2');

    for (const visitor of [owner, anonymous]) {
      const response = await visitor.get(`/api/v1/public/users/${memberUser.username}`);
      expect(response.ok(), await response.text()).toBe(true);
      expect((await response.json()).repositories).toEqual([]);
    }
    const removed = await owner.delete(`/api/v1/repositories/${repository.id}/members/${encodeURIComponent(memberUser.id)}`);
    expect(removed.ok(), await removed.text()).toBe(true);
    await page.reload();
    await expect(page.locator('.repository-card-name')).toHaveText(`${memberUser.username}/project`);
    await expect(page.locator('.profile-section-head .count-badge')).toHaveText('1');
    const denied = await member.get(`/api/v1/public/repositories/${repository.owner_username}/${repository.slug}`);
    expect(denied.status()).toBe(404);
    await page.screenshot({ path: testInfo.outputPath('profile-after-revocation.png'), fullPage: true });
    expect(errors).toEqual([]);
  } finally {
    await owner.dispose();
    await anonymous.dispose();
  }
});
