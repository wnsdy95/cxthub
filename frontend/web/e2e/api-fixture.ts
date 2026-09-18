import type { Page } from '@playwright/test';

export interface ApiRequest {
  method: string;
  pathname: string;
  searchParams: URLSearchParams;
}

export interface ApiResponse {
  body: unknown;
  status?: number;
}

export type ApiResponder = (request: ApiRequest) => ApiResponse | undefined;

// Existing fixtures describe each component. Assemble one server response at
// request time, preserving failures; production never performs this fan-out.
function graphViewFixture(request: ApiRequest, responder: ApiResponder): ApiResponse | undefined {
  if (request.method !== 'GET' || !/^\/api\/v1\/repos\/[^/]+\/view$/.test(request.pathname)) return undefined;
  const body: Record<string, unknown> = {};
  for (const key of ['refs', 'snapshots', 'reflog', 'history', 'pending', 'unsync']) {
    const response = responder({ ...request, pathname: request.pathname.replace(/view$/, key) });
    if (!response) return undefined;
    if ((response.status ?? 200) >= 400) return response;
    body[key] = response.body;
  }
  return { body };
}

export async function installApiFixture(page: Page, responder: ApiResponder): Promise<string[]> {
  const unexpected: string[] = [];
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const key = `${request.method()} ${url.pathname}${url.search}`;
    const input = {
      method: request.method(),
      pathname: url.pathname,
      searchParams: url.searchParams,
    };
    const response = responder(input) ?? graphViewFixture(input, responder)
      ?? (request.method() === 'GET' && /^\/api\/v1\/workspaces\/[^/]+\/notifications$/.test(url.pathname) ? { body: [] } : undefined)
      ?? (request.method() === 'GET' && /^\/api\/v1\/repos\/[^/]+\/prs\/promotions$/.test(url.pathname) ? { body: [] } : undefined);
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
      contentType: 'application/json',
      body: JSON.stringify(response.body),
    });
  });
  return unexpected;
}

export function capturePageErrors(page: Page): Error[] {
  const errors: Error[] = [];
  page.on('pageerror', (error) => errors.push(error));
  return errors;
}
