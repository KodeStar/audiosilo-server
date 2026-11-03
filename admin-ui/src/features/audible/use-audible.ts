import { useState } from 'react';
import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AudibleAccount, AudibleTitleState } from '@/api/types';
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
 * Check now: queues an account's poll and says so (queued, or already waiting),
 * refreshing the accounts (the queued time) and the Bridge's jobs; the titles
 * change only once the poll lands. A refusal is a toast.
 */
export function useCheckNow() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  const checkNow = async (account: AudibleAccount) => {
    setBusy(account.id);
    const name = accountName(t, account);
    try {
      const { created } = await api.pollAudible(account.id);
      toast.add(
        created
          ? {
              title: t('audible.check.done', { name }),
              description: t('audible.check.doneBody'),
              type: 'success',
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
