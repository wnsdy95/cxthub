import { expect, test } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';

test('live sessions precede stored captures, follow replacement, expire offline and respect dismissal/commit', async ({page}) => {
  const now = new Date('2026-09-18T12:00:00Z');
  await page.clock.install({time: now});
  const id = (n: number) => `sha256:${String(n).repeat(64)}`;
  let generation = 2;
  let offline = false;
  let fullReads = 0, pendingReads = 0;
  page.on('request', req => { const path = new URL(req.url()).pathname; if (path.endsWith('/view')) fullReads++; if (path.endsWith('/pending-view')) pendingReads++; });
  const errors = capturePageErrors(page);
  const unexpected = await installApiFixture(page, ({method, pathname}) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') return {body:{id:'member',username:'alice',locale:'en'}};
    if (pathname === '/api/v1/repositories') return {body:[{id:'repositoryMetadata', name:'cxthub', slug:'cxthub',owner_username:'alice',visibility:'private'}]};
    if (pathname === '/api/v1/repositories/repositoryMetadata/members') return {body:[{repository_id:'repositoryMetadata',user_id:'member',role:'member'}]};
    if (pathname === '/api/v1/repos') return {body:[{id:'r',default_branch:'main',remote_url:'https://cxthub.com/alice/cxthub'}]};
    const resource = pathname.replace('/api/v1/repos/r/', '');
    if (resource === 'refs') return offline ? {status:503,body:{error:{message:'offline fixture'}}} : {body:[{kind:'branch',name:'main',repo_id:'r',target:id(1)}]};
    if (resource === 'snapshots') return {body:[1,generation,4,5,6,...(generation===7?[8]:[])].map(n => ({
      id:id(n),doc_hash:id(n),repo_id:'r',parents:n===1?[]:[id(n===7?8:1)],branch:'main',provider:n===4?'claude':'codex',
      session_id:`s${n===generation?2:n}`,message:n===1?'committed':'hook: synthetic live session',
      created_at:n===5?'2026-09-01T12:00:00Z':now.toISOString(), author:{name:'Alice',email:'alice@example.test'},
    }))};
    if (resource === 'pending') return {body:[5,2,4,6,1].map(n=>({repo_id:'r',session_id:`s${n}`,target:id(n===2?generation:n),
      branch:n===4?'feature/claude':'main',provider:n===4?'claude':'codex',updated_at:now.toISOString(),
      ...(n===5?{}:{activity_at:now.toISOString()}),dismissed:n===6,author:{name:'Alice',email:'alice@example.test'},
    }))};
    if (['unsync','history','reflog'].includes(resource)) return {body:[]};
    if (resource.startsWith('settings/') || resource === 'secrets') return {body:null};
    if (resource.startsWith('docs/') && resource.endsWith('/events')) return {body:{hash:resource.split('/')[1],envelope:{cir_version:'2',source_provider:'codex'},
      events:[{kind:'message',role:'user',seq:0,blocks:[{type:'text',text:`Capture generation ${generation}`}]}],total:1,offset:0,next:-1,inherited:0}};
    return undefined;
  }, {graphRevision: () => '1'}); // object staging does not publish a graph change
  await page.goto('/alice/cxthub?tab=onhold');
  const live = page.locator('.pending-sessions[data-live="true"]');
  await expect(live.locator('.commit-row')).toHaveCount(2);
  await expect(page.locator('.pending-sessions[data-live="false"] .commit-row')).toHaveCount(1);
  await expect(page.locator('.pending-sessions').first()).toHaveAttribute('data-live','true');
  await expect(page.locator('.pending-sessions[data-live="false"] em')).toContainText('2026-09-01');
  await live.locator('.commit-row').filter({has:page.locator('code', {hasText:'2222222222'})}).click();
  await expect(page.getByText('Capture generation 2', {exact:true})).toBeVisible();
  const initialReads = fullReads;
  await page.waitForTimeout(6_500);
  expect(fullReads).toBe(initialReads); // reconnects with unchanged revisions
  generation = 3;
  await page.clock.fastForward(6_000);
  await expect(page.locator('.viewer-head code')).toHaveText('3333333333');
  await expect(page.getByText('Capture generation 3', {exact:true})).toBeVisible();
  await expect(page.locator('.live-viewer-status')).toContainText('LIVE');
  expect(fullReads).toBe(initialReads);
  expect(pendingReads).toBeGreaterThan(0);
  // A capture with an ancestor absent from the browser cache requires one
  // coherent refresh, then returns to cheap pending updates.
  generation = 7;
  await page.clock.fastForward(6_000);
  await expect(page.getByText('Capture generation 7', {exact:true})).toBeVisible();
  expect(fullReads).toBe(initialReads + 1);
  await page.clock.fastForward(6_000);
  expect(fullReads).toBe(initialReads + 1);
  offline = true;
  await page.clock.fastForward(125_000);
  await expect(page.locator('.pending-sessions[data-live="true"]')).toHaveCount(0);
  await expect(page.locator('.pending-sessions[data-live="false"] .commit-row')).toHaveCount(3);
  await expect(page.locator('.live-viewer-status')).not.toContainText('LIVE');
  await expect(page.getByText('Capture generation 7', {exact:true})).toBeVisible();
  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});
