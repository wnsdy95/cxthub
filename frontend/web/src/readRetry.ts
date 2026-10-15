import { ApiError } from './api';

export function retryCapacityRead(failureCount: number, error: unknown): boolean {
  return failureCount < 4 && error instanceof ApiError && error.status === 503 &&
    error.code === 'resource_overloaded' && (error.retryAfterMs === undefined || error.retryAfterMs <= 10_000);
}

export function capacityRetryDelay(attempt: number, error: unknown): number {
  const hint = error instanceof ApiError ? error.retryAfterMs ?? 0 : 0;
  const backoff = 1000 * 2 ** Math.min(Math.max(attempt, 0), 3);
  return Math.max(hint, backoff) + Math.floor(Math.random() * 251);
}

export const capacityReadRetry = {
  retry: (failureCount: number, error: Error) => retryCapacityRead(failureCount, error),
  retryDelay: (attempt: number, error: Error) => capacityRetryDelay(attempt, error),
};
