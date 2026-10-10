import { useState, type FormEvent } from 'react';
import { useCreateRepository } from '../hooks';
import { useT } from '../i18n';
import { navigate, repositoryPath } from '../route';
import type { User } from '../types';
import { AppLink } from './AppLink';
import { HeaderActions } from './HeaderActions';
import { Logo } from './Logo';

export function CreateRepositoryPage({ user }: { user: User }) {
  const t = useT();
  const create = useCreateRepository();
  const [name, setName] = useState('');
  const trimmedName = name.trim();
  const valid = trimmedName.length <= 64 && /^[A-Za-z]([A-Za-z0-9_-]*[A-Za-z0-9])?$/.test(trimmedName);
  const profilePath = `/${encodeURIComponent(user.username)}`;
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid || create.isPending) return;
    create.mutate(trimmedName, { onSuccess: (repository) => navigate(repositoryPath(repository)) });
  }
  return <div className="app">
    <header className="topbar">
      <AppLink href="/" className="linkish-logo" aria-label={t('common.home')}><div className="brand sm"><Logo /></div></AppLink>
      <div className="who"><HeaderActions user={user} /></div>
    </header>
    <main className="repository-create-page">
      <h1>{t('createMenu.repository')}</h1>
      <p className="repository-create-intro">{t('createMenu.repositoryHint')}</p>
      <form className="form account-create-form" onSubmit={submit}>
        <div className="repository-create-owner"><span>{t('createMenu.personalAccount')}</span><strong>@{user.username}</strong></div>
        <label>{t('dashboard.newWsAria')}
          <input autoFocus required maxLength={64} value={name} onChange={(event) => setName(event.target.value)} placeholder="my-project" spellCheck={false} autoComplete="off" disabled={create.isPending} aria-describedby="repository-name-rule" aria-invalid={Boolean(trimmedName && !valid)} />
        </label>
        <p className={`hint${trimmedName && !valid ? ' err' : ''}`} id="repository-name-rule">{t('dashboard.nameRule')}</p>
        {create.error && <p className="err" role="alert">{create.error.message}</p>}
        <div className="account-form-actions">
          <AppLink href={profilePath}>{t('common.cancel')}</AppLink>
          <button type="submit" disabled={!valid || create.isPending}>{t(create.isPending ? 'common.creating' : 'common.create')}</button>
        </div>
      </form>
    </main>
  </div>;
}
