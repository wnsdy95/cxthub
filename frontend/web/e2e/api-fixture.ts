import type { Page } from '@playwright/test';

export interface ApiRequest {
  method: string;
  pathname: string;
  searchParams: URLSearchParams;
}

export interface ApiResponse {
  body: unknown;
  status?: number;
  contentType?: string;
}

export type ApiResponder = (request: ApiRequest) => ApiResponse | undefined;

/**
 * What a signed-in person with no organization and no invitation is answered,
 * and what a repository with no session and nothing read from its Git host
 * answers. The sessions tab is where a repository opens, so a test about
 * something else need not repeat these; a test about them supplies its own.
 */
function emptyAnswers(request: ApiRequest): ApiResponse | undefined {
  if (request.method !== 'GET') return undefined;
  if (['/api/v1/organizations', '/api/v1/me/invitations'].includes(request.pathname)) return { body: [] };
  if (/^\/api\/v1\/repositories\/[^/]+\/notifications$/.test(request.pathname)) return { body: [] };
  const resource = request.pathname.match(/^\/api\/v1\/repos\/[^/]+\/(.+)$/)?.[1];
  if (resource === 'remote-git') return { body: { commits: [] } };
  // The mark an open Sessions tab follows: it never moves unless a test moves it.
  if (resource === 'revision') return { body: { revision: '0.0.0' } };
  // Where pushes came from: nothing is known unless a test says so.
  if (resource === 'push-origins') return { body: { collectors: [], pushes: [] } };
  if (resource && ['anchored-sessions', 'anchored-notes', 'branch-lifetimes', 'worktrees'].includes(resource)) return { body: [] };
  return undefined;
}

/** Answers every API request of the page from `responder`. Returns the requests nothing answered. */
export async function installApiFixture(page: Page, responder: ApiResponder): Promise<string[]> {
  const unexpected: string[] = [];
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const key = `${request.method()} ${url.pathname}${url.search}`;
    const input = { method: request.method(), pathname: url.pathname, searchParams: url.searchParams };
    const response = responder(input) ?? emptyAnswers(input);
    if (!response) {
      unexpected.push(key);
      await route.fulfill({
        status: 501,
        contentType: 'application/json',
        body: JSON.stringify({ error: { message: `Unhandled E2E API request: ${key}` } }),
      });
      return;
    }
    await route.fulfill({
      status: response.status ?? 200,
      contentType: response.contentType ?? 'application/json',
      body: response.contentType === 'text/event-stream' ? String(response.body) : JSON.stringify(response.body),
    });
  });
  return unexpected;
}

export function capturePageErrors(page: Page): Error[] {
  const errors: Error[] = [];
  page.on('pageerror', (error) => errors.push(error));
  return errors;
}
