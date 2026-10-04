import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { History, X } from 'lucide-react';
import { useLibraries, useSessions, useUsers } from '@/api/hooks';
import type { ListeningSession } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { PlaybackStatus } from '@/components/playback-status';
import { QueryError } from '@/components/query-error';
import { Button } from '@/components/ui/button';
import { NativeSelect } from '@/components/ui/native-select';
import { bookRoute } from '@/lib/book-route';
import {
  formatDateTime,
  formatDuration,
  formatPercent,
  formatRelative,
  progressFraction,
} from '@/lib/format';
import { useClientName } from './use-client-name';

interface SessionsSearch {
  person?: number;
  library?: number;
  path?: string;
}

/** Activity > Sessions: every listening session, newest first, for everyone, a person or a book. */
export function SessionsPage() {
  const { t } = useTranslation();
  const search = useSearch({ strict: false }) as SessionsSearch;
  const navigate = useNavigate();
  const users = useUsers();
  const libraries = useLibraries();
  const filter = {
    user_id: search.person,
    library_id: search.library,
    path: search.library ? search.path : undefined,
  };
  const sessions = useSessions(filter);
  const rows = sessions.data?.pages.flatMap((p) => p.sessions ?? []) ?? [];
  const setSearch = (next: SessionsSearch) =>
    void navigate({ to: '.', search: next, replace: true });

  const library = libraries.data?.find((l) => l.id === search.library);
  const scopeLabel = filter.path
    ? rows[0]?.title || filter.path.split('/').pop() || filter.path
    : library?.name;

  return (
    <Page>
      <PageHead title={t('sessions.title')} description={t('sessions.description')} />
      <div className="mb-4 flex flex-wrap items-center gap-2.5">
        <NativeSelect
          className="w-[220px]"
          aria-label={t('sessions.filter.person')}
          value={search.person ?? ''}
          onChange={(e) =>
            setSearch({ ...search, person: e.target.value ? Number(e.target.value) : undefined })
          }
        >
          <option value="">{t('sessions.filter.everyone')}</option>
          {(users.data ?? []).map((u) => (
            <option key={u.id} value={u.id}>
              {u.username}
            </option>
          ))}
        </NativeSelect>
        {search.library ? (
          <span className="inline-flex h-[30px] max-w-full items-center gap-1.5 rounded-full border bg-card pr-1 pl-3 text-[12.5px] font-semibold">
            <span className="truncate">
              {filter.path
                ? t('sessions.filter.book', { book: scopeLabel ?? '' })
                : t('sessions.filter.library', { library: scopeLabel ?? '' })}
            </span>
            <button
              type="button"
              className="grid size-6 place-items-center rounded-full text-muted-foreground hover:bg-accent"
              aria-label={t('sessions.filter.clear')}
              onClick={() => setSearch({ person: search.person })}
            >
              <X className="size-3.5" aria-hidden="true" />
            </button>
          </span>
        ) : null}
      </div>
      {sessions.isError ? (
        <QueryError
          title={t('sessions.error')}
          error={sessions.error}
          onRetry={() => void sessions.refetch()}
        />
      ) : !sessions.data ? (
        <div className="flex flex-col gap-2" role="status" aria-label={t('common.loading')}>
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="skel h-12" />
          ))}
        </div>
      ) : rows.length === 0 ? (
        <EmptyState
          icon={History}
          title={t('sessions.empty.title')}
          body={t('sessions.empty.body')}
        />
      ) : (
        <>
          <SessionTable sessions={rows} showPerson={!search.person} />
          {sessions.hasNextPage ? (
            <div className="mt-4 flex justify-center">
              <Button
                variant="outline"
                onClick={() => void sessions.fetchNextPage()}
                disabled={sessions.isFetchingNextPage}
              >
                {t('sessions.more')}
              </Button>
            </div>
          ) : null}
        </>
      )}
    </Page>
  );
}

/** Sessions as a table: person, book, when, time listened, device and app, playback. */
export function SessionTable({
  sessions,
  showPerson = true,
}: {
  sessions: ListeningSession[];
  showPerson?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const clientName = useClientName();
  return (
    <div className="overflow-x-auto rounded-xl border bg-card">
      <table className="w-full text-left text-[13.5px]">
        <thead className="border-b text-[12px] text-muted-foreground">
          <tr>
            {showPerson ? (
              <th className="px-4 py-2.5 font-medium max-md:hidden">{t('sessions.col.person')}</th>
            ) : null}
            <th className="px-4 py-2.5 font-medium">{t('sessions.col.book')}</th>
            <th className="px-4 py-2.5 font-medium max-md:hidden">{t('sessions.col.when')}</th>
            <th className="px-4 py-2.5 text-right font-medium max-md:hidden">
              {t('sessions.col.listened')}
            </th>
            <th className="px-4 py-2.5 font-medium max-lg:hidden">{t('sessions.col.device')}</th>
            <th className="px-4 py-2.5 font-medium">{t('sessions.col.playback')}</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {sessions.map((s) => {
            const title = s.title || s.path;
            const from = progressFraction(s.start_position, s.duration);
            const to = s.finished ? 1 : progressFraction(s.position, s.duration);
            const listened = formatDuration(s.listened, lang);
            return (
              <tr key={s.id} className="hover:bg-muted/70">
                {showPerson ? (
                  <td className="px-4 py-2.5 max-md:hidden">
                    <Link
                      to="/people/user/$userId"
                      params={{ userId: String(s.user_id) }}
                      className="flex items-center gap-2 font-semibold hover:underline"
                    >
                      <Monogram name={s.username} size={24} />
                      <span className="truncate">{s.username}</span>
                    </Link>
                  </td>
                ) : null}
                <td className="px-4 py-2.5">
                  <Link
                    {...bookRoute(s.library_id, s.path)}
                    className="flex min-w-0 items-center gap-2.5"
                  >
                    <BookCover
                      libraryId={s.library_id}
                      path={s.path}
                      title={title}
                      size={160}
                      className="w-[30px] shrink-0 rounded-[3px]"
                    />
                    <span className="flex min-w-0 flex-col">
                      <span className="max-w-[300px] truncate font-semibold hover:underline">
                        {title}
                      </span>
                      <span className="text-[12px] text-muted-foreground tabular-nums">
                        {s.duration > 0
                          ? t('sessions.progress', {
                              from: formatPercent(from, lang),
                              to: formatPercent(to, lang),
                            })
                          : null}
                        <span className="md:hidden">
                          {[
                            showPerson ? s.username : '',
                            formatRelative(s.started_at, lang),
                            listened,
                          ]
                            .filter(Boolean)
                            .map((x) => ` · ${x}`)
                            .join('')}
                        </span>
                      </span>
                    </span>
                  </Link>
                </td>
                <td
                  className="px-4 py-2.5 whitespace-nowrap text-muted-foreground max-md:hidden"
                  title={formatDateTime(s.started_at, lang)}
                >
                  {formatRelative(s.started_at, lang)}
                </td>
                <td className="px-4 py-2.5 text-right tabular-nums max-md:hidden">{listened}</td>
                <td className="px-4 py-2.5 max-lg:hidden">
                  <span className="flex flex-col">
                    <span className="truncate">{s.device_name || t('live.unnamed')}</span>
                    <span className="truncate text-[12px] text-muted-foreground">
                      {clientName(s.client)}
                    </span>
                  </span>
                </td>
                <td className="px-4 py-2.5">
                  <PlaybackStatus direct={!s.transcoded} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
