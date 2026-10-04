import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { KeyRound, Smartphone, UserPlus, Users } from 'lucide-react';
import { useDevices, useLiveSessions, useStats, useUsers } from '@/api/hooks';
import type { Device, ListeningRow, ListeningSession, User } from '@/api/types';
import { AvatarRing } from '@/components/avatar-ring';
import { BookCover } from '@/components/book-cover';
import { ProgressBar } from '@/components/progress-bar';
import { useSearchDialog } from '@/lib/use-search-dialog';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { formatPercent, formatRelative, progressFraction } from '@/lib/format';
import { cn } from '@/lib/utils';
import { InviteDialog } from './invite-dialog';
import { currentBook, groupDevices } from './people-model';

const MONTH_MS = 30 * 24 * 3600 * 1000;

/** People > People: everyone with an account, as cards with what they're listening to. */
export function PeoplePage() {
  const { t } = useTranslation();
  const users = useUsers();
  const stats = useStats();
  // A badge here, not the live list: poll at the stats' pace.
  const live = useLiveSessions(30_000);
  const devices = useDevices();
  const devicesOf = groupDevices(devices.data);
  const [inviting, setInviting] = useSearchDialog('invite');
  const now = Date.now();

  const list = users.data ?? [];
  const active = list.filter(
    (u) => u.last_seen_at && now - Date.parse(u.last_seen_at) <= MONTH_MS,
  ).length;

  const inviteButton = (
    <Button onClick={() => setInviting(true)}>
      <UserPlus aria-hidden="true" />
      {t('people.invite')}
    </Button>
  );

  return (
    <Page>
      <PageHead
        title={t('people.title')}
        description={
          users.data ? t('people.description', { count: list.length, active }) : undefined
        }
        action={inviteButton}
      />
      {users.isError ? (
        <QueryError
          title={t('people.error')}
          error={users.error}
          onRetry={() => void users.refetch()}
        />
      ) : !users.data ? (
        <div
          className="grid gap-4 md:grid-cols-2 xl:grid-cols-3"
          role="status"
          aria-label={t('common.loading')}
        >
          {[0, 1, 2].map((i) => (
            <div key={i} className="skel h-[168px] rounded-xl" />
          ))}
        </div>
      ) : list.length === 0 ? (
        <EmptyState
          icon={Users}
          title={t('people.empty.title')}
          body={t('people.empty.body')}
          action={inviteButton}
        />
      ) : (
        <ul className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((u) => (
            <li key={u.id} className="min-w-0">
              <PersonCard
                user={u}
                listening={stats.data?.listening ?? []}
                live={live.data ?? []}
                devices={devicesOf && (devicesOf.get(u.id) ?? [])}
                now={now}
              />
            </li>
          ))}
        </ul>
      )}
      <InviteDialog open={inviting} onOpenChange={setInviting} />
    </Page>
  );
}

function PersonCard({
  user: u,
  listening,
  live,
  devices,
  now,
}: {
  user: User;
  listening: ListeningRow[];
  live: ListeningSession[];
  /** The person's signed-in devices (undefined while loading). */
  devices: Device[] | undefined;
  now: number;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const current = currentBook(listening, live, u.id);
  const frac = current ? progressFraction(current.position, current.duration) : 0;

  const status = u.disabled
    ? t('people.card.disabled')
    : current?.live
      ? null
      : u.last_seen_at
        ? t('people.card.active', { time: formatRelative(u.last_seen_at, lang, now) })
        : t('people.card.never');

  return (
    <Link
      to="/people/user/$userId"
      params={{ userId: String(u.id) }}
      className={cn(
        'flex h-full flex-col rounded-xl border bg-card text-left transition-colors duration-(--dur-1) hover:border-border-strong',
        u.disabled && 'opacity-60',
      )}
    >
      <div className="flex items-start gap-3.5 p-5">
        {current && !u.disabled ? (
          <AvatarRing name={u.username} size={46} progress={frac} />
        ) : (
          <Monogram name={u.username} size={54} />
        )}
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <b className="truncate text-[15px]">{u.username}</b>
            {u.role === 'admin' ? <Badge variant="ink">{t('people.role.admin')}</Badge> : null}
            {u.is_demo ? <Badge variant="outline">{t('people.badge.demo')}</Badge> : null}
          </div>
          <span className="text-[12.5px] text-muted-foreground">
            {status ?? (
              <span className="inline-flex items-center gap-1.5">
                <span className="dot" data-tone="live" aria-hidden="true" />
                {t('people.card.listening')}
              </span>
            )}
          </span>
          <span className="inline-flex items-center gap-1.5 text-[12px] text-subtle-foreground">
            {u.has_password ? (
              <>
                <KeyRound className="size-3.5" aria-hidden="true" />
                {t('people.card.password')}
              </>
            ) : (
              <>
                <Smartphone className="size-3.5" aria-hidden="true" />
                {t('people.card.pairOnly')}
              </>
            )}
          </span>
        </div>
      </div>
      <div className="mt-auto flex min-h-[63px] items-center gap-3 border-t px-5 py-3">
        {current ? (
          <>
            <BookCover
              libraryId={current.library_id}
              path={current.path}
              title={current.title || current.path}
              className="w-[38px] shrink-0"
            />
            <div className="flex min-w-0 flex-1 flex-col gap-1">
              <span className="truncate text-[13px] font-semibold">
                {current.title || current.path}
              </span>
              <ProgressBar fraction={frac} />
            </div>
            <span className="text-[12px] text-muted-foreground tabular-nums">
              {formatPercent(frac, lang)}
            </span>
          </>
        ) : (
          <span className="text-[12.5px] text-muted-foreground">{t('people.card.nothing')}</span>
        )}
      </div>
      {devices ? (
        <div className="flex min-w-0 items-center gap-1.5 border-t px-5 py-2.5 text-[12px] text-muted-foreground">
          <Smartphone className="size-3.5 shrink-0" aria-hidden="true" />
          <span className="truncate">
            {devices.length
              ? t('people.card.devices', {
                  count: devices.length,
                  names: devices
                    .slice(0, 2)
                    .map((d) => d.name || t('live.unnamed'))
                    .join(', '),
                })
              : t('people.card.noDevices')}
          </span>
        </div>
      ) : null}
    </Link>
  );
}
