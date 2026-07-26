/**
 * infrastructure/index: infrastructure layer public barrel re-export.
 *
 * Import in composition root (presentation/bootstrap/container.ts).
 * Other layers (application, domain, presentation) do not import this barrel directly.
 *
 * Dependency direction: infrastructure → application (ports) + domain.
 */

export { HttpClient } from './http/http-client.js';
export type { HttpClientConfig, HttpError, ApiErrorEnvelope } from './http/http-client.js';

export { ApiRoutes } from './http/api-routes.js';

export { RestSessionGateway } from './rest-session-gateway.js';
