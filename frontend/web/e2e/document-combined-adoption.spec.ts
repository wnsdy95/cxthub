// How a session's conversation reads what its pages leave out: a record too
// large for a page is read whole when asked for, and one too large to read
// whole is read a part at a time. Every read goes through the session, so the
// server holds it to what the reader may see. The CI workflow runs this file by
// name in a step of its own, with the render window switch on.
import { expect, test } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';

const at = (minute: number) => `2031-01-02T03:${String(minute).padStart(2, '0')}:00Z`;
const hash = `sha256:${'0'.repeat(64)}`;
const doc = `sha256:${'2'.repeat(64)}`;
const sha = 'a'.repeat(40);
const wholeBytes = 96 * 1024;
const partBytes = 64 * 1024;
const partsTotal = 2 * partBytes + 1024;

const summary = { provider: 'claude', session_id: 'reader-session', sub: false, revision: 1, body_hash: hash, records: 4, commands: 1, segments: 1, machine: 'laptop',
  started_at: at(0), ended_at: at(30), content: true, updated_at: at(31),
  commits: [{ sha, segment_no: 1, rule: 'printed-number', subject: 'Read large records', created_at: at(20) }],
  uncommitted: { commands: 0, unplaced: true, places: [] } };
const session = { repo_id: 'repo', provider: 'claude', session_id: 'reader-session', uploader: 'member', revision: 1, body_hash: hash, created_at: at(31), updated_at: at(31),
  body: { rule_version: 1, sub: false, scope: 'working-folder', machine: 'laptop', started_at: at(0), ended_at: at(30), records: 4, commands: 1, copied_records: 0,
    segments: [{ first: 'm1', last: 'm4', first_ordinal: 0, last_ordinal: 3, commands: 1, events: 4, closes_turn: true, commits: [{ sha, rule: 'printed-number', subject: 'Read large records' }] }],
    uncommitted: { commands: 0, unplaced: true, places: [] }, content: { doc, events: 4 } } };
// The page carries two messages whole and leaves two records out: one small enough to read whole, one that is not.
const events = [
  { kind: 'message', role: 'user', seq: 0, event_index: 0, blocks: [{ type: 'text', text: 'Show me the build log.' }] },
  { kind: 'tool_result', seq: 1, event_index: 1, deferred: true, event_bytes: wholeBytes },
  { kind: 'tool_result', seq: 2, event_index: 2, deferred: true, event_bytes: partsTotal },
  { kind: 'message', role: 'assistant', seq: 3, event_index: 3, blocks: [{ type: 'text', text: 'The build log is above.' }] },
];
const part = (offset: number) => {
  const size = Math.min(partBytes, partsTotal - offset);
  const marker = `part-at-${offset} `;
  return { event_index: 2, byte_offset: offset, next: offset + size === partsTotal ? -1 : offset + size, total_bytes: partsTotal,
    json_fragment: marker + 'x'.repeat(size - marker.length) };
};

test('a large record of a session conversation is read whole or in parts, always through the session', async ({ page }) => {
  const errors = capturePageErrors(page);
  const reads: string[] = [];
  const state = { failWhole: true };
  const unexpected = await installApiFixture(page, ({ method, pathname, searchParams }) => {
    if (method !== 'GET') return undefined;
    if (pathname === '/api/v1/me') return { body: { id: 'member', username: 'alice', locale: 'en' } };
    if (pathname === '/api/v1/repositories') return { body: [{ id: 'repository', name: 'app', slug: 'app', owner_username: 'alice', visibility: 'private', effective_role: 'member' }] };
    if (pathname === '/api/v1/repositories/repository/members') return { body: [{ repository_id: 'repository', user_id: 'member', role: 'member' }] };
    if (pathname === '/api/v1/repos') return { body: [{ id: 'repo', default_branch: 'main', remote_url: 'https://example.test/alice/app' }] };
    const resource = decodeURIComponent(pathname).match(/^\/api\/v1\/repos\/repo\/(.+)$/)?.[1];
    if (!resource) return undefined;
    if (resource.startsWith('settings/') || resource === 'secrets') return { body: null };
    if (resource === 'anchored-sessions') return { body: [summary] };
    if (resource === 'anchored-sessions/claude/reader-session') return { body: session };
    if (resource === 'anchored-sessions/claude/reader-session/calls') return { body: [] };
    if (/^anchored-commits\/[0-9a-f]{40}\/summaries$/.test(resource)) return { body: [] };
    if (resource === 'anchored-sessions/claude/reader-session/events') {
      reads.push(`events?${searchParams}`);
      if (searchParams.get('view') === 'raw') {
        if (searchParams.get('offset') !== '1' || searchParams.get('limit') !== '1') return undefined;
        if (state.failWhole) return { status: 503, body: { error: { code: 'unavailable', message: 'The record could not be read.' } } };
        return { body: { offset: 1, next: 2, events: [{ kind: 'tool_result', seq: 1, event_index: 1, output: 'whole build log' }] } };
      }
      return { body: { offset: 0, next: -1, events } };
    }
    if (resource === 'anchored-sessions/claude/reader-session/event-fragments') {
      reads.push(`event-fragments?${searchParams}`);
      if (searchParams.get('index') !== '2' || searchParams.get('budget') !== String(partBytes)) return undefined;
      return { body: part(Number(searchParams.get('offset'))) };
    }
    return undefined;
  });

  await page.goto('/alice/app?tab=sessions&session=claude%3Areader-session');
  const conversation = page.getByRole('region', { name: 'Conversation' });
  await expect(conversation.getByText('Show me the build log.', { exact: true })).toBeVisible();
  await expect(conversation.getByText('The build log is above.', { exact: true })).toBeVisible();
  // Nothing of a record that the page left out is read before it is asked for.
  expect(reads).toEqual(['events?offset=0&limit=20']);
  // The records are the agent's work before its answer: they show when that work is opened.
  await expect(conversation.locator('[data-deferred-event]')).toHaveCount(0);
  await conversation.getByRole('button', { name: '2 details', exact: true }).click();
  await expect(conversation.locator('[data-deferred-event]')).toHaveCount(2);
  expect(reads).toEqual(['events?offset=0&limit=20']);

  // A record small enough is read whole. A failure says so and is read again only when asked.
  const whole = conversation.locator('[data-deferred-event="1"]');
  await expect(whole.locator('summary')).toContainText(`${wholeBytes.toLocaleString('en-US')} B`);
  await whole.locator('summary').click();
  await expect(whole.getByRole('alert')).toContainText('The record could not be read.');
  const failedReads = reads.filter(read => read === 'events?view=raw&offset=1&limit=1').length;
  expect(failedReads).toBeGreaterThan(0);
  state.failWhole = false;
  await whole.getByRole('button', { name: 'Retry' }).click();
  await expect(whole.locator('pre')).toHaveText('whole build log');
  expect(reads.filter(read => read === 'events?view=raw&offset=1&limit=1')).toHaveLength(failedReads + 1);

  // A record too large to read whole is read a part at a time, forward and back, each part once.
  const parts = conversation.locator('[data-deferred-event="2"]');
  await parts.locator('summary').click();
  const fragment = parts.locator('.event-fragment');
  await expect(fragment).toHaveAttribute('data-fragment-offset', '0');
  await expect(fragment.locator('pre')).toContainText('part-at-0 ');
  await expect(fragment.getByRole('button', { name: 'Previous fragment' })).toBeDisabled();
  await fragment.getByRole('button', { name: 'Next fragment' }).click();
  await expect(fragment).toHaveAttribute('data-fragment-offset', String(partBytes));
  await expect(fragment.locator('pre')).toContainText(`part-at-${partBytes} `);
  await fragment.getByRole('button', { name: 'Next fragment' }).click();
  await expect(fragment.locator('pre')).toContainText(`part-at-${2 * partBytes} `);
  await expect(fragment.getByRole('button', { name: 'Next fragment' })).toBeDisabled();
  await fragment.getByRole('button', { name: 'Previous fragment' }).click();
  await expect(fragment.locator('pre')).toContainText(`part-at-${partBytes} `);
  expect(reads.filter(read => read.startsWith('event-fragments?'))).toEqual(
    [0, partBytes, 2 * partBytes].map(offset => `event-fragments?index=2&offset=${offset}&budget=${partBytes}`));

  expect(errors).toEqual([]);
  expect(unexpected).toEqual([]);
});
