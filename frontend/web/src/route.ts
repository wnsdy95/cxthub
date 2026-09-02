// History API routing:
//   /<namespace>/<workspace>                       workspace overview / legacy repo
//   /<namespace>/<workspace>/<repository>          repository context
//   /<namespace>/<workspace>/<repository>/onhold   repository on-hold view
//   /<namespace>/<workspace>/-/<tab>                workspace management
// (Switch from hash routing: dev is vite SPA fallback by default, production requires rewrite rules)
// Click → pushState, refresh/entry → server returns index.html then path resolution, back → popstate.
import type { Repo, Workspace } from './types';

type WorkspaceRoute = Pick<Workspace, 'id' | 'owner_username' | 'slug'>;

// Reserved segments for username (to prevent conflicts with feature routes).
const RESERVED = new Set([
  'invite',
  'w',
  'login',
  'settings',
  'pricing',
  'api',
  'assets',
  'public',
  'admin',
  'static',
  'cxt',
  'connect',
  'oauth',
  'mcp',
]);

// Workspace/repository sub-view. Management tabs use /-/ to avoid colliding
// with repositories legitimately named "settings", "members", and so on.
export type WsTab = 'members' | 'connections' | 'onhold' | 'settings';

const WORKSPACE_TABS = new Set<WsTab>(['members', 'connections', 'settings']);
const LEGACY_TABS = new Set<WsTab>(['members', 'connections', 'onhold', 'settings']);

export type Route =
  | {
      kind: 'ws';
      username: string;
      slug: string;
      repository?: string;
      tab?: WsTab;
      /** Three-segment pre-multi-repo tab; used only when no repository has this exact slug. */
      legacyTab?: WsTab;
    }
  | { kind: 'user'; username: string } // /<username> (GitHub style profile)
  | { kind: 'wsid'; id: string } // Legacy /w/<id>
  | { kind: 'invite'; token: string } // /invite/<token>
  | { kind: 'device'; code: string } // /login/device?code=XXX-XXX (CLI pairing approval)
  | { kind: 'mcpConsent'; request: string } // /connect/mcp?request=... (remote MCP OAuth consent)
  | { kind: 'pricing' } // /pricing — public storage pricing
  | null;

// Workspace's canonical path. Fallback to id path if slug is missing (legacy fix).
export function wsPath(w: WorkspaceRoute, tab?: WsTab): string {
  const base = w.owner_username && w.slug ? `/${w.owner_username}/${w.slug}` : `/w/${w.id}`;
  return tab ? `${base}/-/${tab}` : base;
}

/** Repository URL segment. Empty means a legacy two-segment repository. */
export function repositorySlug(repo: Pick<Repo, 'remote_url'>): string {
  try {
    const segments = new URL(repo.remote_url).pathname.split('/').filter(Boolean);
    if (segments.length < 3) return '';
    return decodeURIComponent(segments.at(-1) ?? '');
  } catch {
    return '';
  }
}

export function repoPath(
  w: WorkspaceRoute,
  repo: Pick<Repo, 'remote_url'>,
  tab?: Extract<WsTab, 'onhold'>,
): string {
  const base = wsPath(w);
  const repository = repositorySlug(repo);
  if (!repository) return tab ? `${base}/-/onhold` : base;
  const path = `${base}/${encodeURIComponent(repository)}`;
  return tab ? `${path}/${tab}` : path;
}

// Invitation link path.
export function invitePath(token: string): string {
  return `/invite/${token}`;
}

// Parses the current path as a route.
export function parseRoute(
  pathname: string = location.pathname,
  search: string = typeof location === 'undefined' ? '' : location.search,
): Route {
  // Unicode slug (e.g., Korean) is percent-encoded in pathname, so decode and compare.
  const seg = pathname
    .split('/')
    .filter(Boolean)
    .map((x) => {
      try {
        return decodeURIComponent(x);
      } catch {
        return x;
      }
    });
  if (seg.length === 0 || seg.length > 4) return null;
  if (seg[0] === 'pricing' && seg.length === 1) return { kind: 'pricing' };
  if (seg[0] === 'invite' && seg.length === 2) return { kind: 'invite', token: seg[1] };
  if (seg[0] === 'login' && seg[1] === 'device' && seg.length === 2) {
    return { kind: 'device', code: new URLSearchParams(search).get('code') ?? '' };
  }
  if (seg[0] === 'connect' && seg[1] === 'mcp' && seg.length === 2) {
    return { kind: 'mcpConsent', request: new URLSearchParams(search).get('request') ?? '' };
  }
  if (seg[0] === 'w' && seg.length === 2) return { kind: 'wsid', id: seg[1] };
  if (RESERVED.has(seg[0])) return null;
  if (seg.length === 1) return { kind: 'user', username: seg[0] }; // /<username> profile
  if (seg.length === 2) return { kind: 'ws', username: seg[0], slug: seg[1] };
  if (seg.length === 4 && seg[2] === '-' && WORKSPACE_TABS.has(seg[3] as WsTab)) {
    return { kind: 'ws', username: seg[0], slug: seg[1], tab: seg[3] as WsTab };
  }
  if (seg.length === 4 && seg[2] !== '-' && seg[3] === 'onhold') {
    return { kind: 'ws', username: seg[0], slug: seg[1], repository: seg[2], tab: 'onhold' };
  }
  if (seg.length !== 3 || seg[2] === '-') return null;
  const legacyTab = LEGACY_TABS.has(seg[2] as WsTab) ? (seg[2] as WsTab) : undefined;
  return { kind: 'ws', username: seg[0], slug: seg[1], repository: seg[2], ...(legacyTab ? { legacyTab } : {}) };
}

// Finds the workspace corresponding to the route in the list.
export function findByRoute(route: Route, list: Workspace[]): Workspace | undefined {
  if (!route) return undefined;
  if (route.kind === 'wsid') return list.find((w) => w.id === route.id);
  if (route.kind === 'ws') return list.find((w) => w.owner_username === route.username && w.slug === route.slug);
  return undefined;
}

export function findRepositoryByRoute(route: Route, list: Repo[]): Repo | undefined {
  if (!route || route.kind !== 'ws' || !route.repository) return undefined;
  return list.find((repo) => repositorySlug(repo) === route.repository);
}

/**
 * Resolves the transitional three-segment tab format. An exact repository
 * match always wins, so a repository named "settings" remains addressable.
 */
export function resolvedWorkspaceTab(route: Route, repos: Repo[]): WsTab | undefined {
  if (!route || route.kind !== 'ws') return undefined;
  if (route.tab) return route.tab;
  if (route.legacyTab && !findRepositoryByRoute(route, repos)) return route.legacyTab;
  return undefined;
}

// notifyRoute synthesizes a popstate event after pushState/replaceState (the browser does not fire popstate for programmatic history changes, so it centralizes listeners).
function notifyRoute(): void {
  window.dispatchEvent(new PopStateEvent('popstate'));
}

// User navigation: history is pushed, back button works.
export function navigate(path: string): void {
  history.pushState(null, '', path);
  notifyRoute();
}

// Non-user navigation (auto selection, correction, redirect): does not contaminate history.
export function replacePath(path: string): void {
  history.replaceState(null, '', path);
  notifyRoute();
}

// Upgrades legacy hash URLs (#/w/<id>, #/<u>/<s>, #/invite/<t>) to actual paths (once on boot).
export function upgradeLegacyHash(): void {
  if (location.hash.startsWith('#/')) {
    history.replaceState(null, '', location.hash.slice(1));
  }
}
