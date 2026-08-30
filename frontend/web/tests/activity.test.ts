import assert from 'node:assert/strict';
import { normalizeActivityResponse } from '../src/activity.ts';

assert.deepEqual(normalizeActivityResponse(null), { months: [] });
assert.deepEqual(normalizeActivityResponse({ months: null }), { months: [] });

const normalized = normalizeActivityResponse({
  months: [
    {
      month: '2026-08',
      commit_total: 2,
      commit_repos: [{ name: 'repo', path: 'alice/repo', count: 2 }],
      created: null,
    },
    {
      month: '2026-07',
      commit_total: 0,
      commit_repos: null,
      created: [{ name: 'new', path: 'alice/new', visibility: 'private', date: '2026-07-03' }],
    },
  ],
});

assert.deepEqual(normalized.months[0], {
  month: '2026-08',
  commit_total: 2,
  commit_repos: [{ name: 'repo', path: 'alice/repo', count: 2 }],
  created: [],
});
assert.deepEqual(normalized.months[1], {
  month: '2026-07',
  commit_total: 0,
  commit_repos: [],
  created: [{ name: 'new', path: 'alice/new', visibility: 'private', date: '2026-07-03' }],
});

assert.deepEqual(
  normalizeActivityResponse({
    months: [{ month: '2026-06', commit_total: 'bad', commit_repos: [{}], created: [null] }, null],
  }),
  { months: [{ month: '2026-06', commit_total: 0, commit_repos: [], created: [] }] },
  'malformed activity data is contained at the API boundary',
);
