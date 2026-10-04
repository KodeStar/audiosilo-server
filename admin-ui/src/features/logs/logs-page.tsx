import { useLayoutEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Search, ShieldCheck } from 'lucide-react';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Input } from '@/components/ui/input';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { Switch } from '@/components/ui/switch';
import { formatClockTime } from '@/lib/format';
import { useDebounced } from '@/lib/use-debounced';
import {
  appendPage,
  EMPTY_TAIL,
  LOG_LEVELS,
  MAX_LINES,
  type LogLevelFilter,
  type Tail,
} from './logs-model';

/** How often the live tail asks for new lines. */
const TAIL_MS = 2000;

/**
 * The newest log lines for a filter, then every new one while `live`: each poll
 * asks only for lines after the last seq it saw and appends them (logs-model).
 * A filter change starts a fresh tail; leaving the page drops it.
 */
function useLogTail(level: LogLevelFilter, q: string, live: boolean) {
  const qc = useQueryClient();
  const key = keys.logs(level, q);
  return useQuery({
    queryKey: key,
    queryFn: async (): Promise<Tail> => {
      const prev = qc.getQueryData<Tail>(key);
      const page = await api.logs({
        level: level === 'all' ? undefined : level,
        q: q || undefined,
        after: prev?.lastSeq || undefined,
        limit: prev ? MAX_LINES : MAX_LINES / 2,
      });
      return appendPage(prev ?? EMPTY_TAIL, page, !prev);
    },
    refetchInterval: live ? TAIL_MS : false,
    gcTime: 0,
  });
}

/**
 * Server > Logs: what the server logged since it started (the newest lines it
 * keeps in memory), filtered by level and text, with a live tail. Secrets are
 * never shown: the server redacts them before a line reaches this page.
 */
export function LogsPage() {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [level, setLevel] = useState<LogLevelFilter>('all');
  const [search, setSearch] = useState('');
  const [live, setLive] = useState(true);
  const q = useDebounced(search.trim(), 300);
  const tail = useLogTail(level, q, live);
  const lines = tail.data?.lines ?? [];

  // Follow new lines while the view is at the bottom; stay put once scrolled up.
  const box = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  useLayoutEffect(() => {
    const el = box.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines.length]);

  return (
    <Page>
      <PageHead title={t('logs.title')} description={t('logs.description')} />
      <section className="flex flex-col gap-3" aria-label={t('logs.title')}>
        <div className="flex flex-wrap items-center gap-2.5">
          <SegmentedControl
            label={t('logs.level')}
            value={level}
            onChange={setLevel}
            options={LOG_LEVELS.map((l) => ({ value: l, label: t(`logs.levels.${l}`) }))}
          />
          <label className="relative min-w-[180px] flex-1 sm:max-w-[320px]">
            <Search
              className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden="true"
            />
            <Input
              type="search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder={t('logs.search')}
              aria-label={t('logs.search')}
              className="w-full pl-9"
            />
          </label>
          <label className="ml-auto flex items-center gap-2 text-[13px] font-semibold">
            <Switch checked={live} onCheckedChange={setLive} aria-label={t('logs.live')} />
            {t('logs.live')}
          </label>
        </div>

        {tail.isError ? (
          <QueryError
            title={t('logs.error')}
            error={tail.error}
            onRetry={() => void tail.refetch()}
          />
        ) : (
          <div
            ref={box}
            className="log"
            role="log"
            aria-live="off"
            aria-busy={tail.isPending}
            tabIndex={0}
            aria-label={t('logs.panel')}
            onScroll={(e) => {
              const el = e.currentTarget;
              stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
            }}
          >
            {tail.isPending ? (
              <span className="attr">{t('common.loading')}</span>
            ) : lines.length === 0 ? (
              <span className="attr">
                {q || level !== 'all' ? t('logs.emptyFiltered') : t('logs.empty')}
              </span>
            ) : (
              <>
                {tail.data?.gap ? <div className="attr">{t('logs.gap')}</div> : null}
                {lines.map((l) => (
                  <div key={l.seq}>
                    <span className="ts">{formatClockTime(l.time, lang)}</span>
                    <span className="lv" data-level={l.level}>
                      {t(`logs.lvl.${l.level}`)}
                    </span>{' '}
                    {l.message}
                    {l.attrs.length ? (
                      <span className="attr">
                        {' '}
                        {l.attrs.map((a) => `${a.key}=${a.value}`).join(' ')}
                      </span>
                    ) : null}
                  </div>
                ))}
              </>
            )}
          </div>
        )}
        <Notice tone="info" icon={ShieldCheck}>
          {t('logs.note')}
        </Notice>
      </section>
    </Page>
  );
}
