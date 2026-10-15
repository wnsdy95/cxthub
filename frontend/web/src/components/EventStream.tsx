import { useState } from 'react';
import type { CIREvent, DocViewEvent, DocViewMode } from '../types';
import { useT } from '../i18n';
import { Markdown } from './Markdown';
import { useDocEvent, useDocEventFragment } from '../hooks';
import { ApiError } from '../api';
import { DOCUMENT_RAW_EVENT_BYTES, DocumentReadLimitError } from '../documentReads';

const CXT_SEED_PREFIXES = ['[cxt seed] Branch-switch context:', '[cxt] This session was resumed from a branch context seed.'];

function isCompactSummaryEvent(ev: CIREvent): boolean {
  if (ev.kind !== 'message') return false;
  if (ev.compact_summary) return true;
  const text = (ev.blocks ?? []).map((b) => b.text).join('\n').trimStart();
  return CXT_SEED_PREFIXES.some((prefix) => text.startsWith(prefix));
}

// Prompt-only view hides assistant, tool, and reasoning events while retaining user prompts.
// Clicking a prompt expands every AI event in that turn until the next prompt; clicking again
// collapses it. Expansion state resets for each snapshot key.
function isPrompt(ev: CIREvent): boolean {
  // Distillation is for role=user only — not a human prompt — do not use as turn head.
  return ev.kind === 'message' && ev.role === 'user' && !isCompactSummaryEvent(ev);
}

// chatGroups — group turn body into "hidden work (pre: tools·reasoning) + result message" units.
// In prompt+response mode, append ▸ AI N (pre count) to response lines to expand response units.
function chatGroups(body: { ev: CIREvent; idx: number }[]): { pre: { ev: CIREvent; idx: number }[]; msg: { ev: CIREvent; idx: number } | null }[] {
  const out: { pre: { ev: CIREvent; idx: number }[]; msg: { ev: CIREvent; idx: number } | null }[] = [];
  let pre: { ev: CIREvent; idx: number }[] = [];
  for (const item of body) {
    if (item.ev.kind === 'message') {
      out.push({ pre, msg: item });
      pre = [];
    } else {
      pre.push(item);
    }
  }
  if (pre.length > 0) out.push({ pre, msg: null });
  return out;
}

// Non-rendered events (encryption reasoning without summary — ReasoningRow returns null) are excluded from turn body — the number of rows shown when expanded should match the "AI N" count.
function isRendered(ev: DocViewEvent): boolean {
  return !(ev.kind === 'reasoning' && !ev.deferred && !(ev.redacted_summary ?? '').trim());
}

// View mode: all = all events / prompts = prompts only (fold turns) / chat = prompts+assistant messages only (hide tools·results·reasoning — read conversation flow only).
export type ViewMode = DocViewMode;

export function EventStream({ events, offset = 0, inherited = 0, mode, repoId = '', hash = '' }: {
  events: DocViewEvent[]; offset?: number; inherited?: number; mode: ViewMode; repoId?: string; hash?: string;
}) {
  const tr = useT();
  const [open, setOpen] = useState<Set<string>>(new Set());
  const row = (event: DocViewEvent, index: number, revealed = false) => (
    <div key={index} data-event-index={index} className={index < inherited ? 'doc-inherited' : undefined}>
      {event.deferred && event.kind !== 'compaction'
        ? <DeferredEventRow event={event} index={index} repoId={repoId} hash={hash} revealed={revealed} />
        : <EventRow ev={event} />}
    </div>
  );
  if (mode === 'all') {
    return (
      <>
        {events.map((ev, i) => (
          row(ev, ev.event_index ?? offset + i)
        ))}
      </>
    );
  }

  // Turn splitting: prompt as head, body up to next prompt. Events before the first prompt are grouped into a leading turn (headIdx=-1) with no head, shown as a collapsible row.
  type Turn = { head: DocViewEvent | null; headIdx: number; body: { ev: DocViewEvent; idx: number }[] };
  const turns: Turn[] = [];
  let cur: Turn = { head: null, headIdx: offset - 1, body: [] };
  events.forEach((ev, i) => {
    const absolute = ev.event_index ?? offset + i;
    if (isPrompt(ev)) {
      turns.push(cur);
      cur = { head: ev, headIdx: absolute, body: [] };
    } else if (isRendered(ev)) {
      cur.body.push({ ev, idx: absolute });
    }
  });
  turns.push(cur);

  const toggle = (k: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });

  return (
    <>
      {turns.map((t) => {
        if (!t.head && t.body.length === 0) return null; // Empty leading turn
        const turnKey = `turn:${t.headIdx}`;
        const opened = open.has(turnKey);
        const body = t.body;
        // chat mode: the entire conversation (prompt+response) is shown from the beginning (user confirmed) —
        // only the hidden work (tools·reasoning) per response is collapsed. No turn-based collapsing.
        return (
          <div key={t.headIdx} className={`turn${opened ? ' open' : ''}`}>
            {t.head ? (
              mode === 'chat' ? (
                row(t.head, t.headIdx)
              ) : (
                <div
                  className={`turn-head${body.length > 0 ? ' has-more' : ''}`}
                  onClick={body.length > 0 ? () => toggle(turnKey) : undefined}
                  role={body.length > 0 ? 'button' : undefined}
                  tabIndex={body.length > 0 ? 0 : undefined}
                  aria-expanded={body.length > 0 ? opened : undefined}
                  onKeyDown={event => {
                    if (event.target === event.currentTarget && body.length && ['Enter', ' '].includes(event.key)) {
                      event.preventDefault(); toggle(turnKey);
                    }
                  }}
                  title={body.length > 0 ? `${tr('context.aiEvents', { count: body.length })} ${opened ? tr('common.collapse') : tr('common.expand')}` : undefined}
                >
                  {row(t.head, t.headIdx)}
                  {body.length > 0 && (
                    <span className="turn-count">
                      {opened ? '▾' : '▸'} AI {body.length}
                    </span>
                  )}
                </div>
              )
            ) : mode === 'chat' ? null : (
              <button className="turn-lead" aria-expanded={opened} onClick={() => toggle(turnKey)}>
                {opened ? '▾' : '▸'} {tr('context.leadingEvents', { count: body.length })}
              </button>
            )}
            {(mode === 'chat' || opened) &&
              (mode === 'chat'
                ? // chat: collapses the hidden work (tools·reasoning) that made each response into ▸ AI N.
                  chatGroups(t.body).map((g) => {
                    const key = `work:${g.pre[0]?.idx ?? g.msg!.idx}`;
                    const subOpened = open.has(key);
                    return (
                      <div key={`g${key}`}>
                        {/* time-based rendering (user confirmed): work (tools·reasoning) appears before the response
                            so the expanded content is above the response — same reading order as in full mode.
                            vertical guide (│) + indentation to show that the following response is part of the same group.
                            For interrupted or ongoing tail work with no answer, keep the anchor above
                            and do not imply a conclusion that does not exist. */}
                        {subOpened && g.msg && (
                          <div key="work" className="asst-work">
                            {g.pre.map(({ ev, idx }) => (
                              row(ev, idx)
                            ))}
                          </div>
                        )}
                        {g.msg ? (
                          <div
                            className={`turn-head${g.pre.length > 0 ? ' has-more' : ''}`}
                            onClick={g.pre.length > 0 ? () => toggle(key) : undefined}
                            role={g.pre.length > 0 ? 'button' : undefined}
                            tabIndex={g.pre.length > 0 ? 0 : undefined}
                            aria-expanded={g.pre.length > 0 ? subOpened : undefined}
                            onKeyDown={event => {
                              if (event.target === event.currentTarget && g.pre.length && ['Enter', ' '].includes(event.key)) {
                                event.preventDefault(); toggle(key);
                              }
                            }}
                            title={g.pre.length > 0 ? `${tr('context.aiEvents', { count: g.pre.length })} ${subOpened ? tr('common.collapse') : tr('common.expand')}` : undefined}
                          >
                            {row(g.msg.ev, g.msg.idx)}
                            {g.pre.length > 0 && (
                              <span className="turn-count">
                                {subOpened ? '▾' : '▸'} AI {g.pre.length}
                              </span>
                            )}
                          </div>
                        ) : (
                          g.pre.length > 0 && (
                            <button className="turn-lead" aria-expanded={subOpened} onClick={() => toggle(key)}>
                              {subOpened ? '▾' : '▸'} AI {g.pre.length}
                            </button>
                          )
                        )}
                        {subOpened && !g.msg && (
                          <div key="work" className="asst-work">
                            {g.pre.map(({ ev, idx }) => (
                              row(ev, idx)
                            ))}
                          </div>
                        )}
                      </div>
                    );
                  })
                : body.map(({ ev, idx }) => row(ev, idx, true)))}
          </div>
        );
      })}
    </>
  );
}

type DeferredEventProps = { event: DocViewEvent; repoId: string; hash: string; index: number };

function DeferredEventRow({ event, repoId, hash, index, revealed }: DeferredEventProps & { revealed: boolean }) {
  const translate = useT();
  const [expanded, setExpanded] = useState(false);
  if (revealed && event.kind === 'message' && !event.compact_summary) {
    return <DeferredEventContent event={event} repoId={repoId} hash={hash} index={index} />;
  }
  const label = event.provider_tool_name || event.tool_name || event.role || event.kind;
  return <div className={`msg ${event.role ?? 'tool'}`}>
    <span className="msg-role">{event.role ?? event.kind}</span>
    <div className="msg-body">
      <details className="tool-more" data-deferred-event={index} onClick={click => click.stopPropagation()}
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
  return query.data ? <EventRow ev={query.data} /> : <span role="status">{translate('context.loadingEvents')}</span>;
}

function FragmentEventContent({ repoId, hash, index }: DeferredEventProps) {
  const translate = useT();
  const [navigation, setNavigation] = useState({ offsets: [0], position: 0 });
  const offset = navigation.offsets[navigation.position];
  const query = useDocEventFragment(repoId, hash, index, offset);
  const fragment = query.data;
  return <div className="event-fragment" data-fragment-offset={offset}>
    {fragment && <>
      <p role="status">{translate('context.eventFragment', { start: fragment.byte_offset, end: fragment.next < 0 ? fragment.total_bytes : fragment.next, total: fragment.total_bytes })}</p>
      <pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', maxHeight: '60vh', overflow: 'auto' }}>{fragment.json_fragment}</pre>
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
function EventRow({ ev }: { ev: CIREvent }) {
  const t = useT();
  if (ev.kind === 'tool_call') return <ToolCallRow ev={ev} />;
  if (ev.kind === 'tool_result') return <ToolResultRow ev={ev} />;
  if (ev.kind === 'reasoning') return <ReasoningRow ev={ev} />;
  if (ev.kind === 'compaction') return <div className="compaction-divider">{t('context.compactionDivider')}</div>;
  if (isCompactSummaryEvent(ev)) return <CompactSummaryRow ev={ev} />;
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
      <details className="tool-more">
        <summary>{t('context.viewInput')}</summary>
        <pre>{JSON.stringify(input, null, 2).slice(0, 4000)}</pre>
      </details>
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
        <details className="tool-more">
          <summary>
            ↳ <code>{firstLine}</code>
            {out.length > firstLine.length && ' …'}
          </summary>
          <pre>{out.slice(0, 8000)}</pre>
        </details>
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
        <details className="tool-more reasoning">
          <summary>reasoning — {summary.split('\n')[0].slice(0, 80)}…</summary>
          {/* Reasoning summary also markdown text — rendered like chat body */}
          <Markdown text={mdSlice(summary, 8000)} />
        </details>
      </div>
    </div>
  );
}

// CompactSummaryRow — Summary generated by agent context compression. It can resemble a long user message, so a collapsible block distinguishes it (audit finding #13).
// cxt-synthesized seed digests carry the same CompactSummary marking but are not agent-written — they get their own label (#38).
function CompactSummaryRow({ ev }: { ev: CIREvent }) {
  const t = useT();
  const text = (ev.blocks ?? []).map((b) => b.text).join('\n');
  const isSeed = CXT_SEED_PREFIXES.some((p) => text.trimStart().startsWith(p));
  return (
    <div className="msg meta compact-summary">
      <span className="msg-role" />
      <div className="msg-body">
        <details className="tool-more">
          <summary>
            ◈ {t(isSeed ? 'context.cxtSeedSummary' : 'context.compactSummary')} — <code>{text.slice(0, 72)}…</code>
          </summary>
          <Markdown text={mdSlice(text, 16000)} />
        </details>
      </div>
    </div>
  );
}
