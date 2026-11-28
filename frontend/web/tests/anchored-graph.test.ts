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

test('a squash merge is joined to the branch it came from by a dotted lane, and reachability ignores it', () => {
  // main: a - s (pull request of f1 - f2, merged by squash as s); the branch keeps f2 as its newest commit.
  const squashed = [node('a', 1), node('f1', 2, 'a'), node('f2', 3, 'f1'), { ...node('s', 4, 'a'), joined: ['f2'] }];
  const { rows } = layoutGitGraph(squashed, 's');
  const shape = Object.fromEntries(rows.map(row => [row.node.sha, { lane: row.lane, out: row.branchesOut, soft: row.softOut, merges: row.mergesIn }]));
  assert.deepEqual(shape, {
    s: { lane: 0, out: [1], soft: [1], merges: [] },
    f2: { lane: 1, out: [], soft: [], merges: [] },
    f1: { lane: 1, out: [], soft: [], merges: [] },
    a: { lane: 0, out: [], soft: [], merges: [1] },
  });
  const [s, f2, f1] = rows;
  assert.deepEqual(s.outgoingSoft, [false, true], 'from the squash commit down to the branch the lane is dotted');
  assert.deepEqual(f2.incomingSoft, [false, true]);
  assert.deepEqual(f2.outgoingSoft, [false, false], 'below the branch its own parents are solid lines');
  assert.deepEqual(f1.incomingSoft, [false, false]);
  assert.equal(rows.some(row => row.cut), false);
  const byId = new Map(squashed.map(entry => [entry.sha, entry]));
  assert.deepEqual(commitsNotOn(byId, 'f2', 's'), { shas: ['f2', 'f1'], complete: true }, 'the squashed branch is not reachable from main');
});

test('a joined commit that a real line already reaches only adds a dotted curve', () => {
  // c is a child of f2 above s: the lane to f2 is a line of its own, and stays solid.
  const history = [node('a', 1), node('f2', 2, 'a'), { ...node('s', 3, 'a'), joined: ['f2'] }, node('c', 4, 'f2')];
  const { rows } = layoutGitGraph(history, 's');
  const s = rows.find(row => row.node.sha === 's')!;
  assert.deepEqual(s.softOut, s.branchesOut);
  assert.equal(s.outgoingSoft[s.branchesOut[0]], false);
});

test('joins that would close a loop are left out and the graph is the plain one', () => {
  // s joined f2, but f2 was made on top of s: drawing the join would loop.
  const looped = [node('a', 1), { ...node('s', 2, 'a'), joined: ['f2'] }, node('f2', 3, 's')];
  const { rows } = layoutGitGraph(looped);
  assert.equal(rows.length, looped.length, 'no commit is lost');
  assert.equal(rows.some(row => row.softOut.length > 0 || row.incomingSoft.some(Boolean) || row.outgoingSoft.some(Boolean)), false);
  assert.deepEqual(rows.map(row => row.node.sha), ['f2', 's', 'a']);
});

test('a dotted lane turns solid where a merge commit below reaches the same commit by a real parent', () => {
  // s (newest, on main) joined f2; m, older, merged f2 for real: from m down the lane to f2 is a Git line.
  const history = [node('a', 1), node('f2', 2, 'a'), node('x', 3, 'a'), node('m', 4, 'x', 'f2'), { ...node('s', 5, 'a'), joined: ['f2'] }];
  const { rows } = layoutGitGraph(history, 's');
  const s = rows.find(row => row.node.sha === 's')!;
  const m = rows.find(row => row.node.sha === 'm')!;
  const lane = s.softOut[0];
  assert.equal(s.outgoingSoft[lane], true);
  assert.deepEqual(m.branchesOut, [lane], 'the merge commit reaches f2 on the same lane');
  assert.deepEqual(m.softOut, []);
  assert.equal(m.incomingSoft[lane], true, 'above the merge commit the lane is still the dotted join');
  assert.equal(m.outgoingSoft[lane], false, 'below it the lane is the merge commit\'s own line');
});

test('a joined commit never takes the lane of a commit whose parent is outside the window', () => {
  // s squashed f1 - f2, which were made after p; the window ends before p.
  const window = [node('f1', 2, 'z'), node('f2', 3, 'f1'), { ...node('s', 4, 'p'), joined: ['f2'] }];
  const { rows } = layoutGitGraph(window, 's');
  const s = rows.find(row => row.node.sha === 's')!;
  assert.equal(s.lane, 0);
  assert.equal(s.cut, true);
  assert.equal(s.outgoing[0] ?? null, null, 'main\'s lane ends, so the row draws that history goes on below');
  assert.deepEqual(s.softOut, s.branchesOut);
  assert.notEqual(s.softOut[0], 0, 'the dotted line leaves for a lane of its own');
  for (const sha of ['f2', 'f1']) assert.notEqual(rows.find(row => row.node.sha === sha)!.lane, 0, `${sha} is not drawn as main's own history`);
});
