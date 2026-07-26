/**
 * infrastructure/http/http-client: fetch-based low-level HTTP wrapper.
 *
 * Uses standard fetch (Node 22 / browser built-in) without external npm dependencies.
 * Normalizes error model (including JSON envelope with code field) from SYNC-PROTOCOL §8 to HttpError.
 *
 * Responsibilities:
 *   - Combines base URL with path.
 *   - Serializes request JSON / deserializes response JSON.
 *   - Converts non-2xx responses to HttpError.
 *   - Injects test fetch implementation (fetchImpl parameter).
 *
 * Dependencies: No external packages. Standard fetch API (lib="DOM" type provided).
 */

// ── Error Types ─────────────────────────────────────────────

/**
 * SYNC-PROTOCOL §8 error envelope.
 * JSON returned by server: { "error": { "code": "...", "message": "...", "details": {} } }
 */
export interface ApiErrorEnvelope {
  error: {
    code: string;
    message: string;
    details?: Record<string, unknown>;
  };
}

/**
 * Exception class for backend HTTP errors.
 * status: HTTP status code (e.g., 409, 422).
 * code: Machine-readable code from SYNC-PROTOCOL §8 (e.g., "diverged_forked", "integrity_violation").
 */
export class HttpError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
    public readonly details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = 'HttpError';
  }
}

// ── HttpClient Configuration ───────────────────────────────

/**
 * HttpClient creation configuration.
 * fetchImpl: Used to inject mock fetch in tests (default is global fetch).
 */
export interface HttpClientConfig {
/** Base URL (e.g., "http://127.0.0.1:8787"). No trailing slash. */
  baseUrl: string;
/**
 * Fetch implementation to use (optional).
 * Uses global fetch by default. Fake implementation can be injected for testing.
 */
  fetchImpl?: typeof fetch;
}

// ── HttpClient ────────────────────────────────────────────────

/**
 * Low-level HTTP client based on fetch.
 * Called by RestSessionGateway to invoke the backend.
 *
 * All methods return a Promise, throwing HttpError for non-2xx responses.
 * JSON parsing failures throw a general Error.
 */
export class HttpClient {
  private readonly baseUrl: string;
  private readonly fetchFn: typeof fetch;

  constructor(config: HttpClientConfig) {
    this.baseUrl = config.baseUrl.replace(/\/$/, '');
    this.fetchFn = config.fetchImpl ?? globalThis.fetch.bind(globalThis);
  }

/**
 * Sends a GET request and returns a JSON response of type T.
 * @param path - Path to append to the base URL (e.g., "/api/v1/repos").
 */
  get<T>(path: string): Promise<T> {
    return this.request<T>('GET', path);
  }

/**
 * Sends a POST request and returns a JSON response of type T.
 */
  post<T>(path: string, body: unknown): Promise<T> {
    return this.request<T>('POST', path, body);
  }

/**
 * Sends a PUT request and returns a JSON response of type T.
 */
  put<T>(path: string, body: unknown): Promise<T> {
    return this.request<T>('PUT', path, body);
  }

/** Common request handling: JSON serialization/deserialization + non-2xx → HttpError(§8 Envelope Parsing). */
  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const init: RequestInit = { method };
    if (body !== undefined) {
      init.headers = { 'Content-Type': 'application/json' };
      init.body = JSON.stringify(body);
    }
    const res = await this.fetchFn(this.baseUrl + path, init);
    const text = await res.text();
    if (!res.ok) {
      let code = 'http_error';
      let message = res.statusText || `HTTP ${res.status}`;
      let details: Record<string, unknown> | undefined;
      try {
        const env = JSON.parse(text) as ApiErrorEnvelope;
        if (env && env.error) {
          code = env.error.code;
          message = env.error.message;
          details = env.error.details;
        }
      } catch {
        // Ignore non-JSON error bodies and use status-based messages.
      }
      throw new HttpError(res.status, code, message, details);
    }
    if (text.length === 0) {
      return undefined as unknown as T;
    }
    return JSON.parse(text) as T;
  }
}
