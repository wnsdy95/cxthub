import { test, expect } from '@playwright/test';
import { installApiFixture, capturePageErrors } from './api-fixture';

const repo = 'repo-audit', repository = { id: 'repository-audit', name: 'project', slug: 'project', owner_id: 'owner', effective_role: 'owner', owner_username: 'alice', visibility: 'private' };
for (const conflict of [false, true]) test(`GitHub sync audit runs only on click and preserves partial results: conflict=${conflict}`, async ({ page }) => {
 const errors = capturePageErrors(page); let calls = 0;
 const unexpected = await installApiFixture(page, r => {
  if (r.pathname.endsWith('/github-sync-check')) {
   expect(r.method).toBe('POST'); calls++;
   if (calls === 2 && conflict) return { status: 409, body: {error: {code:'conflict', message:'Evidence changed; restart'}} };
   return {body: {version:1,revision:'a'.repeat(64),checked_at:'2026-09-21T00:00:00Z',processed:calls,total:2,next_cursor:calls === 1 ? 'next-page' : '',checks:calls === 1 ? [{id:'creation',event_id:'event-1',branch:'feature',state:'incomplete',code:'command_unavailable'}] : [{id:'remote',branch:'feature',state:'verified',code:'github_pr_matches',creation:{evidence:'process-argv',command:['git','checkout','-b','feature','main'],origin_branch:'main',origin_branch_id:'main-id'}}]}};
  }
  if (r.method !== 'GET') return undefined;
  if (r.pathname === '/api/v1/me') return {body:{id:'owner',username:'alice',locale:'en',email:'alice@example.test'}};
  if (r.pathname === '/api/v1/repositories') return {body:[repository]};
  if (r.pathname === '/api/v1/repos') return {body:[{id:repo,default_branch:'main',context_protocol:1}]};
  if (r.pathname.endsWith('/members')) return {body:[{repository_id:repository.id,user_id:'owner',role:'owner'}]};
  if (r.pathname.endsWith('/view')) return {body:{refs:[],snapshots:[],history:[],reflog:[],pending:[],unsync:[]}};
  if (/\/(refs|snapshots|pending|unsync|history|reflog|notifications|invites)$/.test(r.pathname)) return {body:[]};
  if (r.pathname.endsWith('/secrets')) return {body:null};
  return undefined;
 });
 await page.goto('/alice/project?tab=settings');
 const section = page.getByRole('region',{name:'GitHub sync check'});
 await expect(section.getByRole('button',{name:'Check GitHub sync'})).toBeVisible();
 expect(calls).toBe(0);
 await section.getByRole('button',{name:'Check GitHub sync'}).click();
 await expect(section).toContainText('Creation command evidence is unavailable');
 if (conflict) {await expect(section.getByRole('alert')).toContainText('Evidence changed');await expect(section).toContainText('Partial results');await expect(section).not.toContainText('Check finished');}
 else {await expect(section).toContainText('Check finished');await section.getByRole('checkbox').uncheck();await expect(section).toContainText('git checkout -b feature main');await expect(section).toContainText('The GitHub PR merge matches');}
 expect(calls).toBe(2);expect(errors).toEqual([]);expect(unexpected).toEqual([]);
});
