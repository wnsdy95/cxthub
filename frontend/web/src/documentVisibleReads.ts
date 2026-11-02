import { api, ApiError } from './api';
import { documentStreamCheckpoint, validateLegacyDocument, validateVisibleFold, validateVisibleUnits, visibleContextWitness, visiblePageCheckpoint, visibleParameters,
  type DocumentSelection, type DocumentStreamCheckpoint, type DocumentStreamPage, type DocVisibleCheckpoint, type DocVisibleContext, type DocVisibleFold, type DocVisiblePage } from './documentVisible';

export function readDocumentStream(selection: DocumentSelection, signal: AbortSignal, previous?: DocumentStreamPage | DocumentStreamCheckpoint, usedCursors: Iterable<string> = []): Promise<DocumentStreamPage> {
  return readDocumentStreamCheckpoint(selection, signal, previous ? documentStreamCheckpoint(previous) : undefined, usedCursors);
}

async function readDocumentStreamCheckpoint(selection: DocumentSelection, signal: AbortSignal, previous: DocumentStreamCheckpoint | undefined, usedCursors: Iterable<string>): Promise<DocumentStreamPage> {
  if (previous?.kind === 'legacy') {
    const page = await api.getDocViewEvents(selection.repoId, selection.hash, undefined, previous.next, selection.mode, signal);
    return validateLegacyDocument(page, selection, previous.next, previous);
  }
  let value: unknown;
  try {
    value = await api.getDocVisibleEvents(selection.repoId, selection.hash,
      visibleParameters(selection, previous ? { kind: 'units', cursor: previous.next_cursor! } : undefined), signal);
  } catch (error) {
    if (previous || !(error instanceof ApiError) || error.status !== 409 || error.code !== 'doc_visible_projection_unsupported') throw error;
    return validateLegacyDocument(await api.getDocViewEvents(selection.repoId, selection.hash, selection.base, selection.start, selection.mode, signal), selection, selection.start);
  }
  if (!previous && value && typeof value === 'object' && !('projection' in value)) {
    return validateLegacyDocument(value, selection, selection.start);
  }
  return validateVisibleUnits(value, selection, previous, usedCursors);
}

export function readDocumentFold(selection: DocumentSelection, signal: AbortSignal, context: DocVisibleContext, fold: DocVisibleFold,
  previous?: DocVisiblePage | DocVisibleCheckpoint, renderedBefore = 0, usedCursors: Iterable<string> = []): Promise<DocVisiblePage> {
  const { from, to, rows, cursor } = fold;
  return readDocumentFoldCheckpoint(selection, signal, visibleContextWitness(context), { from, to, rows, cursor },
    previous ? visiblePageCheckpoint(previous) : undefined, renderedBefore, usedCursors);
}

async function readDocumentFoldCheckpoint(selection: DocumentSelection, signal: AbortSignal, context: DocVisibleContext, fold: DocVisibleFold,
  previous: DocVisibleCheckpoint | undefined, renderedBefore: number, usedCursors: Iterable<string>): Promise<DocVisiblePage> {
  const cursor = previous?.next_cursor ?? fold.cursor;
  const value = await api.getDocVisibleEvents(selection.repoId, selection.hash, visibleParameters(selection, { kind: 'fold', cursor }), signal);
  return validateVisibleFold(value, selection, context, fold, previous, renderedBefore, requestedCursors(cursor, usedCursors));
}

function* requestedCursors(cursor: string, history: Iterable<string>): IterableIterator<string> {
  yield cursor;
  yield* history;
}
