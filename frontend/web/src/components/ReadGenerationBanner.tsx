import { graphReadState } from '../graphReadState';
import { useT } from '../i18n';
import type { GraphProjectionStatus, RepositoryView } from '../types';

export function ReadGenerationBanner({ view, projection, error, noticeError }: {
  view?: RepositoryView; projection?: GraphProjectionStatus; error?: unknown; noticeError?: unknown;
}) {
  const translate = useT();
  const status = graphReadState(view, projection, error, noticeError);
  if (status.state === 'legacy' || status.state === 'ready' || status.state === 'denied') return null;
  const message = status.state === 'failed' ? (status.showGraph ? 'failedRetained' : 'failed') : status.state;
  return <p role="status" data-graph-read-state={status.state}>{translate(`graphRead.${message}`)}</p>;
}
