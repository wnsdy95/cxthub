// Canonical product routes are /{owner}/{repository}?tab=.... Exact legacy
// connection paths are resolved by the server, never guessed from tab names.
import type { Repo, Repository } from './types';

type RepositoryAddress = Pick<Repository, 'id' | 'owner_username' | 'slug'>;
const RESERVED = new Set(['invite', 'w', 'login', 'settings', 'pricing', 'api', 'assets', 'public', 'admin', 'static', 'cxt', 'connect', 'oauth', 'mcp', 'enterprises']);
export type RepositoryTab = 'members' | 'connections' | 'onhold' | 'sessions' | 'settings';
export type AccountSection = 'organizations' | 'enterprises' | 'account';
const TABS = new Set<RepositoryTab>(['members', 'connections', 'onhold', 'sessions', 'settings']);
/** What the sessions tab shows. It is the only selection below a tab that the address holds. */
export type AnchoredSelection =
 | { kind: 'all' }
 | { kind: 'commit'; sha: string }
 | { kind: 'branch'; name: string }
 | { kind: 'worktree'; name: string; machine?: string }
 | { kind: 'session'; provider: 'claude' | 'codex'; id: string; segment?: number; rest?: true };
const SESSION_ID = /^[A-Za-z0-9._-]{1,128}$/;
const COMMIT_SHA = /^[0-9a-f]{40}(?:[0-9a-f]{24})?$/;
const PLACE_NAME = /^[^\u0000-\u001f\u007f]{1,256}$/;
export type Route =
 | { kind: 'repositoryCreate' }
 | { kind: 'account'; section: AccountSection; create?: true }
 | { kind: 'githubConnections' }
 | { kind: 'repository'; username: string; slug: string; tab?: RepositoryTab; anchored?: AnchoredSelection }
 | { kind: 'repositoryId'; id: string; tab?: RepositoryTab; anchored?: AnchoredSelection }
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
/** Address of the sessions tab with one selection. Every link to it is built here. */
export function anchoredPath(repository: RepositoryAddress, selection?: AnchoredSelection): string {
 const params = new URLSearchParams({ tab: 'sessions' });
 if (selection?.kind === 'all') params.set('show', 'all');
 if (selection?.kind === 'commit') params.set('commit', selection.sha);
 if (selection?.kind === 'branch') params.set('branch', selection.name);
 if (selection?.kind === 'worktree') {
  params.set('worktree', selection.name);
  if (selection.machine) params.set('machine', selection.machine);
 }
 if (selection?.kind === 'session') {
  params.set('session', `${selection.provider}:${selection.id}`);
  if (selection.segment) params.set('segment', String(selection.segment));
  else if (selection.rest) params.set('segment', 'rest');
 }
 return `${repositoryPath(repository)}?${params}`;
}
/** Reads the selection of the sessions tab. A value that is not well formed selects nothing. */
export function parseAnchoredSelection(query: URLSearchParams): AnchoredSelection | undefined {
 const session = query.get('session');
 if (session !== null) {
  const split = session.indexOf(':');
  const provider = session.slice(0, split), id = session.slice(split + 1);
  if ((provider !== 'claude' && provider !== 'codex') || !SESSION_ID.test(id)) return undefined;
  const raw = query.get('segment');
  // A segment is named by its number; "rest" names the part after the last commit.
  const segment = raw !== null && /^[1-9][0-9]{0,6}$/.test(raw) ? { segment: Number(raw) } : raw === 'rest' ? { rest: true as const } : {};
  return { kind: 'session', provider, id, ...segment };
 }
 const commit = query.get('commit');
 if (commit !== null) return COMMIT_SHA.test(commit) ? { kind: 'commit', sha: commit } : undefined;
 const branch = query.get('branch');
 if (branch !== null) return PLACE_NAME.test(branch) ? { kind: 'branch', name: branch } : undefined;
 const worktree = query.get('worktree');
 if (worktree !== null) {
  if (!PLACE_NAME.test(worktree)) return undefined;
  const machine = query.get('machine');
  return { kind: 'worktree', name: worktree, ...(machine && PLACE_NAME.test(machine) ? { machine } : {}) };
 }
 return query.get('show') === 'all' ? { kind: 'all' } : undefined;
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
export function accountPath(section: AccountSection): string { return `/settings/${section}`; }
export function repositoryCreationPath(): string { return '/settings/repositories/new'; }
export function accountCreationPath(section: Exclude<AccountSection, 'account'>): string { return `${accountPath(section)}/new`; }
export function invitePath(token: string): string { return `/invite/${encodeURIComponent(token)}`; }
export function parseRoute(pathname: string = location.pathname, search: string = typeof location === 'undefined' ? '' : location.search): Route {
 let segments: string[];
 try { segments = pathname.split('/').filter(Boolean).map(decodeURIComponent); }
 catch { return { kind: 'notFound' }; }
 if (segments.length === 0) return null;
 if (segments.some((part) => part === '.' || part === '..' || part.includes('/') || part.includes('\\'))) return { kind: 'notFound' };
 const query = new URLSearchParams(search);
 if (segments[0] === 'settings') {
  const section = segments[1];
  if (segments.length === 3 && section === 'repositories' && segments[2] === 'new') return { kind: 'repositoryCreate' };
  if (segments.length === 2 && (section === 'account' || section === 'organizations' || section === 'enterprises')) return { kind: 'account', section };
  if (segments.length === 3 && segments[2] === 'new' && (section === 'organizations' || section === 'enterprises')) return { kind: 'account', section, create: true };
  return { kind: 'notFound' };
 }
 if (segments[0] === 'pricing' && segments.length === 1) return { kind: 'pricing' };
 if (segments[0] === 'enterprises' && segments.length === 2) return { kind: 'enterprise', slug: segments[1] };
 if (segments[0] === 'invite' && segments.length === 2) return { kind: 'invite', token: segments[1] };
 if (segments[0] === 'login' && segments[1] === 'device' && segments.length === 2) return { kind: 'device', code: query.get('code') ?? '' };
 if (segments[0] === 'connect' && segments[1] === 'github' && segments.length === 2) return { kind: 'githubConnections' };
 if (segments[0] === 'connect' && segments[1] === 'mcp' && segments.length === 2) return { kind: 'mcpConsent', request: query.get('request') ?? '' };
 const rawTab = query.get('tab') as RepositoryTab;
 const anchored = rawTab === 'sessions' ? parseAnchoredSelection(query) : undefined;
 const tab = TABS.has(rawTab) ? { tab: rawTab, ...(anchored ? { anchored } : {}) } : {};
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
