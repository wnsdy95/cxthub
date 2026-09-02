// History API routing — GitHub style actual path: /<username>/<workspace-slug>.
// (Switch from hash routing: dev is vite SPA fallback by default, production requires rewrite rules)
// Click → pushState, refresh/entry → server returns index.html then path resolution, back → popstate.
import type { Workspace } from './types';

type WorkspaceRoute = Pick<Workspace, 'id' | 'owner_username' | 'slug'>;

// Reserved segments for username (to prevent conflicts with feature routes).
const RESERVED = new Set(['invite', 'w', 'login', 'settings', 'pricing', 'api', 'assets']);

// Workspace sub-tab routing (/<username>/<slug>/<tab>). Default is context (no segment).
export type WsTab = 'members' | 'connections' | 'onhold' | 'settings';

export type Route =
  | { kind: 'ws'; username: string; slug: string; tab?: WsTab } // /<username>/<slug>[/members]
  | { kind: 'user'; username: string } // /<username> (GitHub style profile)
  | { kind: 'wsid'; id: string } // Legacy /w/<id>
  | { kind: 'invite'; token: string } // /invite/<token>
  | { kind: 'device'; code: string } // /login/device?code=XXX-XXX (CLI pairing approval)
  | { kind: 'pricing' } // /pricing — public storage pricing
  | null;

// Workspace's canonical path. Fallback to id path if slug is missing (legacy fix).
export function wsPath(w: WorkspaceRoute, tab?: WsTab): string {
  const base = w.owner_username && w.slug ? `/${w.owner_username}/${w.slug}` : `/w/${w.id}`;
  return tab ? `${base}/${tab}` : base;
}

// Invitation link path.
export function invitePath(token: string): string {
  return `/invite/${token}`;
}

// Parses the current path as a route.
export function parseRoute(pathname: string = location.pathname): Route {
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
  if (seg.length === 0 || seg.length > 3) return null;
  if (seg[0] === 'pricing' && seg.length === 1) return { kind: 'pricing' };
  if (seg[0] === 'invite' && seg.length === 2) return { kind: 'invite', token: seg[1] };
  if (seg[0] === 'login' && seg[1] === 'device' && seg.length === 2) {
    return { kind: 'device', code: new URLSearchParams(location.search).get('code') ?? '' };
  }
  if (seg[0] === 'w' && seg.length === 2) return { kind: 'wsid', id: seg[1] };
  if (RESERVED.has(seg[0])) return null;
  if (seg.length === 1) return { kind: 'user', username: seg[0] }; // /<username> profile
  const tab: WsTab | undefined =
    seg[2] === 'members' || seg[2] === 'connections' || seg[2] === 'onhold' || seg[2] === 'settings'
      ? (seg[2] as WsTab)
      : undefined;
  return { kind: 'ws', username: seg[0], slug: seg[1], tab };
}

// Finds the workspace corresponding to the route in the list.
export function findByRoute(route: Route, list: Workspace[]): Workspace | undefined {
  if (!route) return undefined;
  if (route.kind === 'wsid') return list.find((w) => w.id === route.id);
  if (route.kind === 'ws') return list.find((w) => w.owner_username === route.username && w.slug === route.slug);
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
