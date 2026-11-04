import { heavyTextVisiblePage, syntheticDocumentText as distinctText, foldPage } from './documentVisibleFixture';
import type { DocVisiblePage } from '../src/documentVisible';

const rawEventsPerPage = 620;
export const fragmentBytes = 4096;
export const fragmentTotalBytes = fragmentBytes * 40;

function memoryPage(from: number, pageCount: number): DocVisiblePage {
  return heavyTextVisiblePage(from, pageCount);
}

export function memoryFold(begin: number, pageCount: number): DocVisiblePage {
  const from = Math.floor(begin / rawEventsPerPage) * rawEventsPerPage;
  const body = foldPage(memoryPage(from, pageCount), (begin - from) / 31);
  body.replay_cursor = `fold-${begin}`;
  body.events[0] = { ...body.events[0], kind: 'tool_result', deferred: false,
    output: distinctText(begin + 700_000, 512, `Synthetic fold result ${begin}: `) };
  body.events[1] = { ...body.events[1], kind: 'tool_result', event_bytes: 4352 };
  body.events[2] = { ...body.events[2], kind: 'tool_result', event_bytes: fragmentTotalBytes };
  return body;
}

export function rawResult(index: number): string {
  return distinctText(index + 900_000, 4096, `Synthetic raw result ${index}: `, true);
}

export function fragmentText(index: number, offset: number): string {
  return distinctText(index + offset + 1_100_000, fragmentBytes, `Synthetic fragment ${index}/${offset}: `, true);
}
