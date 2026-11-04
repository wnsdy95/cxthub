import { createContext, useCallback, useContext, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery, type QueryKey } from '@tanstack/react-query';
import { api, ApiError } from '../api';
import { type DocumentFindSource } from '../documentFind';
import { useDocumentFind, DocumentFindReadError, type DocumentFindResult } from '../useDocumentFind';
import { documentReadRetry } from '../readRetry';
import { DocumentProjectionError, validateLegacyDocument, validateVisibleUnits, visibleParameters } from '../documentVisible';
import { DocumentReadScope } from '../documentReadScope';
import { useT } from '../i18n';
import { DocumentEventRow } from './EventStream';
import { DocumentPresentation, DocumentSearchTarget } from './DocumentPresentation';
import { documentScrollRoot } from './DocumentPageWindow';
import { DocumentFindFragment } from './DocumentFindFragment';

type FindRegistration = { id: string; source?: DocumentFindSource; scope: QueryKey; onRestart: () => void };
type FindRegistry = { register: (registration: FindRegistration) => () => void };
const FindRegistry = createContext<FindRegistry | null>(null);

export function DocumentFindScope({ children }: { children: ReactNode }) {
  const parent = useContext(FindRegistry);
  return parent ? <>{children}</> : <DocumentFindRoot>{children}</DocumentFindRoot>;
}

function DocumentFindRoot({ children }: { children: ReactNode }) {
  const [registrations, setRegistrations] = useState<ReadonlyMap<string, FindRegistration>>(new Map());
  const register = useCallback((registration: FindRegistration) => {
    setRegistrations(previous => new Map(previous).set(registration.id, registration));
    return () => setRegistrations(previous => {
      if (previous.get(registration.id) !== registration) return previous;
      const next = new Map(previous);
      next.delete(registration.id);
      return next;
    });
  }, []);
  const registry = useMemo(() => ({ register }), [register]);
  return <FindRegistry.Provider value={registry}>
    <DocumentFindToolbar registrations={registrations} />
    {children}
  </FindRegistry.Provider>;
}

export function useDocumentFindRegistration(registration: FindRegistration | undefined) {
  const registry = useContext(FindRegistry);
  useEffect(() => registration && registry?.register(registration), [registry, registration]);
}

function DocumentFindToolbar({ registrations }: { registrations: ReadonlyMap<string, FindRegistration> }) {
  const translate = useT();
  const identity = useId();
  const input = useRef<HTMLInputElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const container = useRef<HTMLDivElement>(null);
  const composing = useRef(false);
  const [open, setOpen] = useState(false);
  const [text, setText] = useState('');
  const [reveal, setReveal] = useState(false);
  const [representation, setRepresentation] = useState<'source' | 'display'>('source');
  const sources = useMemo(() => [...registrations.values()].flatMap(value => value.source ? [{ ...value.source,
    representation: representation === 'display' ? 'display-prepared-v1' as const : undefined }] : []), [registrations, representation]);
  const unsupported = registrations.size !== sources.length;
  const find = useDocumentFind(sources, identity);
  const result = find.result;
  const selectedScope = result && registrations.get(result.source.id)?.scope;
  const failure = find.query.error;
  const cause = failure instanceof DocumentFindReadError ? failure.cause : failure;
  const failedSource = failure instanceof DocumentFindReadError ? registrations.get(failure.sourceId) : undefined;
  const stale = cause instanceof DocumentProjectionError || cause instanceof ApiError && cause.status === 409;
  const validInput = [...text].length > 0 && [...text].length <= 256;
  const close = () => { setOpen(false); setReveal(false); find.close(); trigger.current?.focus({ preventScroll: true }); };
  useEffect(() => { if (open) input.current?.focus({ preventScroll: true }); }, [open]);
  useLayoutEffect(() => {
    const node = container.current;
    if (!node) return;
    const root = documentScrollRoot(node);
    const header = node.closest('.viewer')?.querySelector<HTMLElement>('.viewer-head');
    const measure = () => {
      const headerStyle = header && getComputedStyle(header);
      const top = header && headerStyle?.position === 'sticky'
        ? header.getBoundingClientRect().height + (parseFloat(headerStyle.top) || 0) : 0;
      if (node.style.getPropertyValue('--document-find-top') !== `${top}px`) node.style.setProperty('--document-find-top', `${top}px`);
      if (!open) return;
      const bottom = Math.min(window.innerHeight, root?.getBoundingClientRect().bottom ?? window.innerHeight);
      node.style.setProperty('--document-find-height', `${Math.max(40, bottom - node.getBoundingClientRect().bottom - 12)}px`);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    if (header) observer.observe(header);
    if (root) observer.observe(root);
    const scroller = root ?? window;
    if (open) scroller.addEventListener('scroll', measure, { passive: true });
    window.addEventListener('resize', measure, { passive: true });
    return () => {
      observer.disconnect();
      if (open) scroller.removeEventListener('scroll', measure);
      window.removeEventListener('resize', measure);
    };
  }, [open]);
  const status = find.invalid ? translate('documentFind.changed') : unsupported ? translate('documentFind.unsupported')
    : find.query.isFetching ? translate('documentFind.scanning') : result ? translate('documentFind.match', { number: find.number })
    : find.started && !find.query.isError ? translate('documentFind.noMatch') : translate(representation === 'source' ? 'documentFind.scope' : 'documentFind.displayScope');
  return <div ref={container} className="document-find" data-document-find data-document-sticky>
    <button ref={trigger} className="dl-btn" aria-expanded={open} aria-controls={`${identity}-panel`} onClick={() => {
      if (open) close(); else setOpen(true);
    }}>{translate('documentFind.title')}</button>
    {open && <section id={`${identity}-panel`} className="document-find-panel" aria-label={translate('documentFind.title')}
      onKeyDown={event => { if (event.key === 'Escape') { event.stopPropagation(); close(); } }}>
      <form role="search" onSubmit={event => {
        event.preventDefault();
        if (composing.current || !validInput || unsupported || !sources.length) return;
        setReveal(false);
        find.start(text);
      }}>
        <label htmlFor={`${identity}-input`}>{translate('documentFind.query')}</label>
        <div className="document-find-controls">
          <select aria-label={translate('documentFind.representation')} value={representation} onChange={event => {
            setRepresentation(event.target.value === 'display' ? 'display' : 'source');
            setReveal(false);
            find.close();
          }}>
            <option value="source">{translate('documentFind.sourceText')}</option>
            <option value="display">{translate('documentFind.displayText')}</option>
          </select>
          <input id={`${identity}-input`} ref={input} value={text} aria-describedby={`${identity}-scope`}
            onCompositionStart={() => { composing.current = true; }} onCompositionEnd={() => { composing.current = false; }}
            onChange={event => { setText(event.target.value); setReveal(false); find.close(); }} onKeyDown={event => {
              if (event.key !== 'Enter' || text !== find.text || !find.started || !result || event.nativeEvent.isComposing) return;
              event.preventDefault();
              setReveal(false);
              if (event.shiftKey) find.previous(); else find.next();
            }} />
          <button type="submit" disabled={!validInput || !sources.length || unsupported}>{translate('documentFind.search')}</button>
          <button type="button" disabled={!find.canPrevious || find.query.isFetching} onClick={() => { setReveal(false); find.previous(); }} aria-label={translate('documentFind.previous')}>↑</button>
          <button type="button" disabled={!find.canNext || find.query.isFetching} onClick={() => { setReveal(false); find.next(); }} aria-label={translate('documentFind.next')}>↓</button>
          <button type="button" onClick={close}>{translate('common.close')}</button>
        </div>
      </form>
      <p id={`${identity}-scope`} className="muted">{translate(representation === 'source' ? 'documentFind.scope' : 'documentFind.displayScope')}</p>
      {find.range && <p className="muted">{translate('documentFind.range', find.range)}{find.expanded && <> {translate('documentFind.newContent')}</>}</p>}
      <p role="status">{status}</p>
      {find.query.isError && !find.invalid && <p role="alert" className="err">{find.query.error.message}{' '}
        <button onClick={() => {
          if (stale && failedSource) failedSource.onRestart(); else void find.query.refetch();
        }}>{translate(stale && failedSource ? 'context.restartDocument' : 'context.retryRead')}</button>
      </p>}
      {result && !find.invalid && <div className="document-find-result">
        <p><code>{result.source.selection.hash.slice(0, 20)}</code> · {translate('documentFind.event', { index: result.match.event_index })}
          {result.match.raw && <> · {translate('documentFind.raw')}</>}</p>
        <DocumentFindPreview result={result} />
        <button aria-expanded={reveal} onClick={() => setReveal(value => !value)}>{translate('documentFind.reveal')}</button>
        {reveal && selectedScope && <DocumentReadScope.Provider value={selectedScope}>
          <DocumentFindReveal key={`${result.source.id}:${result.source.representation ?? ''}:${result.match.event_index}:${result.match.field}:${result.match.byte_offset}:${result.match.reveal_cursor}`} result={result}
            onRestart={registrations.get(result.source.id)!.onRestart} />
        </DocumentReadScope.Provider>}
      </div>}
    </section>}
  </div>;
}

function DocumentFindPreview({ result }: { result: DocumentFindResult }) {
  const match = result.match;
  const raw = new TextEncoder().encode(match.preview);
  const begin = match.byte_offset - match.preview_byte_offset;
  const decoder = new TextDecoder('utf-8', { fatal: true });
  return <pre className="document-find-preview">{decoder.decode(raw.slice(0, begin))}<mark>
    {decoder.decode(raw.slice(begin, begin + match.byte_length))}</mark>{decoder.decode(raw.slice(begin + match.byte_length))}</pre>;
}

function DocumentFindReveal({ result, onRestart }: { result: DocumentFindResult; onRestart: () => void }) {
  return result.match.source_range ? <DocumentFindFragment result={result} onRestart={onRestart} />
    : <DocumentFindEventReveal result={result} onRestart={onRestart} />;
}

function DocumentFindEventReveal({ result, onRestart }: { result: DocumentFindResult; onRestart: () => void }) {
  const translate = useT();
  const identity = useId();
  const { source, match } = result;
  const selection = { ...source.selection, mode: 'all' as const, start: match.event_index };
  const query = useQuery({
    queryKey: ['document-find-reveal', identity, source.id, match.event_index,
      source.protocol === 'legacy' ? source.context.hash : source.context.generation, match.reveal_cursor],
    queryFn: async ({ signal }) => {
      if (source.protocol === 'legacy') {
        const value = await api.getDocFindLegacyEvent(selection.repoId, selection.hash, match.event_index, signal);
        const page = validateLegacyDocument(value, { ...selection, base: undefined }, match.event_index);
        if (page.doc_identity !== '' || page.total !== source.context.total || page.inherited !== 0 || page.events.length !== 1 ||
          page.events[0].event_index !== match.event_index || page.next !== (match.event_index + 1 === page.total ? -1 : match.event_index + 1)) {
          throw new DocumentProjectionError();
        }
        return page.events[0];
      }
      const value = await api.getDocVisibleEvents(selection.repoId, selection.hash,
        visibleParameters(selection, { kind: 'units', cursor: match.reveal_cursor }), signal);
      const page = validateVisibleUnits(value, selection);
      if (page.generation !== source.context.generation || page.total !== source.context.total || page.inherited !== source.context.inherited ||
        page.from !== match.event_index || page.to !== match.event_index + 1 || page.units.length !== 1 || page.units[0].event?.event_index !== match.event_index) {
        throw new DocumentProjectionError();
      }
      return page.units[0].event!;
    },
    gcTime: 0,
    staleTime: 0,
    structuralSharing: false,
    ...documentReadRetry,
    retryOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  const event = query.isFetching || query.isError ? undefined : query.data;
  const stale = query.error instanceof DocumentProjectionError || query.error instanceof ApiError && query.error.status === 409;
  return <DocumentPresentation><DocumentSearchTarget requestId={`${source.selection.hash}:${match.event_index}:${match.reveal_cursor}`}>
    <div className="document-find-reveal" aria-busy={query.isFetching}>
      {query.isFetching && <p role="status">{translate('context.loadingEvents')}</p>}
      {query.isError && <p role="alert" className="err">{query.error.message}{' '}
        <button onClick={stale ? onRestart : () => { void query.refetch(); }}>{translate(stale ? 'context.restartDocument' : 'context.retryRead')}</button>
      </p>}
      {event && !query.isError && <DocumentEventRow event={event} index={match.event_index} inherited={source.context.inherited}
        repoId={selection.repoId} hash={selection.hash} targeted navigationTarget={false} />}
    </div>
  </DocumentSearchTarget></DocumentPresentation>;
}
