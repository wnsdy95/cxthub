import type { ActivityCreated, ActivityMonth, ActivityRepo } from './types';

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function activityRepo(value: unknown): ActivityRepo | null {
  if (
    !isRecord(value) ||
    typeof value.name !== 'string' ||
    typeof value.path !== 'string' ||
    typeof value.count !== 'number' ||
    !Number.isFinite(value.count)
  ) {
    return null;
  }
  return { name: value.name, path: value.path, count: value.count };
}

function activityCreated(value: unknown): ActivityCreated | null {
  if (
    !isRecord(value) ||
    typeof value.name !== 'string' ||
    typeof value.path !== 'string' ||
    typeof value.visibility !== 'string' ||
    typeof value.date !== 'string'
  ) {
    return null;
  }
  return {
    name: value.name,
    path: value.path,
    visibility: value.visibility,
    date: value.date,
  };
}

function normalizeItems<T>(value: unknown, normalize: (item: unknown) => T | null): T[] {
  if (!Array.isArray(value)) return [];
  return value.map(normalize).filter((item): item is T => item !== null);
}

function activityMonth(value: unknown): ActivityMonth | null {
  if (!isRecord(value) || typeof value.month !== 'string') return null;
  return {
    month: value.month,
    commit_total:
      typeof value.commit_total === 'number' && Number.isFinite(value.commit_total) ? value.commit_total : 0,
    commit_repos: normalizeItems(value.commit_repos, activityRepo),
    created: normalizeItems(value.created, activityCreated),
  };
}

/**
 * Normalize the public activity wire response at the API boundary. Older
 * servers encoded empty Go slices as null; malformed rows must not take down
 * the entire profile page while a client and server are on different versions.
 */
export function normalizeActivityResponse(value: unknown): { months: ActivityMonth[] } {
  if (!isRecord(value)) return { months: [] };
  return { months: normalizeItems(value.months, activityMonth) };
}
