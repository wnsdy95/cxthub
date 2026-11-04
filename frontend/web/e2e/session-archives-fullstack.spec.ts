import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { expect, test } from '@playwright/test';

function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  if (value && typeof value === 'object') return `{${Object.entries(value).sort(([left], [right]) => left.localeCompare(right)).map(([key, item]) => `${JSON.stringify(key)}:${canonical(item)}`).join(',')}}`;
  return JSON.stringify(value);
}

const hash = (value: string) => `sha256:${createHash('sha256').update(value).digest('hex')}`;

for (const outcome of ['success', 'interrupted-response'] as const) test(`real raw export and archive retain their lifetimes through view-mode changes: ${outcome}`, async ({page}, info) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires isolated PostgreSQL and real API');
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  const api = page.context().request;
  const headers = {Origin: 'http://127.0.0.1:4174', 'X-Cxt-CSRF': '1', 'X-Cxt-Doc-Identities': 'cxt-manifest-sha256-v1'};
  const suffix = `${Date.now()}-${info.retry}`;
  const login = await api.post('/api/v1/auth/session', {headers: {...headers, Authorization: `Bearer dev:mode-actions-${suffix}@example.test:Mode Actions`}});
  expect(login.ok(), await login.text()).toBe(true);
  const user = await (await api.get('/api/v1/me')).json();
  const created = await api.post('/api/v1/repositories', {headers, data: {name: `ModeActions${suffix}`}});
  expect(created.ok(), await created.text()).toBe(true);
  const metadata = await created.json();
  const remote = `http://cxthub.test/${user.username}/${metadata.slug}`;
  const repo = hash(remote.replace('http://', '').toLowerCase());
  const registered = await api.post('/api/v1/repos', {headers, data: {id: repo, remote_url: remote, default_branch: 'main'}});
  expect(registered.ok(), await registered.text()).toBe(true);
  const base = `/api/v1/repos/${encodeURIComponent(repo)}`;
  const cir = {envelope: {cir_version: '2', source_provider: 'codex', source_model: 'test', captured_at: '2026-10-30T00:00:00Z',
    cwd: '', git_branch: 'main', session_origin_id: `mode-actions-${suffix}`, fidelity: 'full'}, events: [
    {kind: 'message', seq: 0, role: 'user', blocks: [{type: 'text', text: 'Persistent prompt for view-mode changes'}]},
    {kind: 'message', seq: 1, role: 'assistant', blocks: [{type: 'text', text: 'Persistent response for view-mode changes'}]},
  ]};
  const documentHash = hash(canonical(cir));
  const submitted = await api.post(`${base}/push/objects`, {headers, data: {docs: [{hash: documentHash, cir}], snapshots: [{
    id: documentHash, doc_hash: documentHash, repo_id: repo, branch: 'main', provider: 'codex', session_id: cir.envelope.session_origin_id,
    fidelity: 'full', message: 'Mode action lifetime', parents: [], created_at: cir.envelope.captured_at,
  }]}});
  expect(submitted.ok(), await submitted.text()).toBe(true);
  const referenced = await api.put(`${base}/refs/branch/main`, {headers, data: {target: documentHash, expected_target: ''}});
  expect(referenced.ok(), await referenced.text()).toBe(true);
  await page.goto(`/${user.username}/${metadata.slug}`);
  const viewer = page.locator('.viewer');
  await expect(viewer).toContainText('Persistent response for view-mode changes');
  const before = await (await api.get(`${base}/view`, {headers})).json();
  const mode = viewer.getByRole('combobox', {name: 'View mode'});
  const raw = viewer.getByRole('button', {name: '↓ raw', exact: true});
  let releaseRaw!: () => void;
  const rawWait = new Promise<void>(resolve => { releaseRaw = resolve; });
  let rawReads = 0;
  await page.route(url => url.pathname === `${base}/docs/${encodeURIComponent(documentHash)}`, async route => {
    rawReads++;
    const response = await route.fetch();
    expect(response.ok(), await response.text()).toBe(true);
    await rawWait;
    await route.fulfill({response});
  });
  const downloaded = page.waitForEvent('download');
  await raw.click();
  try {
    await expect.poll(() => rawReads).toBe(1);
    for (const value of ['prompts', 'all', 'chat']) {
      await mode.selectOption(value);
      await expect(raw).toBeDisabled();
      await expect(viewer).toContainText('Persistent prompt for view-mode changes');
      if (value === 'prompts') await expect(viewer.getByText('Persistent response for view-mode changes', {exact: true})).toBeHidden();
      else await expect(viewer.getByText('Persistent response for view-mode changes', {exact: true})).toBeVisible();
    }
  } finally { releaseRaw(); }
  const file = await downloaded;
  expect(file.suggestedFilename()).toBe(`${documentHash.slice(7, 17)}-context.json`);
  expect(JSON.parse(await readFile((await file.path())!, 'utf8'))).toEqual(cir);
  expect(rawReads).toBe(1);
  let releaseArchive!: () => void;
  const archiveWait = new Promise<void>(resolve => { releaseArchive = resolve; });
  let archiveWrites = 0;
  const archiveIntents: boolean[] = [];
  await page.route(url => url.pathname === `${base}/snapshots/${encodeURIComponent(documentHash)}/archive`, async route => {
    archiveWrites++;
    archiveIntents.push(route.request().postDataJSON().archived);
    const response = await route.fetch();
    expect(response.ok(), await response.text()).toBe(true);
    await archiveWait;
    await route.fulfill(outcome === 'interrupted-response' && archiveWrites === 1
      ? {status: 503, json: {error: {message: 'Archive response interrupted'}}} : {response});
  });
  await viewer.getByRole('button', {name: 'Archive session', exact: true}).click();
  const dialog = page.getByRole('dialog', {name: 'Archive session'});
  await dialog.getByRole('button', {name: 'Archive session', exact: true}).click();
  try {
    await expect.poll(() => archiveWrites).toBe(1);
    await expect(viewer.locator('.session-archive-notice')).toBeVisible();
    for (const value of ['prompts', 'all', 'chat']) {
      await mode.selectOption(value);
      await expect(dialog.getByRole('status')).toHaveText('Saving session archive…');
      await expect(dialog.getByRole('button', {name: 'Archive session', exact: true})).toBeDisabled();
    }
  } finally { releaseArchive(); }
  if (outcome === 'interrupted-response') {
    await expect(dialog.getByRole('alert')).toHaveText('Archive response interrupted');
    await mode.selectOption('prompts');
    await expect(dialog).toHaveAttribute('aria-label', 'Archive session');
    await dialog.getByRole('button', {name: 'Archive session', exact: true}).click();
  }
  await expect(dialog).toHaveCount(0);
  await expect(viewer.getByRole('button', {name: 'Restore session', exact: true})).toBeVisible();
  await expect(viewer.locator('.viewer-head > code')).toHaveText(documentHash.slice(7, 17));
  const archived = await (await api.get(`${base}/view`, {headers})).json();
  expect(archived.archived_sessions).toHaveLength(1);
  expect(archived.refs).toEqual(before.refs);
  expect(archived.snapshots).toEqual(before.snapshots);
  expect(archiveWrites).toBe(outcome === 'success' ? 1 : 2);
  expect(archiveIntents).toEqual(outcome === 'success' ? [true] : [true, true]);
  await viewer.getByRole('button', {name: 'Restore session', exact: true}).click();
  await page.getByRole('dialog').getByRole('button', {name: 'Restore session', exact: true}).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(viewer.getByRole('button', {name: 'Archive session', exact: true})).toBeVisible();
  const restored = await (await api.get(`${base}/view`, {headers})).json();
  expect(restored.archived_sessions).toEqual([]);
  expect(restored.refs).toEqual(before.refs);
  expect(restored.snapshots).toEqual(before.snapshots);
  expect(archiveWrites).toBe(outcome === 'success' ? 2 : 3);
  expect(archiveIntents).toEqual(outcome === 'success' ? [true, false] : [true, true, false]);
  expect(errors).toEqual([]);
});

test('real API and PostgreSQL preserve archived sessions through live capture and restore', async ({page}, info) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires isolated PostgreSQL and real API');
  const api = page.context().request;
  const headers = {Origin: 'http://127.0.0.1:4174', 'X-Cxt-CSRF': '1', 'X-Cxt-Doc-Identities': 'cxt-manifest-sha256-v1'};
  const login = await api.post('/api/v1/auth/session', {headers: {...headers, Authorization: 'Bearer dev:session-archives@example.test:Session Archives'}});
  expect(login.ok(), await login.text()).toBe(true);
  const user = await (await api.get('/api/v1/me')).json();
  const metadataResponse = await api.post('/api/v1/repositories', {headers, data: {name: `SessionArchives${info.retry}`}});
  expect(metadataResponse.ok(), await metadataResponse.text()).toBe(true);
  const metadata = await metadataResponse.json();
  const remote = `http://cxthub.test/${user.username}/${metadata.slug}`;
  const repo = hash(remote.replace('http://', '').toLowerCase());
  const register = await api.post('/api/v1/repos', {headers, data: {id: repo, remote_url: remote, default_branch: 'main'}});
  expect(register.ok(), await register.text()).toBe(true);
  const base = `/api/v1/repos/${encodeURIComponent(repo)}`;
  const docs = ['First message', 'Latest message'].map((text, index) => {
    const cir = {envelope: {cir_version: '2', source_provider: 'codex', source_model: 'test', captured_at: `2026-10-01T00:00:0${index}Z`, cwd: '', git_branch: 'main', session_origin_id: 'archive-session', fidelity: 'full'},
      events: [{kind: 'message', seq: 0, role: 'user', blocks: [{type: 'text', text}]}]};
    return {hash: hash(canonical(cir)), cir};
  });
  const snapshots = docs.map((doc, index) => ({id: doc.hash, doc_hash: doc.hash, repo_id: repo, branch: 'main', provider: 'codex', session_id: 'archive-session', fidelity: 'full',
    message: index ? 'hook: latest' : 'Committed context', parents: index ? [docs[0].hash] : [], created_at: doc.cir.envelope.captured_at}));
  const objects = await api.post(`${base}/push/objects`, {headers, data: {docs, snapshots}});
  expect(objects.ok(), await objects.text()).toBe(true);
  expect((await api.put(`${base}/refs/branch/main`, {headers, data: {target: docs[0].hash, expected_target: ''}})).ok()).toBe(true);
  const readView = async () => (await api.get(`${base}/view`, {headers})).json();
  const before = await readView();
  await page.goto(`/${user.username}/${metadata.slug}`);
  await expect(page.locator('.viewer')).toContainText('First message');
  await page.locator('.viewer').getByRole('button', {name: 'Archive session', exact: true}).click();
  await page.getByRole('dialog').getByRole('button', {name: 'Archive session', exact: true}).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.locator('.session-archive-notice')).toBeVisible();
  const archived = await readView();
  expect(archived.archived_sessions).toHaveLength(1);
  expect(archived.archived_sessions[0].snapshot_ids.sort()).toEqual(docs.map(doc => doc.hash).sort());
  expect(archived.refs).toEqual(before.refs);
  expect(archived.snapshots).toEqual(before.snapshots);
  const pending = await api.put(`${base}/pending/archive-session`, {headers, data: {target: docs[1].hash, provider: 'codex', branch: 'main'}});
  expect(pending.ok(), await pending.text()).toBe(true);
  const live = await (await api.get(`${base}/pending-view`, {headers})).json();
  expect(live.archived_sessions).toEqual(archived.archived_sessions);
  await page.reload();
  const panel = page.locator('.graph-session-archives');
  await panel.locator(':scope > summary').click();
  await panel.locator('.graph-session-archive > summary').click();
  await panel.getByRole('button', {name: 'Open archived session codex / archive-session'}).click();
  await expect(page.locator('.viewer')).toContainText('Latest message');
  await expect(page.locator(`[data-graph-snapshot="${docs[1].hash}"]`)).toHaveCount(0);
  await page.locator('.viewer').getByRole('button', {name: 'Restore session', exact: true}).click();
  await page.getByRole('dialog').getByRole('button', {name: 'Restore session', exact: true}).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(panel.locator(':scope > summary')).toHaveText('Archived sessions (0)');
  const restored = await readView();
  expect(restored.archived_sessions).toEqual([]);
  expect(restored.refs).toEqual(before.refs);
  expect(restored.snapshots).toEqual(before.snapshots);
  expect(restored.pending).toHaveLength(1);
});
