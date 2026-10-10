import { createHash } from 'node:crypto';
import { expect, test } from '@playwright/test';

function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  if (value && typeof value === 'object') return `{${Object.entries(value).sort(([left], [right]) => left.localeCompare(right)).map(([key, item]) => `${JSON.stringify(key)}:${canonical(item)}`).join(',')}}`;
  return JSON.stringify(value);
}

const hash = (value: string) => `sha256:${createHash('sha256').update(value).digest('hex')}`;

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
