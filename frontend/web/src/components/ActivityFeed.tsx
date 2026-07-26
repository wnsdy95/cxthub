// Contribution activity — GitHub profile activity feed. Two types expressible with our data:
//   1) Monthly context commit bundles (workspace counts + relative bars)
//   2) Workspaces created that month (public/private·date)
// (PRs, reviews, languages omitted due to lack of data.) Data: /public/users/{username}/activity.
import { useQuery } from '@tanstack/react-query';
import type { ActivityMonth } from '../types';
import { api } from '../api';
import { navigate } from '../route';
import { LockIcon } from './Breadcrumb';
import { useT } from '../i18n';

// octicon commit / repo — card left icon.
function CommitIcon() {
  return (
    <svg viewBox="0 0 16 16" width="15" height="15" fill="currentColor" aria-hidden="true">
      <path d="M11.93 8.5a4.002 4.002 0 0 1-7.86 0H.75a.75.75 0 0 1 0-1.5h3.32a4.002 4.002 0 0 1 7.86 0h3.32a.75.75 0 0 1 0 1.5Zm-1.43-.75a2.5 2.5 0 1 0-5 0 2.5 2.5 0 0 0 5 0Z" />
    </svg>
  );
}
function RepoIcon() {
  return (
    <svg viewBox="0 0 16 16" width="15" height="15" fill="currentColor" aria-hidden="true">
      <path d="M2 2.5A2.5 2.5 0 0 1 4.5 0h8.75a.75.75 0 0 1 .75.75v12.5a.75.75 0 0 1-.75.75h-2.5a.75.75 0 0 1 0-1.5h1.75v-2h-8a1 1 0 0 0-.714 1.7.75.75 0 1 1-1.072 1.05A2.495 2.495 0 0 1 2 11.5Zm10.5-1h-8a1 1 0 0 0-1 1v6.708A2.486 2.486 0 0 1 4.5 9h8ZM5 12.25v3.25a.25.25 0 0 0 .4.2l1.45-1.087a.25.25 0 0 1 .3 0L8.6 15.7a.25.25 0 0 0 .4-.2v-3.25a.25.25 0 0 0-.25-.25h-3.5a.25.25 0 0 0-.25.25Z" />
    </svg>
  );
}

function MonthBlock({ m }: { m: ActivityMonth }) {
  const t = useT();
  const max = Math.max(1, ...m.commit_repos.map((r) => r.count));
  const [my, mmo] = m.month.split('-');
  return (
    <div className="act-month">
      <div className="act-month-label">{t('profile.month', { y: my, mo: Number(mmo) })}</div>

      {m.commit_total > 0 && (
        <div className="act-card">
          <span className="act-icon">
            <CommitIcon />
          </span>
          <div className="act-body">
            <div className="act-title">
              {t('profile.commitSummary', { count: m.commit_total.toLocaleString(), repos: m.commit_repos.length })}
            </div>
            <ul className="act-rows">
              {m.commit_repos.map((r) => (
                <li key={r.path} className="act-row">
                  <button className="act-link" onClick={() => navigate(`/${r.path}`)}>
                    {r.path}
                  </button>
                  <span className="act-count">{t('profile.commitsN', { count: r.count })}</span>
                  <span className="act-bar">
                    <i style={{ width: `${Math.max(6, Math.round((r.count / max) * 100))}%` }} />
                  </span>
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}

      {m.created.length > 0 && (
        <div className="act-card">
          <span className="act-icon">
            <RepoIcon />
          </span>
          <div className="act-body">
            <div className="act-title">{t('profile.createdSummary', { count: m.created.length })}</div>
            <ul className="act-rows">
              {m.created.map((c) => (
                <li key={c.path} className="act-row">
                  {c.visibility !== 'public' && <LockIcon className="crumb-lock" />}
                  <button className="act-link" onClick={() => navigate(`/${c.path}`)}>
                    {c.path}
                  </button>
                  <span className="act-vis">{c.visibility === 'public' ? t('common.public') : t('common.private')}</span>
                  <span className="act-date">{c.date}</span>
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}
    </div>
  );
}

export function ActivityFeed({ username }: { username: string }) {
  const t = useT();
  const q = useQuery({
    queryKey: ['activity', username],
    queryFn: () => api.userActivity(username),
    retry: false,
  });
  const months = (q.data?.months ?? []).filter((m) => m.commit_total > 0 || m.created.length > 0);
  if (months.length === 0) return null;

  return (
    <section className="act">
      <h2 className="profile-section act-head">{t('profile.activity')}</h2>
      <div className="act-timeline">
        {months.map((m) => (
          <MonthBlock key={m.month} m={m} />
        ))}
      </div>
    </section>
  );
}
