import { createContext, useContext } from 'react';
import type { DocFragmentPage, DocViewEvent } from './types';

/**
 * Where a conversation's large events are read in full. A session's
 * conversation is read by the session, so that the server holds every read to
 * what the reader may see; `key` stands for the document in the cache.
 */
export interface DocumentEventReader {
  key: string;
  event: (index: number, signal?: AbortSignal) => Promise<DocViewEvent>;
  fragment: (index: number, offset: number, signal?: AbortSignal) => Promise<DocFragmentPage>;
}

export const DocumentEventReaderContext = createContext<DocumentEventReader | null>(null);

/** The reader of the conversation being shown. A conversation is only ever rendered inside one. */
export function useDocumentEventReader(): DocumentEventReader {
  const reader = useContext(DocumentEventReaderContext);
  if (!reader) throw new Error('A conversation is rendered outside its reader.');
  return reader;
}
