import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  ArrowLeft,
  ArrowRight,
  CalendarDays,
  ChevronRight,
  History,
  Import as ImportIcon,
  LoaderCircle,
  TriangleAlert,
} from 'lucide-react';
import { ApiError, api } from '@/api/client';
import {
  importBusy,
  invalidateImported,
  keys,
  setImportRow,
  useImportDetail,
  useImports,
  useUsers,
} from '@/api/hooks';
import type { AbsUser, Import, ImportSummary, UnmatchedItem } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { Field, FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { RadioCards } from '@/components/ui/radio-cards';
import { describedBy } from '@/lib/a11y';
import { errorMessage } from '@/lib/errors';
import { counted, formatDate, formatHours, formatNumber, formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';
import { ImportRow, ImportStatusBadge } from './import-actions';
import { useDiscardImport } from './use-discard-import';
import {
  cutoffChanged,
  cutoffDay,
  cutoffText,
  cutoffValue,
  duplicateUsers,
  failureHintKey,
  importEmpty,
  initialMap,
  mappingBody,
  mappingProblem,
  matchTiers,
  matchedTotal,
  replacedBy,
  sourceHost,
  splitImports,
  unmatchedReasonKey,
  type CutoffChoice,
  type UserMap,
} from './imports-model';

/**
 * Settings > Import: brings a person's listening history over from
 * Audiobookshelf. Connect (address + token), choose who each account's history
 * goes to, then review each person's import (fetched in the background) and
 * apply it. Applied imports can be undone.
 */
export function ImportTopic() {
  const { t } = useTranslation();
  const imports = useImports();
  return (
    <>
      <Notice tone="info" icon={History} title={t('imports.intro.title')}>
        <p>{t('imports.intro.what')}</p>
        <p className="mt-1.5">{t('imports.intro.match')}</p>
      </Notice>
      <ConnectCard />
      {imports.isError ? (
        <QueryError
          title={t('imports.error')}
          error={imports.error}
          onRetry={() => void imports.refetch()}
        />
      ) : !imports.data ? (
        <div className="skel h-[160px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : (
        <ImportLists list={imports.data} />
      )}
    </>
  );
}

/** The connection the mapping step works with; the token lives only here, in memory. */
interface Connection {
  url: string;
  token: string;
  users: AbsUser[];
}

/** Steps 1 and 2: connect, then map accounts to people and start. */
function ConnectCard() {
  const { t } = useTranslation();
  const [conn, setConn] = useState<Connection | null>(null);
  // The address (never the token) comes back when going back a step.
  const [lastUrl, setLastUrl] = useState('');
  return (
    <Card aria-labelledby="import-connect-title">
      <CardHeader
        titleId="import-connect-title"
        title={conn ? t('imports.map.title') : t('imports.connect.title')}
        description={
          conn
            ? t('imports.map.description', { host: sourceHost(conn.url) })
            : t('imports.connect.description')
        }
        action={
          <span className="text-[12px] font-semibold text-muted-foreground">
            {t('imports.step', { n: conn ? 2 : 1, of: 3 })}
          </span>
        }
      />
      {conn ? (
        <MapStep
          conn={conn}
          onBack={() => {
            setLastUrl(conn.url);
            setConn(null);
          }}
          onStarted={() => {
            setLastUrl('');
            setConn(null);
          }}
        />
      ) : (
        <ConnectForm initialUrl={lastUrl} onConnected={setConn} />
      )}
    </Card>
  );
}

/** Where a connect refusal belongs: under the address, under the token, or the whole form. */
function connectErrorField(err: unknown): 'url' | 'token' | undefined {
  if (!(err instanceof ApiError)) return undefined;
  switch (err.code) {
    case 'invalid_url':
    case 'abs_unreachable':
    case 'not_abs':
      return 'url';
    case 'abs_unauthorized':
    case 'invalid_import': // on this step only the token can be malformed
      return 'token';
  }
  return undefined;
}

function ConnectForm({
  initialUrl,
  onConnected,
}: {
  initialUrl: string;
  onConnected: (c: Connection) => void;
}) {
  const { t } = useTranslation();
  const [url, setUrl] = useState(initialUrl);
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [errors, setErrors] = useState<{ url?: string; token?: string; form?: string }>({});

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy) return;
    const next: typeof errors = {};
    if (!url.trim()) next.url = t('imports.connect.urlRequired');
    if (!token.trim()) next.token = t('imports.connect.tokenRequired');
    setErrors(next);
    if (next.url || next.token) return;
    setBusy(true);
    try {
      const r = await api.absUsers(url.trim(), token.trim());
      onConnected({ url: url.trim(), token: token.trim(), users: r.users });
    } catch (err) {
      const field = connectErrorField(err);
      const message =
        err instanceof ApiError && err.code === 'invalid_import'
          ? t('imports.connect.tokenInvalid')
          : errorMessage(err, t);
      setErrors({ [field ?? 'form']: message });
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4 p-5">
      <Field
        htmlFor="abs-url"
        label={t('imports.connect.url')}
        error={errors.url}
        description={t('imports.connect.urlHint')}
      >
        <Input
          id="abs-url"
          type="url"
          inputMode="url"
          autoComplete="off"
          spellCheck={false}
          placeholder="https://abs.example.com"
          className="font-mono text-[12.5px]"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          aria-invalid={errors.url ? true : undefined}
          aria-describedby={describedBy('abs-url', !!errors.url, true)}
        />
      </Field>
      <Field
        htmlFor="abs-token"
        label={t('imports.connect.token')}
        error={errors.token}
        description={t('imports.connect.tokenHint')}
      >
        <Input
          id="abs-token"
          type="password"
          autoComplete="off"
          spellCheck={false}
          className="font-mono text-[12.5px]"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          aria-invalid={errors.token ? true : undefined}
          aria-describedby={describedBy('abs-token', !!errors.token, true)}
        />
      </Field>
      <p className="text-[12.5px] text-muted-foreground">{t('imports.connect.tokenSafe')}</p>
      <FormError>{errors.form}</FormError>
      <div className="flex justify-end">
        <Button type="submit" disabled={busy}>
          {busy ? (
            <LoaderCircle className="animate-spin" aria-hidden="true" />
          ) : (
            <ArrowRight aria-hidden="true" />
          )}
          {busy ? t('imports.connect.connecting') : t('imports.connect.submit')}
        </Button>
      </div>
    </form>
  );
}

function MapStep({
  conn,
  onBack,
  onStarted,
}: {
  conn: Connection;
  onBack: () => void;
  onStarted: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const users = useUsers();
  const list = users.data ?? [];
  const [map, setMap] = useState<UserMap | null>(null);
  const [choice, setChoice] = useState<CutoffChoice>('auto');
  const [day, setDay] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [tried, setTried] = useState(false);

  // The suggestions need the AudioSilo users: fill the map once they're here.
  const current =
    map ?? (users.data ? initialMap(conn.users, new Set(list.map((u) => u.id))) : null);
  const dup = current ? duplicateUsers(current) : new Set<number>();
  const problem = current ? mappingProblem(current) : undefined;
  const cutoff = cutoffValue(choice, day);
  const mapped = current ? mappingBody(current, conn.users).length : 0;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setTried(true);
    if (busy || !current || problem || cutoff === undefined) return;
    setBusy(true);
    setError(undefined);
    try {
      const r = await api.startAbsImport({
        url: conn.url,
        token: conn.token,
        users: mappingBody(current, conn.users),
        cutoff,
      });
      toast.add({
        title: t('imports.started', { count: r.imports.length }),
        description: t('imports.startedBody'),
        type: 'success',
      });
      void qc.invalidateQueries({ queryKey: keys.importLists });
      onStarted();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    }
  };

  if (users.isError)
    return (
      <div className="p-5">
        <QueryError
          title={t('imports.map.usersError')}
          error={users.error}
          onRetry={() => void users.refetch()}
        />
      </div>
    );
  if (!current)
    return (
      <div className="p-5">
        <div className="skel h-24" role="status" aria-label={t('common.loading')} />
      </div>
    );

  return (
    <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col">
      {conn.users.length === 0 ? (
        <p className="px-5 py-6 text-center text-muted-foreground">{t('imports.map.noUsers')}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[440px] text-[13px]">
            <caption className="sr-only">{t('imports.map.title')}</caption>
            <thead>
              <tr className="text-left text-[12px] text-muted-foreground">
                <th scope="col" className="px-5 py-2.5 font-semibold">
                  {t('imports.map.absColumn')}
                </th>
                <th scope="col" className="px-5 py-2.5 font-semibold">
                  {t('imports.map.userColumn')}
                </th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {conn.users.map((u) => {
                const value = current[u.abs_id];
                const clash = value != null && dup.has(value);
                const id = `abs-map-${u.abs_id}`;
                return (
                  <tr key={u.abs_id}>
                    <th scope="row" className="px-5 py-3 text-left font-normal">
                      <span className="flex flex-wrap items-center gap-2">
                        <b className="font-semibold [overflow-wrap:anywhere]">{u.username}</b>
                        {u.type ? <Badge variant="outline">{u.type}</Badge> : null}
                      </span>
                    </th>
                    <td className="px-5 py-3">
                      <NativeSelect
                        id={id}
                        className="max-w-[280px]"
                        aria-label={t('imports.map.selectAria', { name: u.username })}
                        aria-invalid={clash ? true : undefined}
                        aria-describedby={clash ? `${id}-error` : undefined}
                        value={value == null ? '' : String(value)}
                        onChange={(e) =>
                          setMap({
                            ...current,
                            [u.abs_id]: e.target.value ? Number(e.target.value) : null,
                          })
                        }
                      >
                        <option value="">{t('imports.map.skip')}</option>
                        {list.map((au) => (
                          <option key={au.id} value={au.id}>
                            {au.username}
                          </option>
                        ))}
                      </NativeSelect>
                      {clash ? (
                        <p
                          id={`${id}-error`}
                          className="mt-1 text-[12.5px] font-medium text-destructive"
                        >
                          {t('imports.map.clash')}
                        </p>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <div className="flex flex-col gap-3 border-t px-5 py-4">
        <b className="font-semibold">{t('imports.cutoff.label')}</b>
        <CutoffFields
          choice={choice}
          day={day}
          onChange={(c, d) => {
            setChoice(c);
            setDay(d);
          }}
          error={tried && cutoff === undefined ? t('imports.cutoff.dayRequired') : undefined}
        />
      </div>
      <div className="flex flex-col gap-3 border-t px-5 py-4">
        <FormError>{(tried && problem ? t(problem) : undefined) ?? error}</FormError>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Button type="button" variant="ghost" onClick={onBack} disabled={busy}>
            <ArrowLeft aria-hidden="true" />
            {t('imports.map.back')}
          </Button>
          <Button type="submit" disabled={busy || conn.users.length === 0}>
            {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
            {t('imports.map.start', counted(mapped, lang))}
          </Button>
        </div>
      </div>
    </form>
  );
}

/** The cutoff choice (as radio cards), for "before a date" the day, and what it never limits. */
function CutoffFields({
  choice,
  day,
  onChange,
  error,
}: {
  choice: CutoffChoice;
  day: string;
  onChange: (choice: CutoffChoice, day: string) => void;
  /** Shown under the day (a date is chosen but none entered). */
  error?: string;
}) {
  const { t } = useTranslation();
  const dayId = useId();
  return (
    <>
      <RadioCards
        label={t('imports.cutoff.label')}
        value={choice}
        onValueChange={(c) => onChange(c, day)}
        options={(['auto', 'all', 'date'] as const).map((value) => ({
          value,
          title: t(`imports.cutoff.${value}`),
          description: t(`imports.cutoff.${value}Body`),
        }))}
      />
      {choice === 'date' ? (
        <Field
          htmlFor={dayId}
          label={t('imports.cutoff.day')}
          error={error}
          className="max-w-[240px]"
        >
          <Input
            id={dayId}
            type="date"
            value={day}
            onChange={(e) => onChange(choice, e.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={describedBy(dayId, !!error, false)}
          />
        </Field>
      ) : null}
      <p className="text-[12.5px] text-muted-foreground">{t('imports.cutoff.note')}</p>
    </>
  );
}

/** Step 3 (each open import as a review card) and the history of decided ones. */
function ImportLists({ list }: { list: Import[] }) {
  const { t } = useTranslation();
  const { open, done } = splitImports(list);
  return (
    <>
      {open.length ? (
        <section aria-labelledby="import-review-title" className="flex flex-col gap-3">
          <h3 id="import-review-title" className="font-display text-[17px] font-[680]">
            {t('imports.review.heading')}
          </h3>
          {open.map((imp) => (
            <ReviewCard key={imp.id} imp={imp} all={list} />
          ))}
        </section>
      ) : null}
      {done.length ? (
        <Card aria-labelledby="import-history-title">
          <CardHeader
            titleId="import-history-title"
            title={t('imports.history.title')}
            description={t('imports.history.description')}
          />
          <ul className="flex flex-col divide-y" aria-label={t('imports.history.title')}>
            {done.map((imp) => (
              <ImportRow key={imp.id} imp={imp} />
            ))}
          </ul>
        </Card>
      ) : null}
    </>
  );
}

/**
 * One import waiting for a decision: fetching, failed (why, and what to do), or
 * ready (what it adds, what didn't match, the cutoff, Apply / Discard). Its
 * status and summary come from the list (polled while any import is busy); the
 * unmatched books are asked for once it is in review.
 */
function ReviewCard({ imp, all }: { imp: Import; all: Import[] }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const detail = useImportDetail(
    imp.id,
    imp.status === 'review' && (imp.summary?.unmatched ?? 0) > 0,
  );
  const { discard, pending: discarding } = useDiscardImport();
  const [applying, setApplying] = useState(false);
  const titleId = `import-${imp.id}-title`;
  const title = t('imports.row.title', { source: imp.source_user, user: imp.username });
  const replaced = replacedBy(imp, all);

  return (
    <Card aria-labelledby={titleId}>
      <CardHeader
        titleId={titleId}
        title={title}
        description={t('imports.review.from', {
          host: sourceHost(imp.source_url),
          when: formatRelative(imp.created_at, lang),
        })}
        action={<ImportStatusBadge status={imp.status} />}
      />
      <p role="status" className="sr-only">
        {t('imports.review.announce', { title, status: t(`imports.status.${imp.status}`) })}
      </p>
      {imp.status === 'fetching' ? (
        <div className="flex flex-col gap-3 p-5">
          <p className="flex items-center gap-2 text-muted-foreground">
            <LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
            {t('imports.review.fetching', { source: imp.source_user })}
          </p>
          <div className="skel h-16" />
        </div>
      ) : imp.status === 'failed' ? (
        <Notice
          tone="bad"
          icon={TriangleAlert}
          className="m-5"
          title={imp.error || t('imports.review.failed')}
        >
          {t(failureHintKey(imp.error_code))}
        </Notice>
      ) : imp.summary ? (
        <SummaryView
          imp={imp}
          summary={imp.summary}
          unmatched={detail.data?.unmatched_items}
          unmatchedLoading={detail.isLoading}
        />
      ) : null}
      {imp.status === 'review' && imp.summary ? <CutoffEditor imp={imp} /> : null}
      {imp.status !== 'fetching' ? (
        <div className="flex flex-wrap items-center justify-end gap-2 border-t px-5 py-3.5">
          <Button
            variant="ghost"
            disabled={discarding || importBusy(imp)}
            onClick={() => void discard(imp)}
          >
            {t('imports.discard')}
          </Button>
          {imp.status !== 'failed' ? (
            <Button
              disabled={imp.status !== 'review' || !imp.summary || importEmpty(imp.summary)}
              onClick={() => setApplying(true)}
            >
              {imp.status === 'applying' ? (
                <LoaderCircle className="animate-spin" aria-hidden="true" />
              ) : (
                <ImportIcon aria-hidden="true" />
              )}
              {imp.status === 'applying'
                ? t('imports.apply.applying')
                : t('imports.apply.action', { user: imp.username })}
            </Button>
          ) : null}
        </div>
      ) : null}
      <ConfirmDialog
        open={applying}
        onOpenChange={setApplying}
        title={t('imports.apply.title', { source: imp.source_user, user: imp.username })}
        description={
          imp.summary
            ? t('imports.apply.body', {
                hours: formatHours(imp.summary.listened, lang),
                ...counted(imp.summary.sessions, lang),
              })
            : undefined
        }
        icon={ImportIcon}
        tone="brand"
        destructive={false}
        confirmLabel={t('imports.apply.confirm')}
        onConfirm={async () => {
          try {
            // Shown as applied at once; the refetch below moves it to the history.
            setImportRow(qc, await api.applyImport(imp.id));
          } finally {
            invalidateImported(qc, imp);
          }
          toast.add({
            title: t('imports.apply.done', { user: imp.username }),
            description: t('imports.apply.doneBody'),
            type: 'success',
          });
        }}
      >
        <p className="text-[13px] text-muted-foreground">
          {replaced
            ? t('imports.apply.replaces', {
                user: imp.username,
                date: formatDate(replaced.applied_at, lang),
              })
            : t('imports.apply.reimport', { user: imp.username })}
        </p>
      </ConfirmDialog>
    </Card>
  );
}

/** What a planned import adds, as facts, then its unmatched books. */
function SummaryView({
  imp,
  summary: s,
  unmatched,
  unmatchedLoading,
}: {
  imp: Import;
  summary: ImportSummary;
  /** The unmatched books, once asked for. */
  unmatched: UnmatchedItem[] | undefined;
  unmatchedLoading: boolean;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const n = (v: number) => formatNumber(v, lang);
  const matched = matchedTotal(s);
  const tiers = matchTiers(s)
    .map((x) => t(`imports.summary.tier.${x.tier}`, { n: n(x.count) }))
    .join(', ');
  const facts: [string, string][] = [
    [t('imports.summary.sessions'), n(s.sessions)],
    [
      t('imports.summary.range'),
      s.first_listen && s.last_listen
        ? t('imports.summary.rangeValue', {
            from: formatDate(s.first_listen, lang),
            to: formatDate(s.last_listen, lang),
          })
        : t('imports.summary.none'),
    ],
    [
      t('imports.summary.matched'),
      t('imports.summary.matchedValue', { n: n(matched), of: n(s.items) }) +
        (tiers ? ` (${tiers})` : ''),
    ],
    [t('imports.summary.finished'), n(s.finished)],
    [t('imports.summary.progress'), n(s.progress)],
    [t('imports.summary.bookmarks'), n(s.bookmarks)],
  ];
  if (s.estimated > 0) facts.push([t('imports.summary.estimated'), formatHours(s.estimated, lang)]);
  if (s.skipped_after_cutoff > 0)
    facts.push([t('imports.summary.skipped'), n(s.skipped_after_cutoff)]);

  return (
    <div className="flex flex-col gap-4 p-5">
      {importEmpty(s) ? (
        <Notice tone="warn" icon={TriangleAlert} title={t('imports.summary.emptyTitle')}>
          {t('imports.summary.emptyBody')}
        </Notice>
      ) : (
        <p className="flex flex-wrap items-baseline gap-x-2">
          <span className="stat-value">{formatHours(s.listened, lang)}</span>
          <span className="text-muted-foreground">{t('imports.summary.hours')}</span>
        </p>
      )}
      <FactList rows={facts} layout="flow" />
      {s.unmatched > 0 ? (
        <details className="group rounded-lg border">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 rounded-lg px-4 py-2.5 font-semibold select-none [&::-webkit-details-marker]:hidden">
            <ChevronRight
              className="size-4 text-muted-foreground transition-transform duration-(--dur-1) group-open:rotate-90"
              aria-hidden="true"
            />
            {t('imports.unmatched.title', counted(s.unmatched, lang))}
          </summary>
          <div className="border-t">
            <p className="px-4 py-2.5 text-[12.5px] text-muted-foreground">
              {t('imports.unmatched.body')}
            </p>
            {unmatched ? (
              <ul className="max-h-[360px] divide-y overflow-y-auto border-t">
                {unmatched.map((u, i) => (
                  <li
                    key={`${i}-${u.title}`}
                    className="flex flex-wrap items-baseline gap-x-3 px-4 py-2"
                  >
                    <span className="flex min-w-0 flex-1 basis-56 flex-col">
                      <b className="font-semibold [overflow-wrap:anywhere]">
                        {u.title || t('imports.unmatched.untitled')}
                      </b>
                      <span className="text-[12px] text-muted-foreground">
                        {[u.author, t(unmatchedReasonKey(u.reason), { user: imp.username })]
                          .filter(Boolean)
                          .join(' · ')}
                      </span>
                    </span>
                    <span className="text-[12.5px] text-muted-foreground tabular-nums">
                      {formatHours(u.listened, lang)}
                    </span>
                  </li>
                ))}
              </ul>
            ) : unmatchedLoading ? (
              <div className="skel mx-4 mb-3 h-10" />
            ) : null}
          </div>
        </details>
      ) : null}
    </div>
  );
}

/** The review's cutoff: which sessions it keeps, and a way to change that (recounted by the server). */
function CutoffEditor({ imp }: { imp: Import }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [editing, setEditing] = useState(false);

  if (editing) return <CutoffForm imp={imp} onDone={() => setEditing(false)} />;
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 border-t px-5 py-3">
      <span className="flex items-center gap-2 text-[13px]">
        <CalendarDays className="size-4 text-muted-foreground" aria-hidden="true" />
        {imp.cutoff
          ? t('imports.cutoff.before', { date: cutoffText(imp, lang) })
          : t('imports.cutoff.everything')}
      </span>
      <Button
        variant="ghost"
        size="sm"
        aria-label={t('imports.cutoff.changeAria', { user: imp.username })}
        onClick={() => setEditing(true)}
      >
        {t('imports.cutoff.change')}
      </Button>
    </div>
  );
}

/** The cutoff being changed; mounted afresh each time, so it starts from the import's. */
function CutoffForm({ imp, onDone }: { imp: Import; onDone: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [choice, setChoice] = useState<CutoffChoice>(imp.cutoff === null ? 'all' : 'date');
  const [day, setDay] = useState(() => cutoffDay(imp));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const next = cutoffValue(choice, day);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || next === undefined) return;
    if (!cutoffChanged(imp, next)) return onDone();
    setBusy(true);
    setError(undefined);
    try {
      const saved = await api.setImportCutoff(imp.id, next);
      qc.setQueryData(keys.importDetail(imp.id), saved);
      setImportRow(qc, saved);
      void qc.invalidateQueries({ queryKey: keys.importLists });
      onDone();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    }
  };

  return (
    <form
      onSubmit={(e) => void save(e)}
      noValidate
      className="flex flex-col gap-3 border-t px-5 py-4"
    >
      <CutoffFields
        choice={choice}
        day={day}
        onChange={(c, d) => {
          setChoice(c);
          setDay(d);
        }}
        error={next === undefined ? t('imports.cutoff.dayRequired') : undefined}
      />
      <FormError>{error}</FormError>
      <div className="flex flex-wrap justify-end gap-2">
        <Button type="button" variant="ghost" size="sm" onClick={onDone}>
          {t('common.cancel')}
        </Button>
        <Button type="submit" size="sm" disabled={busy || next === undefined}>
          {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
          {t('imports.cutoff.save')}
        </Button>
      </div>
    </form>
  );
}
