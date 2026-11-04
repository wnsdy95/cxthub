import { useMemo, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ApiError } from '../api';
import { useT } from '../i18n';
import { DocumentProjectionError } from '../documentVisible';
import { legacyDocumentBodyOptions, type LegacyPageRecord } from '../documentLegacyLedger';
import { legacyDocumentTurns, legacyResponseGroups, legacyRowSegments, legacyGroupSegments, type LegacyLocatedRow, type LegacyTurn } from '../documentLegacyGroups';
import { DocumentEventRow, type ViewMode } from './EventStream';
import { DocumentPageWindow } from './DocumentPageWindow';
import { useDocumentPresentation } from './DocumentPresentation';

type StreamProps = { pages: LegacyPageRecord[]; mode: ViewMode; repoId: string; hash: string; targetIndex?: number; onRestart: () => void };
type RowProps = Omit<StreamProps, 'pages' | 'mode'> & { inherited: number; revealed?: boolean };

export function LegacyDocumentStream({ pages, mode, ...props }: StreamProps) {
  const turns = useMemo(() => mode === 'all' ? [] : legacyDocumentTurns(pages), [pages, mode]);
  const inherited = pages[0]?.inherited ?? 0;
  if (mode === 'all') return pages.map(page => <LegacyRows key={page.offset} {...props} inherited={inherited}
    rows={page.rows.filter(row => row.rendered).map(row => ({ ...row, page }))} />);
  return legacyGroupSegments(turns).map(segment => <DocumentPageWindow key={segment[0].index}
    identity={`units:${segment[0].head?.index ?? segment[0].body[0].index}`}>
    {() => segment.map(turn => <LegacyDocumentTurn key={turn.index} {...props} inherited={inherited} turn={turn} mode={mode} />)}
  </DocumentPageWindow>);
}

function LegacyDocumentTurn({ turn, mode, ...props }: RowProps & { turn: LegacyTurn; mode: ViewMode }) {
  const translate = useT();
  const [open, setOpen] = useDocumentPresentation(`legacy-turn:${turn.index}`, false);
  const groups = useMemo(() => mode === 'chat' ? legacyResponseGroups(turn.body) : [], [turn.body, mode]);
  return <div className={`turn${open ? ' open' : ''}`} data-legacy-turn={turn.index}>
    {turn.head ? mode === 'chat' ? <LegacyRows rows={[turn.head]} {...props} /> :
      <LegacyToggle count={turn.body.length} open={open} toggle={() => setOpen(value => !value)}>
        <LegacyRows rows={[turn.head]} {...props} />
      </LegacyToggle> : mode === 'chat' ? null : <button className="turn-lead" aria-expanded={open} onClick={() => setOpen(value => !value)}>
        {open ? '▾' : '▸'} {translate('context.leadingEvents', { count: turn.body.length })}
      </button>}
    {mode === 'chat' ? legacyGroupSegments(groups).map(segment => {
      const index = segment[0].pre[0]?.index ?? segment[0].message!.index;
      return <DocumentPageWindow key={index} identity={`units:${index}`}>
        {() => segment.map(group => <LegacyResponse key={group.pre[0]?.index ?? group.message!.index} {...props} group={group} />)}
      </DocumentPageWindow>;
    }) :
      open && <LegacyRows {...props} rows={turn.body} revealed />}
  </div>;
}

function LegacyResponse({ group, ...props }: RowProps & { group: ReturnType<typeof legacyResponseGroups>[number] }) {
  const index = group.pre[0]?.index ?? group.message!.index;
  const [open, setOpen] = useDocumentPresentation(`legacy-work:${index}`, false);
  const work = open && <div className="asst-work"><LegacyRows {...props} rows={group.pre} /></div>;
  return <div data-legacy-response={index}>
    {group.pre.length > 0 && <button className="turn-lead" aria-expanded={open} onClick={() => setOpen(value => !value)}>
      {open ? '▾' : '▸'} AI {group.pre.length}
    </button>}
    {work}
    {group.message && <LegacyRows rows={[group.message]} {...props} />}
  </div>;
}

function LegacyToggle({ count, open, toggle, children }: { count: number; open: boolean; toggle: () => void; children: ReactNode }) {
  const translate = useT();
  return <div className={`turn-head${count ? ' has-more' : ''}`} onClick={count ? toggle : undefined} role={count ? 'button' : undefined}
    tabIndex={count ? 0 : undefined} aria-expanded={count ? open : undefined}
    onKeyDown={event => {
      if (event.target === event.currentTarget && count && ['Enter', ' '].includes(event.key)) { event.preventDefault(); toggle(); }
    }} title={count ? `${translate('context.aiEvents', { count })} ${translate(open ? 'common.collapse' : 'common.expand')}` : undefined}>
    {children}{count > 0 && <span className="turn-count">{open ? '▾' : '▸'} AI {count}</span>}
  </div>;
}

function LegacyRows({ rows, ...props }: RowProps & { rows: readonly LegacyLocatedRow[] }) {
  return legacyRowSegments(rows).map(segment => <DocumentPageWindow key={segment.rows[0].index} identity={`units:${segment.rows[0].index}`}>
    {() => <LegacySegment {...props} record={segment.page} rows={segment.rows} />}
  </DocumentPageWindow>);
}

function LegacySegment({ record, rows, onRestart, targetIndex, ...props }: RowProps & { record: LegacyPageRecord; rows: LegacyLocatedRow[] }) {
  const query = useQuery(legacyDocumentBodyOptions(record));
  const translate = useT();
  if (query.isError) {
    const stale = query.error instanceof DocumentProjectionError || query.error instanceof ApiError && query.error.status === 409;
    return <div data-document-page-loading><p role="alert" className="err doc-load-error">{query.error.message}{' '}
      <button onClick={stale ? onRestart : () => { void query.refetch(); }}>{translate(stale ? 'context.restartDocument' : 'context.retryRead')}</button>
    </p></div>;
  }
  if (!query.data) return <div data-document-page-loading role="status">{translate('context.loadingEvents')}</div>;
  return rows.map(row => <DocumentEventRow key={row.index} {...props} event={query.data.events[row.index - record.offset]}
    index={row.index} targeted={row.index === targetIndex} />);
}
