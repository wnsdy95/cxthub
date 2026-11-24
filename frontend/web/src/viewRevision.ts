// What an open Sessions tab does to stay current. The server keeps one short
// mark for a repository, which is another one after anything the tab shows
// was stored. The tab asks for the mark every few seconds and reads its lists
// again only when the mark is another one than the last it saw.

/** How often an open Sessions tab asks for the mark. */
export const VIEW_REVISION_MS = 3_000;

/**
 * How often the lists are read again whatever the mark says. This is the
 * fallback of following the mark: it covers a server that has no mark to
 * give and a change that landed between the first read of a list and the
 * first read of the mark.
 */
export const SESSIONS_FALLBACK_MS = 300_000;

/** The queries of everything the Sessions tab reads for one repository. */
export const SESSIONS_TAB_QUERIES = [
  'anchored-sessions', 'anchored-session', 'anchored-commit', 'anchored-commit-summaries', 'anchored-search', 'anchored-session-calls',
  'anchored-notes', 'branch-lifetimes', 'reported-worktrees', 'remote-git', 'push-origins',
] as const;

export interface SeenRevision { repo: string; revision: string }

/**
 * Takes a mark the page just read. It is remembered, and it names the queries
 * to read again: all of the tab's when the page had seen another mark of the
 * same repository before, none otherwise. The first mark of a repository is
 * only remembered: the lists were read when the tab opened.
 */
export function followRevision(seen: SeenRevision | null, repo: string, revision: string): { seen: SeenRevision; readAgain: readonly string[] } {
  const moved = seen !== null && seen.repo === repo && seen.revision !== revision;
  return { seen: { repo, revision }, readAgain: moved ? SESSIONS_TAB_QUERIES : [] };
}
