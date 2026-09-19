import type { Pending } from './types';

export const PENDING_LIVE_MS = 120_000;

/** LIVE means recently observed transcript activity, never mere re-upload or
 * proof of an open app. Expire even when polling fails or a client disappears. */
export function pendingIsLive(p: Pending, now = Date.now()): boolean {
  const at = p.activity_at ? Date.parse(p.activity_at) : NaN;
  return Number.isFinite(at) && at <= now + 5_000 && now - at < PENDING_LIVE_MS;
}
