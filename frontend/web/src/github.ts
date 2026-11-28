import type { Repo, Repository, Team } from './types';
interface GitHubRepository { id: number; account_id: number; full_name: string; private: boolean }
interface GitHubBinding { repository_id: string; context_repo_id: string; external_id: number }
interface GitHubTeamMapping { team_id: string; external_id: number; sync_members: boolean }
interface GitHubConnection {
 namespace_id: string; generation: number; enabled: boolean; status: string; checked_at: string; next_sync: string;
 installation: { id: number; account_id: number; login: string; kind: string; suspended: boolean };
 repositories: GitHubRepository[]; bindings: GitHubBinding[]; teams: { id: number; slug: string; name: string }[];
 mappings: GitHubTeamMapping[]; unresolved_members: number;
}
export interface GitHubOwner { namespace: { id: string; slug: string; kind: 'user' | 'organization' }; connection: GitHubConnection | null; repositories: Repository[]; context_repos: (Repo & { repository_id: string })[]; teams: Team[] }
export interface GitHubOverview { enabled: boolean; identity: { external_id: number; login: string } | null; owners: GitHubOwner[] }
export interface GitHubEdit { generation: number; action: 'refresh' | 'disconnect' | 'bind' | 'unbind' | 'map-team' | 'unmap-team'; binding?: GitHubBinding; mapping?: GitHubTeamMapping }
export interface EnterpriseGitHubConnection { organization_id: string; namespace_id: string; slug: string; status: string; checked_at: string; can_manage: boolean }
