import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api';
import { useCreateOrganization, useOrganizations } from '../hooks';
import { useT } from '../i18n';
import { accountPath, accountCreationPath, enterprisePath, navigate, type AccountSection } from '../route';
import type { User } from '../types';
import { safeAvatarUrl } from '../urls';
import { HeaderActions } from './HeaderActions';
import { AppLink } from './AppLink';
import { avatarColor } from './Avatar';
import { Logo } from './Logo';
import { AccountSettings } from './Settings';

export function AccountPage({ user, section, create = false }: { user: User; section: AccountSection; create?: boolean }) {
  const t = useT();
  const title = section === 'account' ? t('settings.account') : create
    ? t(section === 'organizations' ? 'organization.createTitle' : 'enterprise.create')
    : t(section === 'organizations' ? 'accountNav.organizations' : 'accountNav.enterprises');
  return <div className={`app account-app${section === 'account' ? ' account-settings-page' : ''}`}>
    <header className="topbar">
      <AppLink href="/" className="linkish-logo" aria-label={t('common.home')}><div className="brand sm"><Logo /></div></AppLink>
      <div className="who"><HeaderActions user={user} /></div>
    </header>
    <div className="account-layout">
      <nav className="account-nav" aria-label={t('accountNav.navigation')}>
        <AppLink href={`/${encodeURIComponent(user.username)}`}>{t('accountNav.profile')}</AppLink>
        {(['organizations', 'enterprises', 'account'] as const).map((item) => <AppLink key={item} href={accountPath(item)} aria-current={section === item ? 'page' : undefined}>
          {t(item === 'account' ? 'settings.account' : item === 'organizations' ? 'accountNav.organizations' : 'accountNav.enterprises')}
        </AppLink>)}
      </nav>
      <main className="account-main" key={`${user.id}:${section}:${create}`}>
        <div className="account-page-heading">
          <h1>{title}</h1>
          {section === 'account' && <p>{t('accountUI.intro')}</p>}
        </div>
        {section === 'account' ? <AccountSettings user={user} /> : create ? <CreateSpace section={section} /> : section === 'organizations' ? <OrganizationDirectory /> : <EnterpriseDirectory />}
      </main>
    </div>
  </div>;
}

type Space = { id: string; name: string; slug: string; logo?: string; effective_role?: string };

function SpaceDirectory({ section, spaces, loading, error, retry }: {
  section: Exclude<AccountSection, 'account'>;
  spaces: Space[];
  loading: boolean;
  error: Error | null;
  retry: () => void;
}) {
  const t = useT();
  return <>
    <div className="account-page-actions">
      {!loading && !error && <span className="count-badge">{spaces.length}</span>}
      <AppLink className="account-create-link" href={accountCreationPath(section)}>{t(section === 'organizations' ? 'organization.createTitle' : 'enterprise.create')}</AppLink>
    </div>
    {loading ? <p role="status">{t('accountNav.loading')}</p> : error ? <div className="empty-box"><p className="err" role="alert">{error.message}</p><button type="button" onClick={retry}>{t('accountNav.retry')}</button></div> : spaces.length === 0 ? <p className="empty-box">{t(section === 'organizations' ? 'accountNav.noOrganizations' : 'enterprise.empty')}</p> :
      <ul className="account-space-list">{spaces.map((space) => {
        const href = section === 'organizations' ? `/${encodeURIComponent(space.slug)}` : enterprisePath(space.slug);
        const logo = safeAvatarUrl(space.logo);
        return <li key={space.id}><AppLink className="account-space-card" href={href}>
          {logo ? <img src={logo} alt="" /> : <span className="account-space-avatar" style={{ background: avatarColor(space.slug) }} aria-hidden="true">{space.name.charAt(0).toUpperCase()}</span>}
          <span className="account-space-name"><strong>{space.name}</strong><small>{href}</small></span>
          {space.effective_role && <span className="role">{space.effective_role}</span>}
          <span aria-hidden="true">→</span>
        </AppLink></li>;
      })}</ul>}
  </>;
}

function OrganizationDirectory() {
  const query = useOrganizations();
  return <SpaceDirectory section="organizations" spaces={query.data ?? []} loading={query.isPending} error={query.error} retry={() => void query.refetch()} />;
}

function EnterpriseDirectory() {
  const query = useQuery({ queryKey: ['enterprises'], queryFn: api.listEnterprises });
  return <SpaceDirectory section="enterprises" spaces={query.data ?? []} loading={query.isPending} error={query.error} retry={() => void query.refetch()} />;
}

function SpaceForm({ section, onSubmit, busy, error }: { section: Exclude<AccountSection, 'account'>; onSubmit: (name: string, slug: string) => void; busy: boolean; error: Error | null }) {
  const t = useT();
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');
  function submit(event: FormEvent) {
    event.preventDefault();
    if (name.trim() && !busy) onSubmit(name.trim(), slug.trim());
  }
  return <form className="form account-create-form" onSubmit={submit}>
    <label>{t(section === 'organizations' ? 'organization.displayName' : 'enterprise.name')}
      <input autoFocus required maxLength={128} value={name} onChange={(event) => setName(event.target.value)} disabled={busy} />
    </label>
    <label>{t(section === 'organizations' ? 'organization.namespace' : 'enterprise.slug')}
      <input maxLength={64} value={slug} onChange={(event) => setSlug(event.target.value.toLowerCase())} spellCheck={false} disabled={busy} aria-describedby="space-slug-hint" />
    </label>
    <p id="space-slug-hint" className="hint">{t(section === 'organizations' ? 'organization.createHint' : 'accountNav.enterpriseSlugHint')}</p>
    <div className="account-form-actions"><AppLink href={accountPath(section)}>{t('common.cancel')}</AppLink><button disabled={busy || !name.trim()}>{busy ? t('common.creating') : t('common.create')}</button></div>
    {error && <p className="err" role="alert">{error.message}</p>}
  </form>;
}

function CreateSpace({ section }: { section: Exclude<AccountSection, 'account'> }) {
  return section === 'organizations' ? <CreateOrganization /> : <CreateEnterprise />;
}

function CreateOrganization() {
  const create = useCreateOrganization();
  return <SpaceForm section="organizations" busy={create.isPending} error={create.error} onSubmit={(name, slug) => create.mutate({ name, slug }, { onSuccess: (organization) => navigate(`/${encodeURIComponent(organization.slug)}`) })} />;
}

function CreateEnterprise() {
  const client = useQueryClient();
  const create = useMutation({ mutationFn: ({ name, slug }: { name: string; slug: string }) => api.createEnterprise(name, slug), onSuccess: async (enterprise) => {
    await client.invalidateQueries({ queryKey: ['enterprises'] });
    navigate(enterprisePath(enterprise.slug));
  } });
  return <SpaceForm section="enterprises" busy={create.isPending} error={create.error} onSubmit={(name, slug) => create.mutate({ name, slug })} />;
}
