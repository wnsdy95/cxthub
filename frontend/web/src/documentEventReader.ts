import { createContext, useContext } from 'react';
import type { DocFragmentPage, DocViewEvent } from './types';

/**
 * Where a conversation's large events are read in full. Without one they are
 * read from the document by its hash. A session's conversation is read by the
 * session instead, so that the server holds every read to what the reader may
 * see; `key` then stands for the document in the cache.
 */
export interface DocumentEventReader {
  key: string;
  event: (index: number, signal?: AbortSignal) => Promise<DocViewEvent>;
  fragment: (index: number, offset: number, signal?: AbortSignal) => Promise<DocFragmentPage>;
}

export const DocumentEventReaderContext = createContext<DocumentEventReader | null>(null);

export function useDocumentEventReader() {
  return useContext(DocumentEventReaderContext);
}
