import type { SearchHit } from './types';

export function searchEventPosition(hit: SearchHit): number | undefined {
  return hit.kind === 'event' && Number.isSafeInteger(hit.event_index) && hit.event_index! >= 0
    ? hit.event_index : undefined;
}
