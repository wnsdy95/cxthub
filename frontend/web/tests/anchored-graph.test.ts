import assert from 'node:assert/strict';
import { test } from 'node:test';
import { commitsNotOn, layoutGitGraph, orderGitNodes, type GitNode } from '../src/anchoredGraph';

const at = (minute: number) => `2031-01-02T03:${String(minute).padStart(2, '0')}:00Z`;
const node = (sha: string, minute: number, ...parents: string[]): GitNode => ({ sha, parents, at: at(minute) });

// main: a - b - d - m (merge of f2), feature: b - f1 - f2, topic: d - t1 (not merged)
const history = [node('a', 1), node('b', 2, 'a'), node('f1', 3, 'b'), node('d', 4, 'b'), node('f2', 5, 'f1'), node('m', 6, 'd', 'f2'), node('t1', 7, 'd')];
const byId = new Map(history.map(entry => [entry.sha, entry]));

test('commits are ordered children first, the newest ready commit first', () => {
  assert.deepEqual(orderGitNodes(history).map(entry => entry.sha), ['t1', 'm', 'f2', 'd', 'f1', 'b', 'a']);
  // A child whose clock is behind its parent still comes before it.
  assert.deepEqual(orderGitNodes([node('parent', 9), node('child', 1, 'parent')]).map(entry => entry.sha), ['child', 'parent']);
  assert.deepEqual(orderGitNodes([]), []);
});

test('lanes follow first parents, open for a merged line and close where lines meet', () => {
  const { rows, laneCount } = layoutGitGraph(history, 'm');
  assert.equal(laneCount, 3);
  const shape = Object.fromEntries(rows.map(row => [row.node.sha, [row.lane, row.mergesIn, row.branchesOut, row.cut]]));
  assert.deepEqual(shape, {
    t1: [1, [], [], false],
    m: [0, [], [2], false],
    f2: [2, [], [], false],
    d: [0, [1], [], false],
    f1: [2, [], [], false],
    b: [0, [2], [], false],
    a: [0, [], [], false],
  });
  assert.deepEqual(rows[0].incoming, ['m'], 'the pinned line holds the first lane before its commit is reached');
  assert.deepEqual(rows.at(-1)!.outgoing, [], 'the root ends its lane');
});

test('a parent outside the window is not drawn and the row says history goes on', () => {
  const window = history.filter(entry => !['a', 'b'].includes(entry.sha));
  const { rows } = layoutGitGraph(window);
  assert.deepEqual(rows.filter(row => row.cut).map(row => row.node.sha), ['d', 'f1']);
  assert.deepEqual(rows.find(row => row.node.sha === 'd')!.outgoing.filter(Boolean), ['f1'], 'no lane waits for a commit that will not come');
});

test('a branch holds the commits its base does not have yet', () => {
  assert.deepEqual(commitsNotOn(byId, 't1', 'm'), { shas: ['t1'], complete: true });
  assert.deepEqual(commitsNotOn(byId, 'f2', 'd'), { shas: ['f2', 'f1'], complete: true });
  assert.deepEqual(commitsNotOn(byId, 'f2', 'm'), { shas: [], complete: true }, 'a merged branch holds nothing of its own');
  assert.deepEqual(commitsNotOn(byId, 'm', 'm'), { shas: [], complete: true });
  assert.deepEqual(commitsNotOn(byId, 'unknown', 'm'), { shas: [], complete: false });
  assert.deepEqual(commitsNotOn(byId, 't1', 'unknown'), { shas: [], complete: false });
  // The window ends before the place where the branch left its base.
  const window = new Map(history.filter(entry => !['a', 'b'].includes(entry.sha)).map(entry => [entry.sha, entry]));
  assert.deepEqual(commitsNotOn(window, 'f2', 'd'), { shas: ['f2', 'f1'], complete: false });
});
