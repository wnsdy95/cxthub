// How a session's conversation is shown. By default only what the user and
// the agent said to each other: the agent's work before an answer (tool calls,
// their results, reasoning) is folded behind that answer and opened on demand.
import type { DocViewEvent } from './types';
import { documentEventIsSummary } from './documentEventClassification';

type ShownEvent = DocViewEvent & { seed_summary?: boolean; empty_reasoning?: boolean };

/**
 * `chat`: prompts and answers, each answer's work folded behind it.
 * `prompts`: only the prompts, each turn folded behind its prompt.
 * `all`: every event as it was recorded.
 */
export type ConversationMode = 'chat' | 'prompts' | 'all';
export const CONVERSATION_MODES: readonly ConversationMode[] = ['chat', 'prompts', 'all'];
export const DEFAULT_CONVERSATION_MODE: ConversationMode = 'chat';

/** A prompt the user wrote; a summary of an earlier context is not one. */
export function conversationEventIsPrompt(event: ShownEvent): boolean {
  return event.kind === 'message' && event.role === 'user' && !documentEventIsSummary(event);
}

/** Whether the event draws anything: reasoning that left no readable summary draws nothing. */
export function conversationEventIsShown(event: ShownEvent): boolean {
  return !event.empty_reasoning && !(event.kind === 'reasoning' && !event.deferred && !(event.redacted_summary ?? '').trim());
}

/** A prompt and what followed it until the next prompt. The events before the first prompt have no prompt. */
export interface ConversationTurn<Event> { prompt: Event | null; rest: Event[] }

export function conversationTurns<Event extends ShownEvent>(events: readonly Event[]): ConversationTurn<Event>[] {
  const turns: ConversationTurn<Event>[] = [];
  let current: ConversationTurn<Event> = { prompt: null, rest: [] };
  for (const event of events) {
    if (conversationEventIsPrompt(event)) {
      if (current.prompt || current.rest.length) turns.push(current);
      current = { prompt: event, rest: [] };
    } else if (conversationEventIsShown(event)) current.rest.push(event);
  }
  if (current.prompt || current.rest.length) turns.push(current);
  return turns;
}

/**
 * An answer and the work that came before it since the last answer. The
 * work at the end of what was read has no answer yet: the agent was still
 * working, or the rest is read later.
 */
export interface ConversationAnswer<Event> { work: Event[]; answer: Event | null }

export function conversationAnswers<Event extends ShownEvent>(rest: readonly Event[]): ConversationAnswer<Event>[] {
  const answers: ConversationAnswer<Event>[] = [];
  let work: Event[] = [];
  for (const event of rest) {
    if (event.kind === 'message') {
      answers.push({ work, answer: event });
      work = [];
    } else work.push(event);
  }
  if (work.length) answers.push({ work, answer: null });
  return answers;
}
