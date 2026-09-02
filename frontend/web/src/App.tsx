import { useEffect, useRef, useState } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { wsPath, parseRoute, replacePath, findByRoute, type WsTab } from './route';
import { useMe, useAcceptInvite, useWorkspaces } from './hooks';
import { useLocale, useT } from './i18n';
import { Login } from './components/Login';
import { Dashboard } from './components/Dashboard';
import { PublicBrowse } from './components/PublicBrowse';
import { UserProfile } from './components/UserProfile';
import { Landing } from './components/Landing';
import { Pricing } from './components/Pricing';
import { DeviceApprove } from './components/DeviceApprove';

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 30_000, refetchOnWindowFocus: false } },
});

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <Root />
    </QueryClientProvider>
  );
}

function Root() {
  // Cookie session validity is determined by the me query (token is unreadable by JS).
  const me = useMe();
  const t = useT();
  const authed = Boolean(me.data);
  const accept = useAcceptInvite();

  // On login, apply the UI language stored in the account to the user once (device consistency). Subsequent switch changes are not overridden.
  const [uiLocale, setUiLocale] = useLocale();
  const localeAppliedFor = useRef<string | null>(null);
  useEffect(() => {
    const u = me.data;
    if (!u || localeAppliedFor.current === u.id) return;
    localeAppliedFor.current = u.id;
    if ((u.locale === 'ko' || u.locale === 'en') && u.locale !== uiLocale) setUiLocale(u.locale);
  }, [me.data, uiLocale, setUiLocale]);
  const [notice, setNotice] = useState<string | null>(null);
  const [forceLogin, setForceLogin] = useState(false); // On 'Login' click in public view

  // When route type changes (workspace ↔ profile, etc.), the Root is re-rendered to dispatch the correct component. Since navigate() synthesizes a popstate event, we subscribe here to re-evaluate on path changes.
  const [, routeTick] = useState(0);
  useEffect(() => {
    const onNav = () => routeTick((n) => n + 1);
    window.addEventListener('popstate', onNav);
    return () => window.removeEventListener('popstate', onNav);
  }, []);

  // Invite link (/invite/<token>): Automatically accept if logged in.
  useEffect(() => {
    if (!authed) return;
    const r = parseRoute();
    if (r?.kind !== 'invite') return;
    accept.mutate(r.token, {
      onSuccess: (w) => {
        setNotice(t('app.joinedWorkspace', { name: w.name }));
        replacePath(wsPath(w)); // Redirect to the joined workspace path (/<owner>/<slug>)
      },
      onError: (x) => setNotice(t('app.acceptFailed', { msg: x.message })),
    });
    // accept is not a stable reference, so it only reacts to intentional authed changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [authed]);

  // Boot loader only during initial session check. Without isFetched, background re-fetches (status='pending' on no data) will unmount the entire subtree, causing a remount loop.
  if (me.isLoading && !me.isFetched) return <div className="loading">…</div>;
  if (!authed) {
    // Non-logged in + /<username>/<slug> → public workspace: read-only view (determined by server). /login/device re-renders to approval page after login.
    const r = parseRoute();
    if (!forceLogin && r?.kind === 'ws') {
      return <PublicBrowse username={r.username} slug={r.slug} tab={r.tab} onLogin={() => setForceLogin(true)} />;
    }
    if (!forceLogin && r?.kind === 'user') {
      return <UserProfile username={r.username} onLogin={() => setForceLogin(true)} />;
    }
    if (!forceLogin && r?.kind === 'pricing') {
      return <Pricing onSignIn={() => setForceLogin(true)} />;
    }
    if (!forceLogin && r === null) {
      // Home (/) — non-logged in: landing (CLI installation + Sign in/up + feature description). Click Login.
      return <Landing onSignIn={() => setForceLogin(true)} />;
    }
    return <Login />;
  }
  {
    const r = parseRoute();
    if (r?.kind === 'device') return <DeviceApprove code={r.code} />;
    if (r?.kind === 'user') return <UserProfile username={r.username} />;
    if (r?.kind === 'pricing') return <Pricing />;
    if (r?.kind === 'ws') return <AuthenticatedWorkspace username={r.username} slug={r.slug} tab={r.tab} />;
    // Home (/) shows landing even in login state — clicking logo does not redirect to workspace. (Dashboard mounts only in workspace paths, so automatic redirects do not occur)
    if (r === null) return <Landing />;
  }
  return (
    <>
      <Dashboard />
      {notice && (
        <div className="toast" role="status" onClick={() => setNotice(null)} title={t('app.dismissToast')}>
          {notice}
        </div>
      )}
    </>
  );
}

function AuthenticatedWorkspace({ username, slug, tab }: { username: string; slug: string; tab?: WsTab }) {
  const workspaces = useWorkspaces();
  if (workspaces.isLoading) return <div className="loading">…</div>;
  if (workspaces.isError) return <Dashboard />;

  const route = { kind: 'ws' as const, username, slug, tab };
  if (findByRoute(route, workspaces.data ?? [])) return <Dashboard />;

  // A signed-in non-member still uses the public read-only surface. Routing
  // every authenticated workspace URL through Dashboard would redirect away
  // before public visibility and access-denial rules can be evaluated.
  return <PublicBrowse username={username} slug={slug} tab={tab} />;
}
