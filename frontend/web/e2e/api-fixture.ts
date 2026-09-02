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

export async function installApiFixture(page: Page, responder: ApiResponder): Promise<string[]> {
  const unexpected: string[] = [];
  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const key = `${request.method()} ${url.pathname}${url.search}`;
    const response = responder({
      method: request.method(),
      pathname: url.pathname,
      searchParams: url.searchParams,
    });
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
