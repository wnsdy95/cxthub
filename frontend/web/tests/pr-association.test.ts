import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { I18nProvider, translate } from '../src/i18n/index.tsx';
import { ko } from '../src/i18n/locales/ko.ts';
import { PRPromotions } from '../src/components/PRPromotions.tsx';
import type { PRJobAssociation, PRPromotionJob } from '../src/types.ts';

function render(association?: PRJobAssociation): string {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
  const job: PRPromotionJob = {
    id: 'job', repo_id: 'repo', pr: { number: 488, base_branch: 'main', head_branch: 'feature', head_sha: 'a', merge_sha: 'b' },
    state: 'completed', attempts: 1, updated_at: '2026-10-11T00:00:00Z', next_attempt: '2026-10-11T00:00:00Z', association,
  };
  client.setQueryData(['pr-promotions', 'repo'], [job]);
  const html = renderToStaticMarkup(createElement(QueryClientProvider, { client },
    createElement(I18nProvider, null, createElement(PRPromotions, { repoId: 'repo', canRetry: false })),
  ));
  client.clear();
  return html;
}

const recorded: PRJobAssociation = {
  provider_history: 'recorded_complete', provider_checkpoint_count: 1,
  participation: 'recorded', participation_count: 3, publication_count: 2,
  contribution: 'recorded', contribution_count: 1, current_applicability: 'not_evaluated',
};
const html = render(recorded);
assert.match(html, /PR association evidence/);
assert.match(html, /3 witness records/);
assert.match(html, /2 distinct publications/);
assert.match(html, /1 contribution record/);
assert.match(html, /not revalidated associations/);
assert.match(html, /Provider scanner availability is unknown/);
assert.match(html, /Current code applicability is not evaluated/);
assert.match(render(), /Association diagnostics are not available in this response/);

const unknown = render({ provider_history: 'unknown', participation: 'unknown', contribution: 'unavailable', current_applicability: 'not_evaluated' });
assert.match(unknown, /Unknown within this read/);
assert.match(unknown, /Storage unavailable or unreadable/);
assert.doesNotMatch(unknown, /0 (witness|contribution|matching)/);
const empty = render({ ...recorded, participation: 'not_recorded', participation_count: 0, publication_count: 0, contribution: 'not_recorded', contribution_count: 0 });
assert.match(empty, /0 witness records/);
assert.match(empty, /No matching records/);
assert.equal(translate('ko', 'promotion.associationRuntime'), ko.promotion.associationRuntime);
assert.equal(translate('ko', 'promotion.associationApplicability'), ko.promotion.associationApplicability);
