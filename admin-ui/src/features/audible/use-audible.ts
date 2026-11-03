import { useEffect, useRef, useState } from 'react';
import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type {
  AudibleAccount,
  AudibleAccountPatch,
  AudibleTitle,
  AudibleTitleState,
} from '@/api/types';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { accountName, matchedParam, type MatchedFilter } from './audible-model';

/** How often the accounts are asked again while shown (a poll lands, a sign-in). */
const ACCOUNTS_POLL_MS = 30_000;

const TITLES_PAGE = 50;

/** The Audible accounts (shown only while the module is on: 404 module_off otherwise). */
export function useAudibleAccounts() {
  return useQuery({
    queryKey: keys.audibleAccounts,
    queryFn: api.audibleAccounts,
    refetchInterval: ACCOUNTS_POLL_MS,
  });
}

/** The titles by title, a page at a time, narrowed by the page's filters. */
export function useAudibleTitles(f: {
  account: string;
  state: AudibleTitleState | '';
  matched: MatchedFilter;
  q: string;
}) {
  const pages = useInfiniteQuery({
    queryKey: keys.audibleTitles(f.account, f.state, f.matched, f.q),
    queryFn: ({ pageParam }) =>
      api.audibleTitles({
        account: f.account || undefined,
        state: f.state || undefined,
        matched: matchedParam(f.matched),
        q: f.q || undefined,
        after: pageParam,
        limit: TITLES_PAGE,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next || undefined,
    placeholderData: keepPreviousData,
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

/**
 * Backups (Phase 4b): "Back up", "Not this book", a destination, "Back up
 * everything" and an account's settings. Each says how it went (a refusal is a
 * toast, or the dialog's error when it throws) and refreshes the titles, the
 * accounts (their estimate) and the Bridge's jobs.
 */
export function useBackups() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: keys.audibleTitleLists });
    void qc.invalidateQueries({ queryKey: keys.audibleAccounts });
    void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
  };
  const titleKey = (x: AudibleTitle) => `${x.account}/${x.asin}`;
  const name = (x: AudibleTitle) => x.title || x.asin;

  const backUp = async (x: AudibleTitle) => {
    setBusy(titleKey(x));
    try {
      const { pending } = await api.backupAudible(x.account, x.asin);
      toast.add(
        pending
          ? {
              title: t('audible.backup.pending', { name: name(x) }),
              description: t('audible.backup.pendingBody'),
            }
          : {
              title: t('audible.backup.queued', { name: name(x) }),
              description: t('audible.backup.queuedBody'),
              type: 'success',
            },
      );
    } catch (err) {
      toastError(t('audible.backup.failed'), err);
    } finally {
      setBusy(null);
      refresh();
    }
  };

  const setRejected = async (x: AudibleTitle, rejected: boolean) => {
    setBusy(titleKey(x));
    try {
      await api.patchAudibleTitle(x.account, x.asin, { rejected });
    } catch (err) {
      toastError(t('audible.reject.failed'), err);
    } finally {
      setBusy(null);
      refresh();
    }
  };

  /** Throws for the dialog to show. */
  const setDest = async (x: AudibleTitle, dest: string) => {
    await api.patchAudibleTitle(x.account, x.asin, { dest_path: dest });
    refresh();
  };

  /** Throws for the confirm to show. */
  const backUpAll = async (account: AudibleAccount) => {
    const { queued } = await api.backupAllAudible(account.id);
    toast.add({
      title: t('audible.backupAll.done', { count: queued, name: accountName(t, account) }),
      type: 'success',
    });
    refresh();
  };

  /** Throws for the dialog to show. */
  const saveAccount = async (account: AudibleAccount, patch: AudibleAccountPatch) => {
    await api.patchAudibleAccount(account.id, patch);
    toast.add({
      title: t('audible.settings.saved', { name: accountName(t, account) }),
      type: 'success',
    });
    refresh();
  };

  return { backUp, setRejected, setDest, backUpAll, saveAccount, busy, titleKey };
}
