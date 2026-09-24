// Canonical product routes are /{owner}/{repository}?tab=.... Exact legacy
// connection paths are resolved by the server, never guessed from tab names.
import type { Repo, Repository } from './types';

type RepositoryAddress = Pick<Repository, 'id' | 'owner_username' | 'slug'>;
const RESERVED = new Set(['invite', 'w', 'login', 'settings', 'pricing', 'api', 'assets', 'public', 'admin', 'static', 'cxt', 'connect', 'oauth', 'mcp', 'enterprises']);
export type RepositoryTab = 'members' | 'connections' | 'onhold' | 'settings';
const TABS = new Set<RepositoryTab>(['members', 'connections', 'onhold', 'settings']);
export type Route =
 | { kind: 'githubConnections' }
 | { kind: 'repository'; username: string; slug: string; tab?: RepositoryTab }
 | { kind: 'repositoryId'; id: string; tab?: RepositoryTab }
 | { kind: 'user'; username: string }
 | { kind: 'enterprise'; slug: string }
 | { kind: 'invite'; token: string }
 | { kind: 'device'; code: string }
 | { kind: 'mcpConsent'; request: string }
 | { kind: 'pricing' }
 | { kind: 'notFound' }
 | null;

export function repositoryPath(repository: RepositoryAddress, tab?: RepositoryTab): string {
 const base = repository.owner_username && repository.slug
  ? `/${encodeURIComponent(repository.owner_username)}/${encodeURIComponent(repository.slug)}`
  : `/w/${repository.id}`;
 return tab ? `${base}?tab=${tab}` : base;
}
/** Display-only connection name. Identity comes from the server, never this segment. */
export function repositorySlug(repo: Pick<Repo, 'remote_url'>): string {
 try { return decodeURIComponent(new URL(repo.remote_url).pathname.split('/').filter(Boolean).at(-1) ?? ''); }
 catch { return ''; }
}
export function repoPath(repository: RepositoryAddress, _repo: Pick<Repo, 'remote_url'>, tab?: Extract<RepositoryTab, 'onhold'>): string {
 return repositoryPath(repository, tab);
}
export function enterprisePath(slug: string): string { return `/enterprises/${encodeURIComponent(slug)}`; }
export function invitePath(token: string): string { return `/invite/${encodeURIComponent(token)}`; }
export function parseRoute(pathname: string = location.pathname, search: string = typeof location === 'undefined' ? '' : location.search): Route {
 let segments: string[];
 try { segments = pathname.split('/').filter(Boolean).map(decodeURIComponent); }
 catch { return { kind: 'notFound' }; }
 if (segments.length === 0) return null;
 if (segments.some((part) => part === '.' || part === '..' || part.includes('/') || part.includes('\\'))) return { kind: 'notFound' };
 const query = new URLSearchParams(search);
 if (segments[0] === 'pricing' && segments.length === 1) return { kind: 'pricing' };
 if (segments[0] === 'enterprises' && segments.length === 2) return { kind: 'enterprise', slug: segments[1] };
 if (segments[0] === 'invite' && segments.length === 2) return { kind: 'invite', token: segments[1] };
 if (segments[0] === 'login' && segments[1] === 'device' && segments.length === 2) return { kind: 'device', code: query.get('code') ?? '' };
 if (segments[0] === 'connect' && segments[1] === 'github' && segments.length === 2) return { kind: 'githubConnections' };
 if (segments[0] === 'connect' && segments[1] === 'mcp' && segments.length === 2) return { kind: 'mcpConsent', request: query.get('request') ?? '' };
 const rawTab = query.get('tab') as RepositoryTab;
 const tab = TABS.has(rawTab) ? { tab: rawTab } : {};
 if (segments[0] === 'w' && segments.length === 2) return { kind: 'repositoryId', id: segments[1], ...tab };
 if (RESERVED.has(segments[0])) return { kind: 'notFound' };
 if (segments.length === 1) return { kind: 'user', username: segments[0] };
 if (segments.length > 3 || segments[2] === '-') return { kind: 'notFound' };
 // A three-segment address is accepted only if the backend has this exact
 // historical alias. PublicBrowse redirects its resolved canonical address.
 return { kind: 'repository', username: segments[0], slug: segments.slice(1).join('/'), ...tab };
}
export function findByRoute(route: Route, list: Repository[]): Repository | undefined {
 if (route?.kind === 'repositoryId') return list.find((repository) => repository.id === route.id);
 if (route?.kind === 'repository') return list.find((repository) => repository.owner_username === route.username && repository.slug === route.slug);
 return undefined;
}
export function findRepositoryByRoute(route: Route, list: Repo[]): Repo | undefined {
 if (route?.kind !== 'repository' && route?.kind !== 'repositoryId') return undefined;
 return list.length === 1 ? list[0] : undefined;
}
export function resolvedRepositoryTab(route: Route, _repos: Repo[]): RepositoryTab | undefined {
 return route?.kind === 'repository' || route?.kind === 'repositoryId' ? route.tab : undefined;
}
function notifyRoute(): void { window.dispatchEvent(new PopStateEvent('popstate')); }
export function navigate(path: string): void { history.pushState(null, '', path); notifyRoute(); }
export function replacePath(path: string): void { history.replaceState(null, '', path); notifyRoute(); }
export function upgradeLegacyHash(): void { if (location.hash.startsWith('#/')) history.replaceState(null, '', location.hash.slice(1)); }
