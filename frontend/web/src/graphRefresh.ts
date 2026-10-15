import { ApiError, ApiTransportError } from './api';

export function isRetryableGraphRefreshError(error: unknown): boolean {
  if (error instanceof ApiTransportError) return true;
  if (!(error instanceof ApiError)) return false;
  if (error.status === 503 && error.code === 'resource_overloaded') return true;
  return error.code === '' && [408, 429, 502, 503, 504].includes(error.status);
}
