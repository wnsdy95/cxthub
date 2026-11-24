import type { PushCollector, PushOrigin, PushOriginView } from './types';

/**
 * Where a commit without a session came from, as far as the server knows.
 * The server stores each commit with the push it came with, and says of each
 * push whether a computer where cxt runs made it.
 */
export interface CommitOrigin {
  push: PushOrigin;
  /** The collector that said its clone made the push. */
  collector?: PushCollector;
  /** Collectors that have not been heard from since the push: one of them may still say that it made it. */
  waiting: PushCollector[];
}

/**
 * The origin of each push by its newest commit. The same commit pushed to two
 * branches is one commit: a push a collector claimed says more about it than
 * one nobody claimed, and otherwise the newer push is kept, which the server
 * lists first.
 */
export function pushOriginsByHead(view: PushOriginView | undefined): Map<string, CommitOrigin> {
  const byHead = new Map<string, CommitOrigin>();
  if (!view) return byHead;
  for (const push of view.pushes) {
    const held = byHead.get(push.sha);
    if (held && (held.push.collector !== undefined || push.collector === undefined)) continue;
    byHead.set(push.sha, {
      push,
      collector: push.collector === undefined ? undefined : view.collectors[push.collector],
      waiting: (push.waiting ?? []).map(index => view.collectors[index]).filter((collector): collector is PushCollector => collector !== undefined),
    });
  }
  return byHead;
}

/** A commit came with a push; where that push came from is where the commit came from. */
export function commitOrigin(byHead: Map<string, CommitOrigin>, pushHead: string | undefined): CommitOrigin | undefined {
  return pushHead ? byHead.get(pushHead) : undefined;
}

/** The caller's own collectors whose last run failed: the one person who can repair them is told. */
export function ownFailingCollectors(view: PushOriginView | undefined): PushCollector[] {
  return (view?.collectors ?? []).filter(collector => collector.own && collector.failing);
}
