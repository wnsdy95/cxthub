import { skipToken, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';
import { api, ApiError } from './api';
import { capacityReadRetry } from './readRetry';
import { parseRevision, revisionCovers } from './repositoryUpdates';
import { denyRepositoryAccess, sameRevision } from './useRepositoryUpdates';
import type { RepositoryRevision } from './types';

export function useRepositoryBootstrap(repo: string, branch?: string) {
  const qc = useQueryClient();
  const noticeKey = ['repository-notice', repo];
  const notice = useQuery<RepositoryRevision>({queryKey: noticeKey, queryFn: skipToken});
  const followedNotice = useRef<string>();
  const query = useQuery({
    queryKey: ['repo-bootstrap', repo, branch ?? null],
    queryFn: async ({signal}) => {
      const access = qc.getQueryState(noticeKey);
      if (access?.error) throw access.error;
      try {
        const value = await api.repositoryBootstrap(repo, branch, signal);
        const latest = qc.getQueryState(noticeKey);
        if (latest?.error) throw latest.error;
        if ((access?.errorUpdateCount ?? 0) !== (latest?.errorUpdateCount ?? 0)) throw new Error('Repository access changed during the bootstrap read');
        if (!value || !parseRevision(value.revision) || typeof value.default_branch !== 'string' || !value.branch ||
            (branch && value.branch !== branch) || typeof value.archived !== 'boolean' ||
            (value.ref && (value.ref.repo_id !== repo || value.ref.kind !== 'branch' || value.ref.name !== value.branch)) ||
            (value.snapshot && (value.archived || value.snapshot.repo_id !== repo || value.snapshot.id !== value.ref?.target)) ||
            (value.parent && (value.parent.repo_id !== repo || value.parent.id !== value.snapshot?.parents?.[0]))) {
          throw new Error('Invalid repository bootstrap');
        }
        return value;
      } catch (error) {
        if (!signal.aborted) denyRepositoryAccess(repo, qc, error);
        throw error;
      }
    },
    enabled: !notice.error,
    ...capacityReadRetry,
    staleTime: Infinity,
    refetchOnMount: 'always',
    refetchOnWindowFocus: false,
  });
  const current = query.data?.revision;
  const wanted = notice.data;
  const {isFetching, isError, refetch} = query;
  useEffect(() => {
    if (!current || !wanted || notice.error || isFetching || isError || sameRevision(current, wanted) || !revisionCovers(wanted, current)) return;
    const identity = JSON.stringify([repo, branch, wanted.graph, wanted.pending, wanted.evidence ?? '0']);
    if (followedNotice.current === identity) return;
    followedNotice.current = identity;
    void refetch();
  }, [repo, branch, current, wanted, notice.error, isFetching, isError, refetch]);
  const unsupported = !notice.error && query.error instanceof ApiError && query.error.status === 501 &&
    ['catalog_unsupported', 'repository_bootstrap_unavailable'].includes(query.error.code);
  return {...query, data: notice.error ? undefined : query.data, error: notice.error ?? query.error,
    denied: Boolean(notice.error), unsupported};
}
