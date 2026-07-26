// Contribution heatmap (commit grid) — GitHub style. Draws a grid with daily context commits
// for the past ~53 weeks, with weeks as columns and days as rows. Data is at /public/users/{username}/contributions.
import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api';
import { useT } from '../i18n';

function ymd(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function level(count: number): number {
  if (count <= 0) return 0;
  if (count <= 2) return 1;
  if (count <= 5) return 2;
  if (count <= 9) return 3;
  return 4;
}
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

export function ContributionGraph({ username }: { username: string }) {
  const t = useT();
  const q = useQuery({
    queryKey: ['contrib', username],
    queryFn: () => api.userContributions(username),
    retry: false,
  });

  const grid = useMemo(() => {
    const counts = new Map<string, number>();
    for (const d of q.data?.days ?? []) counts.set(d.date, d.count);
    const end = new Date();
    end.setHours(0, 0, 0, 0);
    const cur = new Date(end);
    cur.setDate(end.getDate() - 7 * 52);
    cur.setDate(cur.getDate() - cur.getDay()); // Align to Sunday (start of the column week)
    const weeks: { date: string; count: number; future: boolean }[][] = [];
    const monthLabels: string[] = [];
    let total = 0;
    while (cur <= end) {
      const week: { date: string; count: number; future: boolean }[] = [];
      let label = '';
      for (let dow = 0; dow < 7; dow++) {
        const future = cur > end;
        const key = ymd(cur);
        const count = future ? 0 : counts.get(key) ?? 0;
        if (!future) total += count;
        if (cur.getDate() === 1) label = MONTHS[cur.getMonth()]; // Label this column (week) if it contains the 1st day
        week.push({ date: key, count, future });
        cur.setDate(cur.getDate() + 1);
      }
      weeks.push(week);
      monthLabels.push(label);
    }
    return { weeks, monthLabels, total };
  }, [q.data]);

  return (
    <section className="contrib">
      <div className="contrib-head">{t('profile.contribTitle', { count: grid.total.toLocaleString() })}</div>
      <div className="contrib-scroll">
        <div className="contrib-cal">
          <div className="contrib-months">
            {grid.monthLabels.map((m, i) => (
              <span key={i} className="contrib-month">
                {m}
              </span>
            ))}
          </div>
          <div className="contrib-body">
            <div className="contrib-days">
              {['', 'Mon', '', 'Wed', '', 'Fri', ''].map((d, i) => (
                <span key={i} className="contrib-day">
                  {d}
                </span>
              ))}
            </div>
            <div className="contrib-grid">
              {grid.weeks.map((week, wi) => (
                <div className="contrib-week" key={wi}>
                  {week.map((cell) =>
                    cell.future ? (
                      <i key={cell.date} className="grass future" />
                    ) : (
                      <i key={cell.date} className={`grass l${level(cell.count)}`} title={t('profile.cellTitle', { date: cell.date, count: cell.count })} />
                    ),
                  )}
                </div>
              ))}
            </div>
          </div>
        </div>
        <div className="contrib-legend">
          <span>{t('profile.less')}</span>
          <i className="grass l0" />
          <i className="grass l1" />
          <i className="grass l2" />
          <i className="grass l3" />
          <i className="grass l4" />
          <span>{t('profile.more')}</span>
        </div>
      </div>
    </section>
  );
}
