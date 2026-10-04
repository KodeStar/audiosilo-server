import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Headphones, LibraryBig, Plus, Share2, Sparkles } from 'lucide-react';
import { useLibraries, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail, BookShare } from '@/api/types';
import { AvatarRing } from '@/components/avatar-ring';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { formatNumber, formatPercent, formatRelative, progressFraction } from '@/lib/format';
import { cn } from '@/lib/utils';
import { ProgressMenu } from '@/features/people/progress-actions';

/** The right-hand column: who's listening, community metadata, and who can see the book. */
export function BookAside({
  detail,
  matchBlocked,
  onMatch,
  onAddToShare,
}: {
  detail: AdminBookDetail;
  matchBlocked: boolean;
  onMatch: () => void;
  onAddToShare: () => void;
}) {
  return (
    <aside className="flex min-w-0 flex-col gap-4">
      <Listeners detail={detail} />
      <CommunityCard detail={detail} matchBlocked={matchBlocked} onMatch={onMatch} />
      <WhoCanSee detail={detail} onAddToShare={onAddToShare} />
    </aside>
  );
}

/** Everyone with progress on the book, each with the progress actions (mark finished, dates, sessions). */
function Listeners({ detail }: { detail: AdminBookDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const listeners = detail.listeners ?? [];
  const book = detail.book;
  return (
    <Card aria-labelledby="listeners-title">
      <CardHeader
        titleId="listeners-title"
        title={t('book.listeners.title')}
        action={
          <span className="text-muted-foreground tabular-nums">
            {formatNumber(listeners.length, lang)}
          </span>
        }
      />
      {listeners.length === 0 ? (
        <div className="flex flex-col items-center gap-2 px-5 py-7 text-center text-muted-foreground">
          <Headphones className="size-6" aria-hidden="true" />
          <p>{t('book.listeners.none')}</p>
        </div>
      ) : (
        <ul className="divide-y">
          {listeners.map((l) => {
            const progress = l.finished ? 1 : progressFraction(l.position, l.duration);
            const when = formatRelative(l.updated_at, lang);
            return (
              <li key={l.user_id} className="flex items-center gap-3 px-[18px] py-3">
                <AvatarRing name={l.username} size={38} progress={progress} />
                <div className="flex min-w-0 flex-1 flex-col">
                  <Link
                    to="/people/user/$userId"
                    params={{ userId: String(l.user_id) }}
                    className="truncate font-[650] hover:underline"
                  >
                    {l.username}
                  </Link>
                  <span className="text-[12.5px] text-muted-foreground tabular-nums">
                    {l.finished
                      ? t('book.listeners.finished', { time: when })
                      : t('book.listeners.progress', {
                          percent: formatPercent(progress, lang),
                          time: when,
                        })}
                  </span>
                </div>
                <ProgressMenu
                  target={{
                    library_id: book.library_id,
                    path: book.path,
                    userId: l.user_id,
                    username: l.username,
                    title: book.title || book.path,
                    finished: l.finished,
                    position: l.position,
                    started_at: l.started_at,
                    finished_at: l.finished_at,
                  }}
                />
              </li>
            );
          })}
        </ul>
      )}
    </Card>
  );
}

function CommunityCard({
  detail,
  matchBlocked,
  onMatch,
}: {
  detail: AdminBookDetail;
  matchBlocked: boolean;
  onMatch: () => void;
}) {
  const { t } = useTranslation();
  const server = useServerInfo();
  const asin = detail.fields.asin.value;
  const isbn = detail.fields.isbn.value;
  const matched = detail.book.matched;

  if (server.data && !server.data.capabilities.metadata) {
    return (
      <Card className="flex flex-col gap-2 border-dashed p-5" aria-labelledby="community-title">
        <h3 id="community-title" className="h3">
          {t('book.community.title')}
        </h3>
        <p className="text-[13px] text-muted-foreground">
          {t('book.community.off')}{' '}
          <Link
            to="/server/{-$section}"
            params={{ section: undefined }}
            className="font-semibold text-brand-ink hover:underline"
          >
            {t('book.community.offLink')}
          </Link>
        </p>
      </Card>
    );
  }
  if (!server.data) return null;

  return (
    <Card
      className={cn('flex flex-col gap-3 p-5', !matched && 'border-dashed')}
      aria-labelledby="community-title"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 id="community-title" className="h3">
          {t('book.community.title')}
        </h3>
        <span
          className={cn(
            'inline-flex items-center gap-1.5 text-[12.5px] font-semibold',
            matched ? 'text-success' : 'text-muted-foreground',
          )}
        >
          <span className="dot" data-tone={matched ? undefined : 'off'} aria-hidden="true" />
          {matched ? t('book.community.matched') : t('book.community.notMatched')}
        </span>
      </div>
      {matched ? (
        <>
          <dl className="grid grid-cols-[minmax(80px,auto)_1fr] gap-x-[18px] gap-y-2 text-[13px]">
            {asin ? (
              <>
                <dt className="text-muted-foreground">{t('book.field.asin')}</dt>
                <dd className="font-mono font-[550] [overflow-wrap:anywhere]">{asin}</dd>
              </>
            ) : null}
            {isbn ? (
              <>
                <dt className="text-muted-foreground">{t('book.field.isbn')}</dt>
                <dd className="font-mono font-[550] [overflow-wrap:anywhere]">{isbn}</dd>
              </>
            ) : null}
          </dl>
          <div>
            <Button variant="outline" size="sm" onClick={onMatch} disabled={matchBlocked}>
              {t('book.community.compare')}
            </Button>
          </div>
        </>
      ) : (
        <>
          <p className="text-[13px] text-muted-foreground">{t('book.community.pitch')}</p>
          <div>
            <Button size="sm" onClick={onMatch} disabled={matchBlocked}>
              <Sparkles aria-hidden="true" />
              {t('book.community.find')}
            </Button>
          </div>
        </>
      )}
    </Card>
  );
}

function WhoCanSee({
  detail,
  onAddToShare,
}: {
  detail: AdminBookDetail;
  onAddToShare: () => void;
}) {
  const { t } = useTranslation();
  const libraries = useLibraries().data ?? [];
  const shares = detail.shares ?? [];
  const label = (s: BookShare) => {
    if (s.whole_library_id !== undefined) {
      const name = libraries.find((l) => l.id === s.whole_library_id)?.name ?? s.name;
      return {
        title: t('book.access.library', { library: name }),
        sub: t('book.access.wholeLibrary'),
      };
    }
    return {
      title: s.name,
      sub: s.path ? t('book.access.via', { path: s.path }) : t('book.access.viaLibrary'),
    };
  };
  return (
    <Card aria-labelledby="access-title">
      <CardHeader titleId="access-title" title={t('book.access.title')} />
      {shares.length === 0 ? (
        <p className="px-5 py-5 text-[13px] text-muted-foreground">{t('book.access.none')}</p>
      ) : (
        <ul className="divide-y">
          {shares.map((s) => {
            const { title, sub } = label(s);
            const Icon = s.whole_library_id !== undefined ? LibraryBig : Share2;
            return (
              <li key={`${s.share_id}:${s.path}`}>
                <Link
                  to="/people/{-$section}"
                  params={{ section: 'shares' }}
                  search={{ share: s.share_id }}
                  className="flex items-center gap-3 px-[18px] py-[11px] hover:bg-muted/70"
                >
                  <span
                    className="grid size-[30px] shrink-0 place-items-center rounded-[9px] bg-muted text-muted-foreground"
                    aria-hidden="true"
                  >
                    <Icon className="size-[15px]" />
                  </span>
                  <span className="flex min-w-0 flex-col">
                    <b className="truncate font-semibold">{title}</b>
                    <span className="truncate text-[12px] text-muted-foreground">{sub}</span>
                  </span>
                </Link>
              </li>
            );
          })}
        </ul>
      )}
      <div className="border-t px-3 py-2">
        <Button variant="ghost" size="sm" onClick={onAddToShare}>
          <Plus aria-hidden="true" />
          {t('book.access.add')}
        </Button>
      </div>
    </Card>
  );
}
