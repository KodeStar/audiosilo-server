import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { History, KeyRound, Search, Server } from 'lucide-react';
import { useAudit, useUsers } from '@/api/hooks';
import type { AuditEvent, AuditFilter } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { formatDateTime, formatNumber } from '@/lib/format';
import { useDebounced } from '@/lib/use-debounced';
import { actionText, AUDIT_AREAS, detailLines } from './audit-model';

/**
 * Server > Audit log: what admins changed, newest first, narrowed by area,
 * person or text. Kept for a year; sign-ins and listening are in Activity, scans
 * in Health > Jobs.
 */
export function AuditPage() {
  const { t } = useTranslation();
  const [area, setArea] = useState('');
  const [actor, setActor] = useState('');
  const [search, setSearch] = useState('');
  const q = useDebounced(search.trim(), 300);
  const filter = useMemo<AuditFilter>(
    () => ({
      area: area || undefined,
      actor_id: actor ? Number(actor) : undefined,
      q: q || undefined,
    }),
    [area, actor, q],
  );
  const audit = useAudit(filter);
  const admins = (useUsers().data ?? []).filter((u) => u.role === 'admin');
  const events = audit.data?.pages.flatMap((p) => p.events) ?? [];
  const filtered = Boolean(area || actor || q);

  return (
    <Page>
      <PageHead title={t('audit.title')} description={t('audit.description')} />
      <div className="mb-4 flex flex-wrap items-center gap-2.5">
        <NativeSelect
          className="w-full sm:w-[190px]"
          value={area}
          onChange={(e) => setArea(e.target.value)}
          aria-label={t('audit.filter.area')}
        >
          <option value="">{t('audit.filter.allAreas')}</option>
          {AUDIT_AREAS.map((a) => (
            <option key={a} value={a}>
              {t(`audit.area.${a}`)}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect
          className="w-full sm:w-[190px]"
          value={actor}
          onChange={(e) => setActor(e.target.value)}
          aria-label={t('audit.filter.person')}
        >
          <option value="">{t('audit.filter.everyone')}</option>
          {admins.map((u) => (
            <option key={u.id} value={u.id}>
              {u.username}
            </option>
          ))}
        </NativeSelect>
        <label className="relative min-w-[180px] flex-1 sm:max-w-[320px]">
          <Search
            className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t('audit.filter.search')}
            aria-label={t('audit.filter.search')}
            className="w-full pl-9"
          />
        </label>
      </div>

      {audit.isError ? (
        <QueryError
          title={t('audit.error')}
          error={audit.error}
          onRetry={() => void audit.refetch()}
        />
      ) : audit.isPending ? (
        <div className="skel h-[360px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : events.length === 0 ? (
        <EmptyState
          icon={History}
          title={filtered ? t('audit.emptyFiltered.title') : t('audit.empty.title')}
          body={filtered ? t('audit.emptyFiltered.body') : t('audit.empty.body')}
        />
      ) : (
        <section
          className="rounded-xl border bg-card"
          aria-label={t('audit.title')}
          aria-busy={audit.isFetching}
        >
          <ol className="flex flex-col divide-y">
            {events.map((e) => (
              <AuditRow key={e.id} event={e} />
            ))}
          </ol>
          {audit.hasNextPage ? (
            <div className="border-t px-5 py-3 text-center">
              <Button
                variant="ghost"
                size="sm"
                disabled={audit.isFetchingNextPage}
                onClick={() => void audit.fetchNextPage()}
              >
                {audit.isFetchingNextPage ? t('common.loading') : t('audit.older')}
              </Button>
            </div>
          ) : null}
        </section>
      )}
    </Page>
  );
}

function AuditRow({ event: e }: { event: AuditEvent }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const lines = detailLines(e, t, {
    number: (n) => formatNumber(n, lang),
    date: (iso) => formatDateTime(iso, lang),
  });
  const system = e.via === 'system';
  return (
    <li className="grid grid-cols-1 gap-x-4 gap-y-1.5 px-5 py-3.5 md:grid-cols-[130px_170px_minmax(0,1fr)]">
      <time dateTime={e.at} className="text-[12.5px] text-muted-foreground tabular-nums md:pt-0.5">
        {formatDateTime(e.at, lang)}
      </time>
      <span className="flex min-w-0 items-center gap-2 text-[13px]">
        {system ? (
          <span
            className="grid size-[22px] shrink-0 place-items-center rounded-full bg-muted text-muted-foreground"
            aria-hidden="true"
          >
            <Server className="size-3" />
          </span>
        ) : (
          <Monogram name={e.actor_name || '?'} size={22} />
        )}
        <span className="truncate font-semibold">{system ? t('audit.system') : e.actor_name}</span>
        {e.via === 'api' ? (
          <KeyRound
            className="size-3.5 shrink-0 text-muted-foreground"
            aria-label={t('audit.viaKey')}
          />
        ) : null}
      </span>
      <div className="flex min-w-0 flex-col gap-0.5">
        <span className="[overflow-wrap:anywhere]">
          <b className="font-semibold">{actionText(e, t)}</b>
          {e.target ? <span className="text-muted-foreground"> · {e.target}</span> : null}
        </span>
        {lines.length ? (
          <dl className="flex flex-wrap gap-x-3 gap-y-0.5 text-[12.5px] text-muted-foreground">
            {lines.map((l, i) => (
              // Inline, so a long value wraps after its label instead of squeezing it.
              <div key={i} className="min-w-0 [overflow-wrap:anywhere]">
                <dt className="inline">{l.label}:</dt>{' '}
                <dd className="inline text-foreground/80">{l.value}</dd>
              </div>
            ))}
          </dl>
        ) : null}
      </div>
    </li>
  );
}
