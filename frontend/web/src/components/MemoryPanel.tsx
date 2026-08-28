import type { MemoryDigest } from '../types';
import { useT } from '../i18n';
import { Markdown } from './Markdown';

/** Immutable MemoryDigest archive attached to one snapshot.
 *
 * This is deliberately not called the active provider prompt: load/seed applies
 * provider-native deduplication, authority projection, and byte budgets later.
 */
export function MemoryPanel({ memory }: { memory: MemoryDigest }) {
  const t = useT();
  const facts = memory.key_facts ?? [];
  const tasks = memory.open_tasks ?? [];

  return (
    <details className="memory-box" data-memory-representation="archival">
      <summary className="label">◆ {t('context.compactMemory')}</summary>
      <p className="memory-note">{t('context.memoryArchiveNote')}</p>
      {memory.summary.trim() && (
        <section className="memory-section">
          <h4 className="memory-section-label">{t('context.memorySummaryLabel')}</h4>
          <Markdown text={memory.summary} />
        </section>
      )}
      {facts.length > 0 && (
        <section className="memory-section">
          <h4 className="memory-section-label">{t('context.memoryFactsLabel')}</h4>
          <ul>{facts.map((fact, i) => <li key={i}>{fact}</li>)}</ul>
        </section>
      )}
      {tasks.length > 0 && (
        <section className="memory-section">
          <h4 className="memory-section-label">{t('context.memoryTasksLabel')}</h4>
          <ul className="tasks">{tasks.map((task, i) => <li key={i}>{task}</li>)}</ul>
        </section>
      )}
    </details>
  );
}
