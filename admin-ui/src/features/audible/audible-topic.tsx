import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { Trans, useTranslation } from 'react-i18next';
import { BookCheck, Headphones } from 'lucide-react';
import { AUDIBLE_TITLE_STATES, type AudibleTitle, type AudibleTitleState } from '@/api/types';
import { LoadMore } from '@/components/load-more';
import { QueryError } from '@/components/query-error';
import { SearchBox } from '@/components/search-box';
import { StatusText } from '@/components/status-text';
import { Badge } from '@/components/ui/badge';
import { Card, CardHeader } from '@/components/ui/card';
import { NativeSelect } from '@/components/ui/native-select';
import { bookRoute } from '@/lib/book-route';
import { formatDay, formatHours, formatList } from '@/lib/format';
import { CardEmpty } from '@/features/settings/card-empty';
import {
  accountName,
  chosenAccount,
  importStatus,
  plannedLine,
  pollTarget,
  REASON_CONTESTED,
  reasonKey,
  TITLE_STATE_TONE,
  titleByline,
  titleName,
  type MatchedFilter,
} from './audible-model';
import { BackupAllButton } from './backup-all-button';
import { CheckNowButton } from './check-now-button';
import { RemoveAccountButton } from './remove-account-button';
import { TitleActions } from './title-actions';
import {
  useAudibleAccounts,
  useAudibleTitles,
  useBackups,
  useCheckNow,
  useTitlesAfterPolls,
  type Backups,
} from './use-audible';

/**
 * Settings > Audible (Bridge Phase 4a; a page of the audible module, listed
 * only while it is on): every title the Bridge's Audible accounts own, which are
 * on this server and which aren't, filtered by account, state and whether it is
 * here, searched by title, author or ASIN. Check now asks the Bridge to look at
 * an account's library again, and Remove takes an account the Bridge can no
 * longer remove off this server. Phase 4b: each title's backup and its actions
 * (Back up, Not this book, a destination), and Back up everything.
 */
export function AudibleTopic() {
  const { t } = useTranslation();
  const accounts = useAudibleAccounts();
  const list = accounts.data?.accounts ?? [];
  const [picked, setPicked] = useState('');
  const account = chosenAccount(list, picked);
  const [state, setState] = useState<AudibleTitleState | ''>('');
  const [matched, setMatched] = useState<MatchedFilter>('');
  const [q, setQ] = useState('');
  const titles = useAudibleTitles({ account, state, matched, q });
  useTitlesAfterPolls(accounts.data?.accounts);
  const { checkNow, busy } = useCheckNow();
  const backups = useBackups();
  const target = pollTarget(list, account);
  const names = new Map(list.map((a) => [a.id, accountName(t, a)] as const));
  const templates = new Map(list.map((a) => [a.id, a.template] as const));
  const paused = new Set(list.filter((a) => a.paused).map((a) => a.id));
  const filtered = Boolean(account || state || matched || q);

  if (accounts.isError)
    return (
      <QueryError
        title={t('audible.accounts.error')}
        error={accounts.error}
        onRetry={() => void accounts.refetch()}
      />
    );

  return (
    <Card aria-labelledby="audible-titles-title">
      <CardHeader
        titleId="audible-titles-title"
        title={t('audible.titles.title')}
        description={t('audible.titles.description')}
        action={
          target ? (
            <div className="flex flex-wrap items-center gap-2">
              <CheckNowButton
                busy={busy === target.id}
                paused={target.paused}
                onCheck={() => void checkNow(target)}
              />
              <BackupAllButton account={target} backups={backups} />
              <RemoveAccountButton account={target} />
            </div>
          ) : undefined
        }
      />
      <div className="flex flex-wrap items-center gap-2 border-t px-5 py-3">
        <SearchBox
          value={q}
          onChange={setQ}
          label={t('audible.titles.search')}
          placeholder={t('audible.titles.searchPlaceholder')}
          clearLabel={t('audible.titles.clearSearch')}
          className="min-w-[200px] flex-1"
        />
        {list.length > 1 ? (
          <NativeSelect
            className="w-[180px]"
            value={account}
            onChange={(e) => setPicked(e.target.value)}
            aria-label={t('audible.titles.account')}
          >
            <option value="">{t('audible.titles.allAccounts')}</option>
            {list.map((a) => (
              <option key={a.id} value={a.id}>
                {accountName(t, a)}
              </option>
            ))}
          </NativeSelect>
        ) : null}
        <NativeSelect
          className="w-[160px]"
          value={state}
          onChange={(e) => setState(e.target.value as AudibleTitleState | '')}
          aria-label={t('audible.titles.state')}
        >
          <option value="">{t('audible.titles.allStates')}</option>
          {AUDIBLE_TITLE_STATES.map((s) => (
            <option key={s} value={s}>
              {t(`audible.state.${s}`)}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect
          className="w-[170px]"
          value={matched}
          onChange={(e) => setMatched(e.target.value as MatchedFilter)}
          aria-label={t('audible.titles.onServer')}
        >
          <option value="">{t('audible.titles.anyServer')}</option>
          <option value="matched">{t('audible.titles.matched')}</option>
          <option value="missing">{t('audible.titles.missing')}</option>
        </NativeSelect>
      </div>
      <p className="border-t px-5 py-2.5 text-[12.5px] text-muted-foreground">
        {t('audible.legal')}
      </p>
      {titles.isError ? (
        <div className="p-5">
          <QueryError
            title={t('audible.titles.error')}
            error={titles.error}
            onRetry={() => void titles.refetch()}
          />
        </div>
      ) : titles.isPending || accounts.isPending ? (
        <div className="p-5">
          <div className="skel h-24 rounded-lg" role="status" aria-label={t('common.loading')} />
        </div>
      ) : titles.rows.length === 0 ? (
        <CardEmpty
          icon={Headphones}
          title={
            filtered
              ? t('audible.titles.noneFiltered')
              : list.length
                ? t('audible.titles.none')
                : t('audible.accounts.none')
          }
          body={
            filtered
              ? t('audible.titles.noneFilteredBody')
              : list.length
                ? t('audible.titles.noneBody')
                : t('audible.accounts.noneBody')
          }
        />
      ) : (
        <ul className="flex flex-col divide-y" aria-label={t('audible.titles.title')}>
          {titles.rows.map((x) => (
            <TitleRow
              key={`${x.account}/${x.asin}`}
              title={x}
              account={list.length > 1 ? names.get(x.account) : undefined}
              template={templates.get(x.account)}
              paused={paused.has(x.account)}
              backups={backups}
            />
          ))}
        </ul>
      )}
      {titles.hasMore ? (
        <div className="border-t px-5 py-3">
          <LoadMore
            onMore={() => void titles.fetchMore()}
            loading={titles.isFetchingMore}
            loaded={titles.pageCount}
          />
        </div>
      ) : null}
    </Card>
  );
}

function TitleRow({
  title: x,
  account,
  template,
  paused,
  backups,
}: {
  title: AudibleTitle;
  account?: string;
  /** The account's placement template now (the plan notes when it used another). */
  template?: string;
  /** The title's account is paused (its library was deleted): nothing can be backed up. */
  paused: boolean;
  backups: Backups;
}) {
  const { t, i18n } = useTranslation();
  const backup = importStatus(x);
  const lang = i18n.resolvedLanguage ?? 'en';
  const byline = titleByline(x);
  const details = [
    x.runtime_s > 0 ? formatHours(x.runtime_s, lang) : '',
    x.state === 'preorder' && x.release_date
      ? t('audible.titles.releases', {
          date: formatDay(x.release_date, lang, {
            day: 'numeric',
            month: 'short',
            year: 'numeric',
          }),
        })
      : '',
    x.asin,
    account ?? '',
    x.dest_path && (!x.match || x.rejected) ? t('audible.dest.to', { path: x.dest_path }) : '',
  ].filter(Boolean);
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-1.5 px-5 py-3">
      <div className="flex min-w-0 flex-1 basis-64 flex-col gap-0.5">
        <b className="font-semibold [overflow-wrap:anywhere]">{titleName(x)}</b>
        {byline ? <span className="text-[13px] text-muted-foreground">{byline}</span> : null}
        <span className="font-mono text-[12px] text-muted-foreground">{details.join(' · ')}</span>
        <PlannedNote title={x} template={template} />
        <ReasonNote title={x} />
        {backup?.code ? (
          <span className="text-[12.5px] text-muted-foreground">{t(backup.code)}</span>
        ) : null}
        <TitleActions title={x} paused={paused} backups={backups} />
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <StatusText
          tone={TITLE_STATE_TONE[x.state]}
          colored
          className="text-[12.5px] font-semibold"
        >
          {t(`audible.state.${x.state}`)}
        </StatusText>
        {x.match ? (
          <Link
            {...bookRoute(x.match.library_id, x.match.path)}
            className="inline-flex items-center gap-1 text-[12.5px] font-semibold text-primary hover:underline"
          >
            <BookCheck className="size-3.5" aria-hidden="true" />
            {t('audible.titles.onThisServer')}
          </Link>
        ) : (
          <Badge variant="outline">{t('audible.titles.notHere')}</Badge>
        )}
        {x.match?.how === 'fuzzy' ? (
          <Badge variant="outline">{t('audible.titles.fuzzy')}</Badge>
        ) : null}
        {x.match?.how === 'import' ? (
          <Badge variant="outline">{t('audible.titles.backedUp')}</Badge>
        ) : null}
        {x.rejected ? <Badge variant="outline">{t('audible.titles.rejected')}</Badge> : null}
        {backup ? (
          <StatusText tone={backup.tone} colored className="text-[12.5px] font-semibold">
            {t(backup.key)}
          </StatusText>
        ) : null}
      </div>
    </li>
  );
}

/**
 * Where an unmatched title's backup would go, as the Bridge planned it at its
 * last check: a warning when the place is taken or the plan failed, a note
 * when the account's template changed since. With a destination of the
 * admin's own, that is checked when the backup starts instead.
 */
function PlannedNote({ title: x, template }: { title: AudibleTitle; template?: string }) {
  const { t } = useTranslation();
  if (x.dest_path && (!x.match || x.rejected))
    return (
      <span className="text-[12.5px] text-muted-foreground">{t('audible.planned.override')}</span>
    );
  const line = plannedLine(x, template);
  if (!line) return null;
  const text = (
    <span className="[overflow-wrap:anywhere]">
      {t(line.key, line.values)}{' '}
      <span className="text-muted-foreground">({t('audible.planned.when')})</span>
    </span>
  );
  return (
    <span className="flex flex-col gap-0.5 text-[12.5px]">
      {line.warn ? (
        <StatusText tone="warn" colored>
          {text}
        </StatusText>
      ) : (
        <span className="text-muted-foreground">{text}</span>
      )}
      {line.stale ? (
        <span className="text-muted-foreground">{t('audible.planned.staleTemplate')}</span>
      ) : null}
    </span>
  );
}

/**
 * Why a title isn't shown as here, in words: contested names the book it tied
 * for and the account's other titles on it (or says so plainly without them),
 * an unknown code reads as a sentence, and Audible's own words stay.
 */
function ReasonNote({ title: x }: { title: AudibleTitle }) {
  const { t, i18n } = useTranslation();
  if (!x.reason) return null;
  const cls = 'text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]';
  if (x.reason === REASON_CONTESTED) {
    const c = x.contested;
    if (!c || c.with.length === 0)
      return <span className={cls}>{t('audible.reason.contestedUnknown')}</span>;
    return (
      <span className={cls}>
        <Trans
          i18nKey="audible.reason.contested"
          values={{
            with: formatList(
              c.with.map((w) => w.title || w.asin),
              i18n.resolvedLanguage ?? 'en',
            ),
            path: c.path,
          }}
          components={{
            book: (
              <Link
                {...bookRoute(c.library_id, c.path)}
                className="font-semibold text-primary hover:underline"
              />
            ),
          }}
        />
      </span>
    );
  }
  const key = reasonKey(x.reason);
  return <span className={cls}>{key ? t(key) : x.reason}</span>;
}
