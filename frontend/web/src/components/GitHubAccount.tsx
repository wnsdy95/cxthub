import { useMutation } from '@tanstack/react-query';
import { firebaseLinkGitHub, githubLoginEnabled } from '../auth';
import { useT } from '../i18n';

export function GitHubAccount({ uid }: { uid: string }) {
  const t = useT();
  const link = useMutation({ mutationFn: () => firebaseLinkGitHub(uid, t) });
  if (!githubLoginEnabled) return null;
  return <section>
    <h4>GitHub</h4>
    <p className="hint">{t('auth.linkGitHubHint')}</p>
    <button type="button" className="ghost" disabled={link.isPending} onClick={() => link.mutate()}>{t('auth.linkGitHub')}</button>
    {link.isSuccess && <p role="status">{t('auth.linkedGitHub')}</p>}
    {link.error && <p className="err" role="alert">{link.error.message}</p>}
  </section>;
}
