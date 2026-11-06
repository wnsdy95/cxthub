import type { RepositoryBootstrap, RepositoryView } from '../src/types';
import { sessionArchiveIndex } from '../src/sessionArchives';

export function repositoryBootstrapFixture(view: Pick<RepositoryView, 'revision' | 'default_branch' | 'refs' | 'snapshots' | 'archived_sessions'>, branch?: string, repo = ''): RepositoryBootstrap {
  const name = branch || view.default_branch || 'main';
  const ref = view.refs.find(value => value.kind === 'branch' && value.name === name);
  const archived = Boolean(ref && sessionArchiveIndex(view.archived_sessions ?? []).has(ref.target));
  const snapshot = archived ? undefined : view.snapshots.find(value => value.id === ref?.target);
  const parent = snapshot && view.snapshots.find(value => value.id === snapshot.parents?.[0]);
  return {revision: view.revision!, default_branch: view.default_branch || 'main', branch: name,
    ref: ref && {...ref, repo_id: ref.repo_id || repo}, snapshot: snapshot && {...snapshot, repo_id: snapshot.repo_id || repo},
    parent: parent && {...parent, repo_id: parent.repo_id || repo}, archived};
}
