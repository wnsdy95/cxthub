import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  CONVERSATION_MODES, DEFAULT_CONVERSATION_MODE, conversationAnswers, conversationEventIsPrompt, conversationEventIsShown, conversationTurns,
} from '../src/conversationView';
import type { DocViewEvent } from '../src/types';

type Event = DocViewEvent & { seed_summary?: boolean; empty_reasoning?: boolean };
let next = 0;
const event = (extra: Partial<Event>): Event => ({ kind: 'message', event_index: next++, ...extra });
const said = (role: 'user' | 'assistant', text: string) => event({ role, blocks: [{ type: 'text', text }] });
const indexes = (events: Event[]) => events.map(shown => shown.event_index);

test('the conversation opens with only what was said', () => {
  assert.equal(DEFAULT_CONVERSATION_MODE, 'chat');
  assert.deepEqual([...CONVERSATION_MODES], ['chat', 'prompts', 'all']);
});

test('a prompt is what the user wrote, not a summary of an earlier context', () => {
  assert.equal(conversationEventIsPrompt(said('user', 'Fix the parser')), true);
  assert.equal(conversationEventIsPrompt(said('assistant', 'Fixed.')), false);
  assert.equal(conversationEventIsPrompt(event({ kind: 'tool_result', role: 'user', output: 'ok' })), false);
  assert.equal(conversationEventIsPrompt(event({ role: 'user', compact_summary: true, blocks: [{ type: 'text', text: 'Earlier work' }] })), false);
  assert.equal(conversationEventIsPrompt(said('user', '[cxt seed] Branch-switch context: main')), false);
});

test('reasoning without a readable summary draws nothing and is not counted', () => {
  assert.equal(conversationEventIsShown(event({ kind: 'reasoning' })), false);
  assert.equal(conversationEventIsShown(event({ kind: 'reasoning', redacted_summary: '  ' })), false);
  assert.equal(conversationEventIsShown(event({ kind: 'reasoning', redacted_summary: 'Looked at the parser' })), true);
  assert.equal(conversationEventIsShown(event({ kind: 'reasoning', deferred: true })), true, 'a described event is read when opened');
  assert.equal(conversationEventIsShown(event({ kind: 'reasoning', deferred: true, empty_reasoning: true })), false,
    'the server says of a described reasoning event that it has no summary');
  assert.equal(conversationEventIsShown(event({ kind: 'tool_call', empty_reasoning: true })), false);
});

test('each prompt starts a turn; what came before the first prompt has none', () => {
  const lead = event({ kind: 'tool_call', tool_name: 'Read' });
  const first = said('user', 'Fix the parser');
  const silent = event({ kind: 'reasoning' });
  const call = event({ kind: 'tool_call', tool_name: 'Edit' });
  const answer = said('assistant', 'Fixed.');
  const second = said('user', 'Add a test');
  const turns = conversationTurns([lead, first, silent, call, answer, second]);
  assert.deepEqual(turns.map(turn => ({ prompt: turn.prompt?.event_index ?? null, rest: indexes(turn.rest) })), [
    { prompt: null, rest: [lead.event_index] },
    { prompt: first.event_index, rest: [call.event_index, answer.event_index] },
    { prompt: second.event_index, rest: [] },
  ]);
  assert.deepEqual(conversationTurns([]), []);
});

test('an answer carries the work since the answer before it; work read without its answer stays on its own', () => {
  const read = event({ kind: 'tool_call', tool_name: 'Read' });
  const result = event({ kind: 'tool_result', role: 'user', output: 'source' });
  const first = said('assistant', 'The parser drops the last line.');
  const second = said('assistant', 'Fixing it now.');
  const edit = event({ kind: 'tool_call', tool_name: 'Edit' });
  const answers = conversationAnswers([read, result, first, second, edit]);
  assert.deepEqual(answers.map(answer => ({ work: indexes(answer.work), answer: answer.answer?.event_index ?? null })), [
    { work: [read.event_index, result.event_index], answer: first.event_index },
    { work: [], answer: second.event_index },
    { work: [edit.event_index], answer: null },
  ]);
  const summary = event({ role: 'user', compact_summary: true, blocks: [{ type: 'text', text: 'Earlier work' }] });
  assert.deepEqual(conversationAnswers([summary]).map(answer => answer.answer?.event_index), [summary.event_index], 'a summary is said, not work');
});
