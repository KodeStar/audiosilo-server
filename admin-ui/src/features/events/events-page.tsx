import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { BellOff } from 'lucide-react';
import { useServerEventList } from '@/api/hooks';
import type { ServerEventKind } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { ServerEventItem } from '@/components/server-event-item';
import { Button } from '@/components/ui/button';
import { NativeSelect } from '@/components/ui/native-select';
import { EVENT_KINDS } from '@/lib/server-events';

/**
 * Server > Events: everything the bell has shown, kept for 90 days, newest first,
 * narrowed to one kind. Each event leads to where it can be dealt with; which
 * of them are also sent out is Settings > Notifications.
 */
export function EventsPage() {
  const { t } = useTranslation();
  const [kind, setKind] = useState<ServerEventKind | ''>('');
  const list = useServerEventList(kind || undefined);
  const events = list.data?.pages.flatMap((p) => p.events) ?? [];

  return (
    <Page>
      <PageHead title={t('eventsPage.title')} description={t('eventsPage.description')} />
      <div className="flex flex-wrap items-center gap-2.5">
        <NativeSelect
          className="w-full sm:w-[240px]"
          value={kind}
          onChange={(e) => setKind(e.target.value as ServerEventKind | '')}
          aria-label={t('eventsPage.filter')}
        >
          <option value="">{t('eventsPage.allKinds')}</option>
          {EVENT_KINDS.map((k) => (
            <option key={k} value={k}>
              {t(`notify.event.${k}`)}
            </option>
          ))}
        </NativeSelect>
      </div>

      {list.isError ? (
        <QueryError
          title={t('eventsPage.error')}
          error={list.error}
          onRetry={() => void list.refetch()}
        />
      ) : list.isPending ? (
        <div className="skel h-[360px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : events.length === 0 ? (
        <EmptyState
          icon={BellOff}
          title={kind ? t('eventsPage.emptyFiltered.title') : t('eventsPage.empty.title')}
          body={kind ? t('eventsPage.emptyFiltered.body') : t('eventsPage.empty.body')}
        />
      ) : (
        <section
          className="rounded-xl border bg-card p-2"
          aria-label={t('eventsPage.title')}
          aria-busy={list.isFetching}
        >
          <ol className="flex flex-col">
            {events.map((ev) => (
              <li key={ev.id}>
                <ServerEventItem event={ev} time="absolute" />
              </li>
            ))}
          </ol>
          {list.hasNextPage ? (
            <div className="mt-1 border-t px-3 pt-2.5 pb-1 text-center">
              <Button
                variant="ghost"
                size="sm"
                disabled={list.isFetchingNextPage}
                onClick={() => void list.fetchNextPage()}
              >
                {list.isFetchingNextPage ? t('common.loading') : t('eventsPage.older')}
              </Button>
            </div>
          ) : null}
        </section>
      )}
    </Page>
  );
}
