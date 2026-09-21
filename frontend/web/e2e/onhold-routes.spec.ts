import { expect, test } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';

for (const selected of ['legacy', 'named']) {
  test(`On Hold preserves ${selected} repository across tabs, badges and browser history`, async ({ page }) => {
    const errors = capturePageErrors(page);
    const head = `sha256:${'1'.repeat(64)}`;
    const held = `sha256:${'2'.repeat(64)}`;
    const base = '/alice/cxthub';
    const contextPath = selected === 'legacy' ? base : '/alice/onhold';
    const holdPath = `${contextPath}?tab=onhold`;
    const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
      if (method !== 'GET') return undefined;
      if (pathname === '/api/v1/me') return { body: { id: 'member', username: 'alice', locale: 'en' } };
      if (pathname === '/api/v1/repositories') return { body: ['legacy','named'].map(id => ({id, name: id === 'legacy' ? 'cxthub' : 'onhold', slug: id === 'legacy' ? 'cxthub' : 'onhold', owner_username:'alice',visibility:'private',effective_role:'member'})) };
      if (decodeURIComponent(pathname) === '/api/v1/public/repositories/alice/cxthub/onhold') return {body:{id:'named',name:'onhold',slug:'onhold',owner_username:'alice',visibility:'private',effective_role:'member'}};
      if (/^\/api\/v1\/repositories\/(legacy|named)\/members$/.test(pathname)) return { body: [{ repository_id: 'repositoryMetadata', user_id: 'member', role: 'member' }] };
      if (pathname === '/api/v1/repos') return {body:[{id:searchParams.get('repository'),default_branch:'main',remote_url:`https://cxthub.com${searchParams.get('repository') === 'legacy' ? base : `${base}/onhold`}`}]};
      const match = pathname.match(/^\/api\/v1\/repos\/(legacy|named)\/(.+)$/);
      if (!match) return undefined;
      const [, repo, resource] = match;
      if (resource === 'refs') return { body: [{ kind: 'branch', name: 'main', repo_id: repo, target: head }] };
      if (resource === 'snapshots') return { body: [head, held].map((hash, index) => ({
        id: hash, doc_hash: hash, repo_id: repo, parents: index ? [head] : [], branch: 'main',
        message: `${repo} ${index ? 'held' : 'shared'} snapshot`, provider: 'codex',
        created_at: `2026-09-16T0${index}:00:00Z`, author: { name: 'Alice', email: 'alice@example.test' },
      })) };
      if (resource === 'unsync') return { body: [{ repo_id: repo, branch: 'main', user: 'alice', target: held, updated_at: '2026-09-16T01:00:00Z' }] };
      if (['pending', 'history', 'reflog'].includes(resource)) return { body: [] };
      if (resource.startsWith('settings/') || resource === 'secrets') return { body: null };
      if (resource.startsWith('docs/') && resource.endsWith('/events')) return { body: {
        hash: resource.split('/')[1], envelope: { cir_version: '2', source_provider: 'codex' },
        events: Array.from({length:60},(_,seq)=>({ kind: 'message', role: 'user', seq, blocks: [{ type: 'text', text: `${repo} conversation ${seq}` }] })),
        total: 60, offset: 0, next: -1, inherited: 0,
      } };
      return undefined;
    });

    await page.goto(selected === 'named' ? `${base}/onhold` : contextPath);
    await expect(page).toHaveURL(new URL(contextPath, 'http://127.0.0.1:4174').href);
    await expect(page.locator('.commit-row').first()).toContainText(`${selected} shared snapshot`);
    await page.getByRole('button', { name: 'On Hold', exact: true }).click();
    await expect(page).toHaveURL(new URL(holdPath, 'http://127.0.0.1:4174').href);
    await expect(page.locator('.tab.on')).toHaveText('On Hold');
    await expect(page.locator('.commit-row').first()).toContainText(`${selected} held snapshot`);
    const graphTarget = page.locator(`[data-graph-snapshot="${held}"]`);
    await graphTarget.click();
    await expect(page.locator('.viewer-head code')).toHaveText('2222222222');
    await expect(page.getByText(`${selected} conversation 59`, {exact:true})).toBeAttached();
    await expect.poll(()=>page.locator('.viewer').evaluate(el=>Math.abs(el.getBoundingClientRect().top-el.closest('.ctx-main')!.getBoundingClientRect().top))).toBeLessThan(3);
    await page.locator('.ctx-main').evaluate(el=>{el.scrollTop=el.scrollHeight;});
    await expect.poll(()=>page.locator('.ctx-main').evaluate(el=>el.scrollTop)).toBeGreaterThan(300);
    await graphTarget.click();
    await expect.poll(()=>page.locator('.viewer').evaluate(el=>Math.abs(el.getBoundingClientRect().top-el.closest('.ctx-main')!.getBoundingClientRect().top))).toBeLessThan(3);
    await page.reload();
    await expect(page.locator('.commit-row').first()).toContainText(`${selected} held snapshot`);
    await page.goBack();
    await expect(page.locator('.commit-row').first()).toContainText(`${selected} shared snapshot`);
    await page.goForward();
    await expect(page.locator('.tab.on')).toHaveText('On Hold');
    await page.getByRole('button', { name: 'Context', exact: true }).click();
    await expect(page).toHaveURL(new URL(contextPath, 'http://127.0.0.1:4174').href);
    await page.locator('.commit-row .pending.link').click();
    await expect(page).toHaveURL(new URL(holdPath, 'http://127.0.0.1:4174').href);
    await expect(page.locator('.commit-row').first()).toContainText(`${selected} held snapshot`);

    if (selected === 'legacy') {
      await page.goto(`${base}/-/onhold`);
      await expect(page.getByRole('heading', { name: '404' })).toBeVisible();
      await expect(page.locator('.tabs')).toHaveCount(0);
    }
    expect(errors).toEqual([]);
    expect(unexpected).toEqual([]);
  });
}

for (const signedIn of [true, false]) {
  test(`retired separator URLs do not load repository data (${signedIn ? 'signed in' : 'anonymous'})`, async ({ page }) => {
    const errors = capturePageErrors(page);
    const unexpected = await installApiFixture(page, ({ method, pathname }) => {
      if (method === 'GET' && pathname === '/api/v1/me') {
        return signedIn
          ? { body: { id: 'owner', username: 'alice', locale: 'en' } }
          : { status: 401, body: { error: { message: 'anonymous fixture' } } };
      }
      return undefined;
    });
    for (const tab of ['settings', 'members', 'connections', 'onhold']) {
      const path = `/alice/cxthub/-/${tab}?tab=${tab}`;
      await page.goto(path);
      await expect(page.getByRole('heading', { name: '404' })).toBeVisible();
      await expect(page).toHaveURL(new URL(path, 'http://127.0.0.1:4174').href);
      await expect(page.locator('.tabs')).toHaveCount(0);
    }
    await page.reload();
    await expect(page.getByRole('heading', { name: '404' })).toBeVisible();
    expect(errors).toEqual([]);
    expect(unexpected).toEqual([]);
  });
}
