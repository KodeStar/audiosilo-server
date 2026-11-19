import { useEffect, useRef, useState } from 'react';
import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type Query,
  type QueryClient,
} from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type {
  AudibleAccount,
  AudibleAccountPatch,
  AudibleTitle,
  AudibleTitlePage,
  AudibleTitlePatch,
  AudibleTitleState,
} from '@/api/types';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import {
  accountName,
  backupRunning,
  matchedParam,
  titleName,
  type MatchedFilter,
} from './audible-model';

/** How often the accounts are asked again while shown (a poll lands, a sign-in). */
const ACCOUNTS_POLL_MS = 30_000;

/** How often the loaded titles are asked again while a backup on them is queued or running. */
const TITLES_POLL_MS = 5000;

const TITLES_PAGE = 50;

/**
 * The Audible accounts: read with the module on or off (the route is outside
 * its toggle), so Settings > Modules lists those kept while it is off. Asked
 * again every 30 seconds unless `poll` is false: with the module off or no
 * Bridge paired nothing changes them but this console's Remove, which
 * invalidates the list itself.
 */
export function useAudibleAccounts({ poll = true } = {}) {
  return useQuery({
    queryKey: keys.audibleAccounts,
    queryFn: api.audibleAccounts,
    refetchInterval: poll ? ACCOUNTS_POLL_MS : false,
  });
}

/**
 * The titles by title, a page at a time, narrowed by the page's filters; asked
 * again every few seconds while a loaded title's backup is queued or running
 * (its state and match change with the job, not with a poll).
 */
export function useAudibleTitles(f: {
  account: string;
  state: AudibleTitleState | '';
  matched: MatchedFilter;
  q: string;
}) {
  // Keyed on what the server is asked, not the filter: "Not here" and "Needs a
  // look" read the same titles (the second narrows them here), so they share
  // the pages loaded.
  const matched = matchedParam(f.matched);
  const pages = useInfiniteQuery({
    queryKey: keys.audibleTitles(f.account, f.state, String(matched ?? ''), f.q),
    queryFn: ({ pageParam }) =>
      api.audibleTitles({
        account: f.account || undefined,
        state: f.state || undefined,
        matched,
        q: f.q || undefined,
        after: pageParam,
        limit: TITLES_PAGE,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next || undefined,
    placeholderData: keepPreviousData,
    refetchInterval: (query) =>
      query.state.data?.pages.some((p) => p.titles.some(backupRunning)) ? TITLES_POLL_MS : false,
  });
  return {
    rows: pages.data?.pages.flatMap((p) => p.titles) ?? [],
    pageCount: pages.data?.pages.length ?? 0,
    isPending: pages.isPending,
    isError: pages.isError,
    error: pages.error,
    refetch: () => pages.refetch(),
    hasMore: pages.hasNextPage,
    fetchMore: () => pages.fetchNextPage(),
    isFetchingMore: pages.isFetchingNextPage,
  };
}

/**
 * Refreshes the loaded titles once a poll lands: the accounts, asked again every
 * 30 s, carry each one's last poll time, and a new one means new titles (an
 * account added or removed changes them too).
 */
export function useTitlesAfterPolls(accounts: readonly AudibleAccount[] | undefined) {
  const qc = useQueryClient();
  const stamp = accounts?.map((a) => `${a.id}@${a.last_poll_at ?? ''}`).join(' ');
  const seen = useRef(stamp);
  useEffect(() => {
    if (stamp === undefined) return;
    if (seen.current !== undefined && stamp !== seen.current) {
      void qc.invalidateQueries({ queryKey: keys.audibleTitleLists });
    }
    seen.current = stamp;
  }, [stamp, qc]);
}

/**
 * Check now: queues an account's poll and says so (queued, already waiting, or
 * still stopping: the open poll that came back is being cancelled, so nothing
 * runs until it has stopped), refreshing the accounts (the queued time) and the
 * Bridge's jobs; the titles change once the poll lands (useTitlesAfterPolls). A
 * refusal is a toast.
 */
export function useCheckNow() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  const checkNow = async (account: AudibleAccount) => {
    setBusy(account.id);
    const name = accountName(t, account);
    try {
      const { job, created } = await api.pollAudible(account.id);
      toast.add(
        created
          ? {
              title: t('audible.check.done', { name }),
              description: t('audible.check.doneBody'),
              type: 'success',
            }
          : job.cancel_requested
            ? {
                title: t('audible.check.stopping', { name }),
                description: t('audible.check.stoppingBody'),
                type: 'warning',
              }
            : {
                title: t('audible.check.already', { name }),
                description: t('audible.check.alreadyBody'),
              },
      );
    } catch (err) {
      toastError(t('audible.check.failed'), err);
    } finally {
      setBusy(null);
      void qc.invalidateQueries({ queryKey: keys.audibleAccounts });
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
    }
  };
  return { checkNow, busy };
}

/** A title's key among the loaded titles: its account and ASIN. */
const titleKey = (x: Pick<AudibleTitle, 'account' | 'asin'>) => `${x.account}/${x.asin}`;

/**
 * Puts a title as the server answered it into every loaded titles page, in
 * place, rather than reading every page again; the lists are marked stale, so
 * one it now belongs in or has left is read again when next shown. A list's
 * read it had to stop is started again.
 */
export async function storeTitle(qc: QueryClient, x: AudibleTitle) {
  const key = titleKey(x);
  const holds = (p: AudibleTitlePage) => p.titles.some((t) => titleKey(t) === key);
  const lists = {
    queryKey: keys.audibleTitleLists,
    predicate: (q: Query) =>
      (q.state.data as InfiniteData<AudibleTitlePage> | undefined)?.pages.some(holds) ?? false,
  };
  // A read of a list holding the title already under way (the poll while a
  // backup runs, the re-read after an account's check landed) would land its
  // row as it was before this answer, over it: it is stopped first, and
  // started again below. A list without it, a first read among them, carries on.
  const stopped = new Set(
    qc
      .getQueryCache()
      .findAll({ ...lists, fetchStatus: 'fetching' })
      .map((q) => q.queryHash),
  );
  await qc.cancelQueries(lists);
  // A list (or page) without the title is left as it is: its rows don't render again.
  qc.setQueriesData<InfiniteData<AudibleTitlePage>>({ queryKey: keys.audibleTitleLists }, (data) =>
    data?.pages.some(holds)
      ? {
          ...data,
          pages: data.pages.map((p) =>
            holds(p) ? { ...p, titles: p.titles.map((t) => (titleKey(t) === key ? x : t)) } : p,
          ),
        }
      : data,
  );
  void qc.invalidateQueries({ queryKey: keys.audibleTitleLists, refetchType: 'none' });
  // The reads stopped above had news of their own (a check's new titles): a
  // shown list reads again, now past this answer.
  if (stopped.size > 0) {
    void qc.refetchQueries({
      queryKey: keys.audibleTitleLists,
      type: 'active',
      predicate: (q) => stopped.has(q.queryHash),
    });
  }
}

/**
 * Backups (Phase 4b): "Back up", "Not this book", a destination, "Back up
 * everything" and an account's settings; "Don't sync this book" (Phase 4c).
 * Each says how it went (a refusal is a toast, or the dialog's error when it
 * throws). A title's action puts the title the server answered into its row
 * (storeTitle; every page is read again only when an older server answers
 * without it) and refreshes the accounts (their estimate) and the Bridge's
 * jobs ("Don't sync" its row only); Back up everything reads the titles again.
 * A title's row is busy while its own action runs, whatever another row does.
 */
/** What useBackups gives the account card and the title rows. */
export type Backups = ReturnType<typeof useBackups>;

export function useBackups() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState<ReadonlySet<string>>(() => new Set());
  const mark = (x: AudibleTitle, on: boolean) =>
    setBusy((prev) => {
      const next = new Set(prev);
      if (on) next.add(titleKey(x));
      else next.delete(titleKey(x));
      return next;
    });
  const isBusy = (x: AudibleTitle) => busy.has(titleKey(x));
  /**
   * The title the server answered into its row, else (no answer) every page
   * read again; then, unless `others` is false, the accounts and the jobs.
   */
  const settle = (x: AudibleTitle | undefined, others = true) => {
    if (x) void storeTitle(qc, x);
    else void qc.invalidateQueries({ queryKey: keys.audibleTitleLists });
    if (others) {
      void qc.invalidateQueries({ queryKey: keys.audibleAccounts });
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
    }
  };

  const backUp = async (x: AudibleTitle) => {
    mark(x, true);
    let answered: AudibleTitle | undefined;
    try {
      const { pending, title } = await api.backupAudible(x.account, x.asin);
      answered = title;
      toast.add(
        pending
          ? {
              title: t('audible.backup.pending', { name: titleName(x) }),
              description: t('audible.backup.pendingBody'),
            }
          : {
              title: t('audible.backup.queued', { name: titleName(x) }),
              description: t('audible.backup.queuedBody'),
              type: 'success',
            },
      );
    } catch (err) {
      toastError(t('audible.backup.failed'), err);
    } finally {
      mark(x, false);
      settle(answered);
    }
  };

  /** A title's flag from its row's menu: a refusal is a toast, then `done` with the title answered (none after a refusal). */
  const patchTitle = async (
    x: AudibleTitle,
    patch: AudibleTitlePatch,
    failed: string,
    done: (answered: AudibleTitle | undefined) => void,
  ) => {
    mark(x, true);
    let answered: AudibleTitle | undefined;
    try {
      answered = await api.patchAudibleTitle(x.account, x.asin, patch);
    } catch (err) {
      toastError(failed, err);
    } finally {
      mark(x, false);
      done(answered);
    }
  };

  const setRejected = (x: AudibleTitle, rejected: boolean) =>
    patchTitle(x, { rejected }, t('audible.reject.failed'), settle);

  /** "Don't sync this book" and its undo (Phase 4c): it changes no estimate or job, only the title's row. */
  const setNoSync = (x: AudibleTitle, noSync: boolean) =>
    patchTitle(x, { no_sync: noSync }, t('audible.nosync.failed'), (answered) =>
      settle(answered, false),
    );

  /** Throws for the dialog to show. */
  const setDest = async (x: AudibleTitle, dest: string) => {
    settle(await api.patchAudibleTitle(x.account, x.asin, { dest_path: dest }));
  };

  /** Throws for the confirm to show. */
  const backUpAll = async (account: AudibleAccount) => {
    const { queued, skipped = 0 } = await api.backupAllAudible(account.id);
    toast.add({
      title: t('audible.backupAll.done', { count: queued, name: accountName(t, account) }),
      description: skipped > 0 ? t('audible.backupAll.skipped', { count: skipped }) : undefined,
      type: skipped > 0 ? 'warning' : 'success',
    });
    settle(undefined);
  };

  /** Throws for the dialog to show. */
  const saveAccount = async (account: AudibleAccount, patch: AudibleAccountPatch) => {
    await api.patchAudibleAccount(account.id, patch);
    toast.add({
      title: t('audible.settings.saved', { name: accountName(t, account) }),
      type: 'success',
    });
    // A policy, template or sync changes no title row: only the account card.
    void qc.invalidateQueries({ queryKey: keys.audibleAccounts });
  };

  return { backUp, setRejected, setNoSync, setDest, backUpAll, saveAccount, isBusy };
}
