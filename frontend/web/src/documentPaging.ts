import type { DocFragmentPage, DocViewEvent } from './types';
import { DOCUMENT_FRAGMENT_BYTES } from './documentReads';

export const DOCUMENT_VISIBLE_MESSAGES = 20;

export function validateDocFragment(page: DocFragmentPage, index: number, offset: number) {
  if (typeof page.json_fragment !== 'string' || ![page.event_index, page.byte_offset, page.next, page.total_bytes].every(Number.isSafeInteger)) throw new Error('Invalid document fragment');
  const size = new TextEncoder().encode(page.json_fragment).byteLength;
  const end = offset + size;
  if (page.event_index !== index || page.byte_offset !== offset || !size || size > DOCUMENT_FRAGMENT_BYTES || page.total_bytes < end ||
      (page.next === -1 ? end !== page.total_bytes : page.next !== end || page.next >= page.total_bytes)) {
    throw new Error('Invalid document fragment range');
  }
  return page;
}

/** A page of a session's conversation as one reader may read it. `next` is -1 at the end of what is open from here. */
export interface AnchoredEventPage { events: DocViewEvent[]; offset: number; next: number }

export function validateAnchoredEventPage(page: AnchoredEventPage, requested: number, limit: number): AnchoredEventPage {
  if (!Array.isArray(page.events) || page.events.length > limit || !Number.isSafeInteger(page.offset) || !Number.isSafeInteger(page.next) ||
      page.offset !== requested || (page.next !== -1 && (page.events.length === 0 || page.next !== page.offset + page.events.length))) {
    throw new Error('Invalid session conversation page');
  }
  const events = page.events.map((event, index) => {
    const absolute = page.offset + index;
    if (event.event_index !== undefined && event.event_index !== absolute) throw new Error('Invalid session conversation event index');
    return { ...event, event_index: absolute };
  });
  return { ...page, events };
}
