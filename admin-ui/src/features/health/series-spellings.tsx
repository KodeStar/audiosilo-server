import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Eye, EyeOff, Loader2, Merge } from 'lucide-react';
import { api } from '@/api/client';
import { useSeriesSpellings } from '@/api/hooks';
import type { AdminBook, SeriesSpellingGroup } from '@/api/types';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Button } from '@/components/ui/button';
import { NativeSelect } from '@/components/ui/native-select';
import { useMerge } from '@/features/library/people/use-merge';
import { toastError } from '@/lib/errors';
import { counted, formatList } from '@/lib/format';
import { seriesKey } from '@/lib/series-key';
import { AllClear } from './all-clear';
import { useIssueActions } from './use-issue-actions';

/**
 * Health > Series spelled several ways: one notice per series a library's books
 * spell more than one way, offering to set one spelling on every book of it (the
 * Authors page's merge: a bulk edit, files untouched), the Series page's name
 * first. Ignore stops a series being suggested; with `ignored`, those series, each
 * with Show again.
 */
export function SeriesSpellings({ ignored }: { ignored: boolean }) {
  const { t } = useTranslation();
  const groups = useSeriesSpellings(ignored);
  if (groups.isError) {
    return (
      <QueryError
        title={t('health.listError')}
        error={groups.error}
        onRetry={() => void groups.refetch()}
      />
    );
  }
  if (!groups.data) {
    return (
      <div className="skel h-[120px] rounded-xl" role="status" aria-label={t('common.loading')} />
    );
  }
  if (groups.data.length === 0) return <AllClear ignored={ignored} />;
  return (
    <div className="flex flex-col gap-3">
      {groups.data.map((g) => (
        <SpellingNotice
          key={`${g.library_id}:${seriesKey(g.suggested)}`}
          group={g}
          ignored={ignored}
        />
      ))}
    </div>
  );
}

/**
 * Every book of the group's library whose series is that series in some
 * spelling (its main series; the series filter also finds the books listing it
 * among their other series, which a rename doesn't touch).
 */
async function seriesBooks(g: SeriesSpellingGroup): Promise<AdminBook[]> {
  const key = seriesKey(g.suggested);
  const out: AdminBook[] = [];
  let cursor: string | undefined;
  do {
    const page = await api.adminBooks({
      series: g.suggested,
      library_id: g.library_id,
      limit: 200,
      cursor,
    });
    out.push(...page.books.filter((b) => b.series !== '' && seriesKey(b.series) === key));
    cursor = page.next_cursor || undefined;
  } while (cursor);
  return out;
}

function SpellingNotice({ group: g, ignored }: { group: SeriesSpellingGroup; ignored: boolean }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [keep, setKeep] = useState(g.suggested);
  const { merging, merge } = useMerge('series', g.library_id);
  const actions = useIssueActions('series_spellings');
  const [ignoring, setIgnoring] = useState(false);
  const names = g.spellings.map((s) => s.name);
  const chosen = names.includes(keep) ? keep : g.suggested;
  const others = names.filter((n) => n !== chosen);
  const rewrites = g.books - (g.spellings.find((s) => s.name === chosen)?.books ?? 0);
  const busy = merging !== null || ignoring;

  /** Ignores (or shows again) every book of the series under this issue. */
  const toggleIgnore = async () => {
    setIgnoring(true);
    try {
      const books = await seriesBooks(g);
      await (ignored ? actions.unignore(books) : actions.ignore(books));
    } catch (err) {
      toastError(t(ignored ? 'health.toast.unignoreFailed' : 'health.toast.ignoreFailed'), err);
    } finally {
      setIgnoring(false);
    }
  };

  return (
    <Notice
      tone="info"
      icon={Merge}
      title={t('people-merge.title', {
        count: others.length,
        names: formatList(
          others.map((name) => t('people-merge.quoted', { name })),
          lang,
        ),
        suggested: chosen,
      })}
      actions={
        <div className="flex flex-wrap items-center gap-2">
          {ignored ? (
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void toggleIgnore()}>
              <Eye aria-hidden="true" />
              {t('health.unignore')}
            </Button>
          ) : (
            <>
              <NativeSelect
                value={chosen}
                onChange={(e) => setKeep(e.target.value)}
                disabled={busy}
                aria-label={t('health.spellings.keep')}
                className="w-auto max-w-[260px]"
              >
                {names.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </NativeSelect>
              <Button
                size="sm"
                disabled={busy}
                onClick={() =>
                  void merge({ names, suggested: chosen, books: g.books, other_books: rewrites })
                }
              >
                {merging ? <Loader2 className="animate-spin" aria-hidden="true" /> : null}
                {merging ? t('people-merge.merging') : t('health.spellings.use')}
              </Button>
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => void toggleIgnore()}>
                <EyeOff aria-hidden="true" className="md:hidden" />
                <span className="max-md:sr-only">{t('health.ignore')}</span>
              </Button>
            </>
          )}
        </div>
      }
    >
      {t('people-merge.body', counted(rewrites, lang))}
    </Notice>
  );
}
