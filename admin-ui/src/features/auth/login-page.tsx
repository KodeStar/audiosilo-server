import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, LoaderCircle } from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { clearToken, setToken } from '@/api/token';
import { LogoTile } from '@/components/logo';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { LanguageSelect } from '@/components/language-select';
import { useSession, type SignedOutReason } from '@/lib/session';

/**
 * Admin sign-in. Shown in place of the console whenever there is no valid
 * admin session, so the URL the admin asked for is kept and opens after signing in.
 */
export function LoginPage({ reason }: { reason: SignedOutReason }) {
  const { t } = useTranslation();
  const { signIn } = useSession();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(reason ? t(`auth.reason.${reason}`) : null);
  const errorId = useId();

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await api.login(username, password);
      if (res.user.role !== 'admin') {
        // Don't leave a session behind for an account that can't use the console.
        setToken(res.token);
        await api.logout().catch(() => {});
        clearToken();
        setError(t('login.notAdmin'));
        return;
      }
      signIn(res.token, res.user);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) setError(t('auth.badCredentials'));
      else if (err instanceof ApiError && err.status === 429) setError(t('auth.rateLimited'));
      else if (err instanceof ApiError) setError(err.message);
      else setError(t('auth.unreachable'));
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="grid min-h-dvh place-items-center px-4 py-10">
      <div className="w-full max-w-[400px]">
        <div className="mb-6 flex items-center gap-2.5">
          <LogoTile />
          <span className="font-display text-base font-bold tracking-[-0.02em]">AudioSilo</span>
        </div>
        <div className="rounded-xl border bg-card p-6 sm:p-8">
          <h1 className="text-[26px] font-bold tracking-[-0.03em]">{t('login.title')}</h1>
          <p className="mt-1.5 text-muted-foreground">{t('login.intro')}</p>
          <form className="mt-6 flex flex-col gap-4" onSubmit={onSubmit} noValidate>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="login-username">{t('login.username')}</Label>
              <Input
                id="login-username"
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                required
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? errorId : undefined}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="login-password">{t('login.password')}</Label>
              <Input
                id="login-password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? errorId : undefined}
              />
            </div>
            {error ? (
              <p id={errorId} role="alert" className="text-[13px] font-medium text-destructive">
                {error}
              </p>
            ) : null}
            <Button type="submit" size="lg" className="mt-1 w-full" disabled={busy || !username}>
              {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
              {t('login.submit')}
            </Button>
          </form>
        </div>
        <div className="mt-5 flex flex-wrap items-center justify-between gap-3 text-[13px]">
          <a
            href="/"
            className="inline-flex items-center gap-1.5 font-medium text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="size-3.5" aria-hidden="true" />
            {t('login.back')}
          </a>
          <LanguageSelect />
        </div>
      </div>
    </main>
  );
}
