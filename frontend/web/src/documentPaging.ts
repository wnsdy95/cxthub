import type { DocViewEventPage } from './api';
import type { DocFragmentPage, DocViewMode } from './types';
import { DOCUMENT_FRAGMENT_BYTES } from './documentReads';

export const DOCUMENT_VISIBLE_MESSAGES = 20;
export const DOCUMENT_TRAVERSED_EVENTS = 512;

export function docPageKey(repoId: string | null, hash: string | null, base: string | undefined, start: number, mode: DocViewMode) {
  return ['doc-events', repoId, hash, base ?? '', start, mode] as const;
}

export function validateDocViewPage(page: DocViewEventPage, hash: string, requested: number): DocViewEventPage {
  if (!Array.isArray(page.events)) throw new Error('Invalid document events');
  const end = page.offset + page.events.length;
  if (page.hash !== hash || page.events.length > DOCUMENT_TRAVERSED_EVENTS ||
      ![page.offset, page.total, page.inherited, page.next].every(Number.isSafeInteger) ||
      page.offset < 0 || page.total < end || page.inherited < 0 || page.inherited > page.total ||
      (requested >= 0 && page.offset !== requested) ||
      (page.next === -1 ? end !== page.total : page.next !== end || page.next <= page.offset || page.next >= page.total)) {
    throw new Error('Invalid document page range');
  }
  const events = page.events.map((event, index) => {
    const absolute = page.offset + index;
    if (event.event_index !== undefined && event.event_index !== absolute) throw new Error('Invalid document event index');
    return { ...event, event_index: absolute };
  });
  return { ...page, events };
}

export function validateDocEventDetail(page: DocViewEventPage, hash: string, index: number) {
  if (page.hash !== hash || page.offset !== index || page.events?.length !== 1 ||
      (page.events[0].event_index !== undefined && page.events[0].event_index !== index)) throw new Error('Invalid document event detail');
  return page.events[0];
}

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
