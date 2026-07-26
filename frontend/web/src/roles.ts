// 5-tier role ladder — UI response to server gate (requireRepoRole).
// UI gating is for usability, not security (enforcement is always done by the server).
import type { Membership, Workspace } from './types';

export type Role = 'viewer' | 'puller' | 'member' | 'maintainer' | 'owner';

export const ROLE_RANK: Record<string, number> = { viewer: 1, puller: 2, member: 3, maintainer: 4, owner: 5 };

// Role labels (including descriptions) are moved to i18n — roles.ts should only contain pure logic.
// UI labels are rendered using t('roles.viewer' …) (e.g., Dashboard InvitePanel).

export const ROLES: Role[] = ['viewer', 'puller', 'member', 'maintainer', 'owner'];

/** Role within the workspace. The constructor is always owner, null for non-members. */
export function myRole(ws: Workspace | null, userId: string | undefined, members: Membership[]): Role | null {
  if (!ws || !userId) return null;
  if (ws.owner_id === userId) return 'owner';
  const m = members.find((x) => x.user_id === userId);
  if (!m) return null;
  return (ROLE_RANK[m.role] ? m.role : 'member') as Role; // Unknown values are displayed as the default role.
}

/** Checks if role is min or above (same rules as server RoleRank — undefined/null always return false). */
export function atLeast(role: string | null | undefined, min: Role): boolean {
  return (ROLE_RANK[role ?? ''] ?? 0) >= ROLE_RANK[min];
}

/** Write access to team assets: maintainer or above + policy-specific (owner restriction if applicable). */
export function canWriteAsset(role: Role | null, policy: string | undefined): boolean {
  if (!atLeast(role, 'maintainer')) return false;
  if (policy === 'owner') return role === 'owner';
  return true;
}
