export const DOCUMENT_READ_CONCURRENCY = 2;
export const DOCUMENT_PAGE_BYTES = 1024 * 1024;
export const DOCUMENT_DETAIL_BYTES = 1024 * 1024;
export const DOCUMENT_FRAGMENT_RESPONSE_BYTES = 512 * 1024;
export const DOCUMENT_RAW_EVENT_BYTES = 128 * 1024;
export const DOCUMENT_FRAGMENT_BYTES = 64 * 1024;

export class DocumentReadLimitError extends Error {}

type WaitingRead = { start: () => void; signal?: AbortSignal; abort: () => void };
const waiting: WaitingRead[] = [];
let active = 0;

export function scheduleDocumentRead<T>(read: () => Promise<T>, signal?: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const entry: WaitingRead = {
      signal,
      abort: () => {
        const index = waiting.indexOf(entry);
        if (index >= 0) waiting.splice(index, 1);
        reject(signal?.reason ?? new DOMException('Aborted', 'AbortError'));
      },
      start: () => {
        signal?.removeEventListener('abort', entry.abort);
        active++;
        Promise.resolve().then(read).then(resolve, reject).finally(() => {
          active--;
          drain();
        });
      },
    };
    if (signal?.aborted) { entry.abort(); return; }
    signal?.addEventListener('abort', entry.abort, { once: true });
    waiting.push(entry);
    drain();
  });
}

function drain() {
  while (active < DOCUMENT_READ_CONCURRENCY && waiting.length) {
    const entry = waiting.shift()!;
    if (entry.signal?.aborted) entry.abort();
    else entry.start();
  }
}

export async function readDocumentResponse(response: Response, maxBytes: number): Promise<string> {
  const reader = response.body?.getReader();
  if (!reader) return '';
  const decoder = new TextDecoder();
  const parts: string[] = [];
  let size = 0;
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      size += chunk.value.byteLength;
      if (size > maxBytes) throw new DocumentReadLimitError('Document response exceeds the browser read limit.');
      parts.push(decoder.decode(chunk.value, { stream: true }));
    }
    parts.push(decoder.decode());
    return parts.join('');
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    reader.releaseLock();
  }
}
