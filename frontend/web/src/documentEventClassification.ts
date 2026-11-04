import type { DocViewEvent } from './types';

type ClassifiedEvent = DocViewEvent & { seed_summary?: boolean; empty_reasoning?: boolean };

export const documentSeedPrefixes = ['[cxt seed] Branch-switch context:', '[cxt] This session was resumed from a branch context seed.'] as const;

export function documentEventIsSummary(event: ClassifiedEvent): boolean {
  if (event.kind !== 'message') return false;
  if (event.compact_summary) return true;
  if (event.seed_summary !== undefined) return event.seed_summary;
  const text = (event.blocks ?? []).map(block => block.text).join('\n').trimStart();
  return documentSeedPrefixes.some(prefix => text.startsWith(prefix));
}

export function documentEventIsPrompt(event: ClassifiedEvent): boolean {
  return event.kind === 'message' && event.role === 'user' && !documentEventIsSummary(event);
}

export function documentEventIsRendered(event: ClassifiedEvent): boolean {
  return !event.empty_reasoning && !(event.kind === 'reasoning' && !event.deferred && !(event.redacted_summary ?? '').trim());
}
