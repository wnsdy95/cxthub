import type { CIREvent, DocViewEvent, DocViewMode } from '../types';
import { useT } from '../i18n';
import { Markdown } from './Markdown';
import { useDocEvent, useDocEventFragment } from '../hooks';
import { ApiError } from '../api';
import { DOCUMENT_RAW_EVENT_BYTES, DocumentReadLimitError } from '../documentReads';
import { DocumentDisclosure, DocumentEventIdentity, DocumentScrollText, useDocumentPresentation, useDocumentDisclosureState } from './DocumentPresentation';
import { documentEventIsSummary, documentSeedPrefixes } from '../documentEventClassification';

type DisplayEvent = DocViewEvent & { seed_summary?: boolean; empty_reasoning?: boolean };

export type ViewMode = DocViewMode;

export function EventStream({ events, inherited = 0, repoId = '', hash = '' }: {
  events: DisplayEvent[]; inherited?: number; repoId?: string; hash?: string;
}) {
  return events.map(event => <DocumentEventRow key={event.event_index} event={event} index={event.event_index!}
    inherited={inherited} repoId={repoId} hash={hash} />);
}

type DeferredEventProps = { event: DisplayEvent; repoId: string; hash: string; index: number };

export function DocumentEventRow({ event, index, inherited, repoId, hash, revealed = false, targeted = false, navigationTarget = targeted }: {
  event: DisplayEvent; index: number; inherited: number; repoId: string; hash: string; revealed?: boolean; targeted?: boolean; navigationTarget?: boolean;
}) {
  if (event.empty_reasoning) return null;
  return <DocumentEventIdentity index={index} targeted={targeted}><div data-event-index={index} data-document-target={navigationTarget ? index : undefined} className={index < inherited ? 'doc-inherited' : undefined}>
    {event.deferred && event.kind !== 'compaction'
      ? <DeferredEventRow event={event} index={index} repoId={repoId} hash={hash} revealed={revealed} />
      : <EventRow ev={event} />}
  </div></DocumentEventIdentity>;
}

function DeferredEventRow({ event, repoId, hash, index, revealed }: DeferredEventProps & { revealed: boolean }) {
  const translate = useT();
  const [expanded, setExpanded] = useDocumentDisclosureState('deferred');
  if (revealed && event.kind === 'message' && !event.compact_summary) {
    return <DeferredEventContent event={event} repoId={repoId} hash={hash} index={index} />;
  }
  const label = event.seed_summary ? translate('context.cxtSeedSummary') : event.compact_summary ? translate('context.compactSummary')
    : event.provider_tool_name || event.tool_name || event.role || event.kind;
  return <div className={`msg ${event.role ?? 'tool'}`}>
    <span className="msg-role">{event.role ?? event.kind}</span>
    <div className="msg-body">
      <details open={expanded} className="tool-more" data-deferred-event={index} onClick={click => click.stopPropagation()}
        onToggle={toggle => setExpanded(toggle.currentTarget.open)}>
        <summary>{translate('common.expand')} — {label}{event.event_bytes ? ` (${event.event_bytes.toLocaleString()} B)` : ''}</summary>
        {expanded && <DeferredEventContent event={event} repoId={repoId} hash={hash} index={index} />}
      </details>
    </div>
  </div>;
}

function DeferredEventContent(props: DeferredEventProps) {
  return (props.event.event_bytes ?? 0) > DOCUMENT_RAW_EVENT_BYTES
    ? <FragmentEventContent {...props} />
    : <RawEventContent {...props} />;
}

function RawEventContent(props: DeferredEventProps) {
  const translate = useT();
  const query = useDocEvent(props.repoId, props.hash, props.index);
  if (query.data?.deferred || query.error instanceof DocumentReadLimitError || (query.error instanceof ApiError && query.error.status === 413)) {
    return <FragmentEventContent {...props} />;
  }
  if (query.isError) return <div role="alert" className="err">{query.error.message} <button onClick={() => void query.refetch()}>{translate('context.retryRead')}</button></div>;
  return query.data ? <EventRow ev={{ ...query.data, seed_summary: props.event.seed_summary, empty_reasoning: props.event.empty_reasoning }} />
    : <span role="status">{translate('context.loadingEvents')}</span>;
}

function FragmentEventContent({ repoId, hash, index }: DeferredEventProps) {
  const translate = useT();
  const [navigation, setNavigation] = useDocumentPresentation('fragment', { offsets: [0], position: 0 });
  const offset = navigation.offsets[navigation.position];
  const query = useDocEventFragment(repoId, hash, index, offset);
  const fragment = query.data;
  return <div className="event-fragment" data-fragment-offset={offset}>
    {fragment && <>
      <p role="status">{translate('context.eventFragment', { start: fragment.byte_offset, end: fragment.next < 0 ? fragment.total_bytes : fragment.next, total: fragment.total_bytes })}</p>
      <DocumentScrollText name={`fragment-scroll:${offset}`} style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', maxHeight: '60vh', overflow: 'auto' }}>{fragment.json_fragment}</DocumentScrollText>
    </>}
    {query.isFetching && <span role="status">{translate('context.loadingEvents')}</span>}
    {query.isError && <div role="alert" className="err">{query.error.message} <button onClick={() => void query.refetch()}>{translate('context.retryRead')}</button></div>}
    <button disabled={navigation.position === 0 || query.isFetching} onClick={() => setNavigation(previous => ({ ...previous, position: previous.position - 1 }))}>{translate('context.previousFragment')}</button>
    <button disabled={!fragment || fragment.next < 0 || query.isFetching || query.isError} onClick={() => {
      if (fragment && fragment.next >= 0) setNavigation(previous => ({ offsets: [...previous.offsets.slice(0, previous.position + 1), fragment.next], position: previous.position + 1 }));
    }}>{translate('context.nextFragment')}</button>
  </div>;
}

// Event line: role label + block (text is body), tool_call is tool-specific details
// (Edit=diff, Write=file content, Bash=command), tool_result/reasoning is collapsible.
// In CIR, the original tool input/output is preserved, so rendering is handled (truncation is for display — data is complete).
function EventRow({ ev }: { ev: DisplayEvent }) {
  const t = useT();
  if (ev.kind === 'tool_call') return <ToolCallRow ev={ev} />;
  if (ev.kind === 'tool_result') return <ToolResultRow ev={ev} />;
  if (ev.kind === 'reasoning') return <ReasoningRow ev={ev} />;
  if (ev.kind === 'compaction') return <div className="compaction-divider">{t('context.compactionDivider')}</div>;
  if (documentEventIsSummary(ev)) return <CompactSummaryRow ev={ev} />;
  if (ev.kind !== 'message' || !ev.blocks?.length) {
    return <div className="msg meta">[{ev.kind}]</div>;
  }
  const roleLabel = ev.agent_message && ev.agent_author ? ev.agent_author : ev.role;
  return (
    <div className={`msg ${ev.role ?? ''}`}>
      <span className="msg-role">{roleLabel}</span>
      <div className="msg-body">
        {ev.blocks.map((b, i) =>
          b.type === 'text' ? (
            // Conversation body (user prompt·assistant answer) is also marked down in the same way as compressed memory
            // rendering — agent answers are usually in titles/lists/codefences, so plain text <p> can degrade readability.
            // Markdown is safe for React element creation (no raw HTML).
            <Markdown key={i} text={b.text ?? ''} />
          ) : (
            <p key={i} className="msg-block">
              [{b.type}
              {b.name ? `: ${b.name}` : ''}]
            </p>
          ),
        )}
      </div>
    </div>
  );
}

const MAX_DIFF_LINES = 40; // display limit; the complete source remains preserved in CIR
const str = (v: unknown): string => (typeof v === 'string' ? v : '');

// mdSlice — truncates text to render in markdown. If the truncation point is inside ``` fences,
// an unfinished fence would encompass the entire following content in <pre>, so it closes odd-numbered fences to balance.
function mdSlice(text: string, n: number): string {
  const cut = text.slice(0, n);
  const fences = (cut.match(/^\s*```/gm) ?? []).length;
  return fences % 2 === 1 ? cut + '\n```' : cut;
}

// File relative path (absolute path outside repo root is last segments only).
function shortPath(p: string): string {
  const seg = p.split('/');
  return seg.length > 4 ? '…/' + seg.slice(-3).join('/') : p;
}

// Diff line render (±prefix). kind: 'del' | 'add'
function DiffLines({ text, sign }: { text: string; sign: 'add' | 'del' }) {
  const t = useT();
  const lines = text.split('\n');
  const shown = lines.slice(0, MAX_DIFF_LINES);
  return (
    <>
      {shown.map((l, i) => (
        <div key={i} className={`diff-line ${sign}`}>
          {sign === 'add' ? '+' : '−'} {l}
        </div>
      ))}
      {lines.length > MAX_DIFF_LINES && <div className="diff-line more">… {t('context.moreLines', { count: lines.length - MAX_DIFF_LINES })}</div>}
    </>
  );
}

// ToolCallRow — Tool header + body. Edit/Write shows code modification logs as diff.
function ToolCallRow({ ev }: { ev: CIREvent }) {
  const t = useT();
  const name = ev.provider_tool_name || ev.tool_name || 'tool';
  const input = ev.input ?? {};
  const filePath = str(input.file_path);
  const headArg = filePath ? shortPath(filePath) : str(input.command) || str(input.pattern) || str(input.url) || '';

  let body: JSX.Element | null = null;
  if (name === 'Edit' && (input.old_string || input.new_string)) {
    body = (
      <div className="tool-diff">
        {str(input.old_string) && <DiffLines text={str(input.old_string)} sign="del" />}
        {str(input.new_string) && <DiffLines text={str(input.new_string)} sign="add" />}
      </div>
    );
  } else if (name === 'Write' && input.content) {
    body = (
      <div className="tool-diff">
        <DiffLines text={str(input.content)} sign="add" />
      </div>
    );
  } else if (name === 'Bash' && input.command) {
    body = <pre className="tool-cmd">$ {str(input.command)}</pre>;
  } else if (Object.keys(input).length > 0) {
    body = (
      <DocumentDisclosure name="input" className="tool-more">
        <summary>{t('context.viewInput')}</summary>
        <pre>{JSON.stringify(input, null, 2).slice(0, 4000)}</pre>
      </DocumentDisclosure>
    );
  }

  return (
    <div className="msg tool">
      <span className="msg-role">tool</span>
      <div className="msg-body">
        <p className="tool-head">
          <strong>{name}</strong>
          {headArg && <code>({headArg})</code>}
        </p>
        {body}
      </div>
    </div>
  );
}

// ToolResultRow — Result is collapsible (first line preview).
function ToolResultRow({ ev }: { ev: CIREvent }) {
  const out = typeof ev.output === 'string' ? ev.output : ev.output != null ? JSON.stringify(ev.output, null, 2) : '';
  if (!out.trim()) return <div className="msg meta">[tool_result]</div>;
  const firstLine = out.trimStart().split('\n')[0].slice(0, 100);
  return (
    <div className="msg tool">
      <span className="msg-role" />
      <div className="msg-body">
        <DocumentDisclosure name="result" className="tool-more">
          <summary>
            ↳ <code>{firstLine}</code>
            {out.length > firstLine.length && ' …'}
          </summary>
          <pre>{out.slice(0, 8000)}</pre>
        </DocumentDisclosure>
      </div>
    </div>
  );
}

// ReasoningRow — Plain text summary only (locked original text is not shown). Summary-less codex
// encrypted reasoning is shown as 0 — empty "[reasoning]" line is noise and omitted.
function ReasoningRow({ ev }: { ev: CIREvent }) {
  const summary = ev.redacted_summary ?? '';
  if (!summary.trim()) return null;
  return (
    <div className="msg meta">
      <span className="msg-role" />
      <div className="msg-body">
        <DocumentDisclosure name="reasoning" className="tool-more reasoning">
          <summary>reasoning — {summary.split('\n')[0].slice(0, 80)}…</summary>
          {/* Reasoning summary also markdown text — rendered like chat body */}
          <Markdown text={mdSlice(summary, 8000)} />
        </DocumentDisclosure>
      </div>
    </div>
  );
}

// CompactSummaryRow — Summary generated by agent context compression. It can resemble a long user message, so a collapsible block distinguishes it (audit finding #13).
// cxt-synthesized seed digests carry the same CompactSummary marking but are not agent-written — they get their own label (#38).
function CompactSummaryRow({ ev }: { ev: DisplayEvent }) {
  const t = useT();
  const text = (ev.blocks ?? []).map((b) => b.text).join('\n');
  const isSeed = ev.seed_summary ?? documentSeedPrefixes.some(prefix => text.trimStart().startsWith(prefix));
  return (
    <div className="msg meta compact-summary">
      <span className="msg-role" />
      <div className="msg-body">
        <DocumentDisclosure name="summary" className="tool-more">
          <summary>
            ◈ {t(isSeed ? 'context.cxtSeedSummary' : 'context.compactSummary')} — <code>{text.slice(0, 72)}…</code>
          </summary>
          <Markdown text={mdSlice(text, 16000)} />
        </DocumentDisclosure>
      </div>
    </div>
  );
}
