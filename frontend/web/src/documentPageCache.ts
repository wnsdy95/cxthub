import type { QueryKey } from '@tanstack/react-query';

export function documentBodyKey(owner: QueryKey, ...identity: readonly unknown[]) {
  return [...owner, 'body', ...identity] as const;
}
