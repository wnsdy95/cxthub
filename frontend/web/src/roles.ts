// 5-tier role ladder — UI response to server gate (requireRepoRole).
// UI gating is for usability, not security (enforcement is always done by the server).
import type { Membership, Repository } from './types';

export type Role = 'viewer' | 'puller' | 'member' | 'maintainer' | 'owner';

export const ROLE_RANK: Record<string, number> = { viewer: 1, puller: 2, member: 3, maintainer: 4, owner: 5 };

// Role labels (including descriptions) are moved to i18n — roles.ts should only contain pure logic.
// UI labels are rendered using t('roles.viewer' …) (e.g., Dashboard InvitePanel).

export const ROLES: Role[] = ['viewer', 'puller', 'member', 'maintainer', 'owner'];

// Cumulative repository capability baseline. Keep this list aligned with the
// server's requireRepoRole contract (backend/internal/domain/identity.go).
// Repository policies may narrow selected maintainer capabilities to owner,
// but they never grant a capability below this baseline.
export type RoleCapability =
  | 'viewContext'
  | 'pullTeamAssets'
  | 'pushContext'
  | 'manageTeamAssets'
  | 'administerRepository';

export const ROLE_CAPABILITIES: ReadonlyArray<{
  id: RoleCapability;
  minimumRole: Role;
}> = [
  { id: 'viewContext', minimumRole: 'viewer' },
  { id: 'pullTeamAssets', minimumRole: 'puller' },
  { id: 'pushContext', minimumRole: 'member' },
  { id: 'manageTeamAssets', minimumRole: 'maintainer' },
  { id: 'administerRepository', minimumRole: 'owner' },
];

/** Server-computed repository role, including inherited Organization Owner access. */
export function myRole(repositoryMetadata: Repository | null, userId: string | undefined, members: Membership[]): Role | null {
  if (!repositoryMetadata || !userId) return null;
  // Organization, team and direct authority are resolved by the server. Missing/unknown roles
  // fail closed until the authorization projection has loaded.
  void members;
  return repositoryMetadata.effective_role && ROLE_RANK[repositoryMetadata.effective_role] ? repositoryMetadata.effective_role : null;
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
