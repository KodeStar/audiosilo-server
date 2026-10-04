import { useState } from 'react';
import { useQueries, useQueryClient } from '@tanstack/react-query';
import { Link, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import {
  Database,
  FolderSearch,
  FolderTree as FolderTreeIcon,
  LibraryBig,
  RotateCw,
  ShieldCheck,
} from 'lucide-react';
import { api } from '@/api/client';
import { keys, useLibraries } from '@/api/hooks';
import type { AdminLibrary } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { rescanLibrary } from '@/features/libraries/rescan';
import { LibraryFilter } from '../library-filter';
import { useUpdateSearch } from '../library-param';
import type { LibrarySearch } from '../library-search';
import { FolderDetail } from './folder-detail';
import { FolderTree } from './folder-tree';
import { treeRows, visibleListings, withAncestors, type ListingState } from './folders-model';

/**
 * Library > Folders: a library's folders as a tree, and for the selected one
 * how AudioSilo reads it (a folder that holds audio is one book) with the
 * override that corrects it. `?library=` and `?folder=` deep-link the
 * selection; the book page links here with both. A finished rescan (which can
 * change which folders are books) refetches the listings (the scan watcher).
 */
export function FoldersPage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const libraries = useLibraries();
  const search = useSearch({ strict: false }) as LibrarySearch;
  const update = useUpdateSearch();
  const all = libraries.data ?? [];
  const library = all.find((l) => l.id === search.library) ?? all[0];
  const go = (next: Pick<LibrarySearch, 'library' | 'folder'>) =>
    update((prev) => ({ ...prev, ...next }));

  const picker = library ? (
    <LibraryFilter
      allowAll={false}
      value={library.id}
      onChange={(id) => go({ library: id, folder: undefined })}
    />
  ) : null;

  return (
    <Page>
      <PageHead title={t('folders.title')} description={t('folders.description')} action={picker} />
      {libraries.isError ? (
        <QueryError
          title={t('folders.librariesError')}
          error={libraries.error}
          onRetry={() => void libraries.refetch()}
        />
      ) : !libraries.data ? (
        <div className="skel h-[320px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : !library ? (
        <EmptyState
          icon={LibraryBig}
          title={t('folders.noLibraries.title')}
          body={t('folders.noLibraries.body')}
          action={
            <Link
              to="/library/{-$section}"
              params={{ section: 'libraries' }}
              className={buttonVariants({ variant: 'outline' })}
            >
              {t('folders.noLibraries.action')}
            </Link>
          }
        />
      ) : !library.available ? (
        <Notice
          tone="safe"
          icon={ShieldCheck}
          role="status"
          title={t('folders.offline.title', { root: library.root })}
          actions={
            <Button variant="outline" size="sm" onClick={() => rescanLibrary(qc, library)}>
              <RotateCw aria-hidden="true" />
              {t('folders.offline.retry')}
            </Button>
          }
        >
          {t('folders.offline.body')}
        </Notice>
      ) : (
        <FolderExplorer
          key={library.id}
          library={library}
          folder={search.folder || undefined}
          onSelect={(folder) => go({ folder })}
        />
      )}
    </Page>
  );
}

function FolderExplorer({
  library,
  folder,
  onSelect,
}: {
  library: AdminLibrary;
  folder?: string;
  onSelect: (path: string) => void;
}) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() =>
    withAncestors(new Set(), folder ?? ''),
  );
  // A folder selected from outside (a deep link, the book page) opens its ancestors.
  const [revealed, setRevealed] = useState(folder);
  if (folder !== revealed) {
    setRevealed(folder);
    if (folder) setExpanded((e) => withAncestors(e, folder));
  }

  // The open folders' listings, plus the selected one's (it says whether that
  // folder has subfolders, so its chevron is right before it is opened).
  const paths = visibleListings(expanded);
  if (folder && !paths.includes(folder)) paths.push(folder);
  const results = useQueries({
    queries: paths.map((p) => ({
      queryKey: keys.browse(library.id, p),
      queryFn: () => api.browse(library.id, p),
    })),
  });
  const byPath = new Map(paths.map((p, i) => [p, results[i]]));
  const listing = (p: string): ListingState | undefined => {
    const q = byPath.get(p);
    return q && { data: q.data, failed: q.isError };
  };
  const root = byPath.get('')!;

  const toggle = (path: string, open: boolean) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (open) next.add(path);
      else next.delete(path);
      return next;
    });

  if (root.isError) {
    return (
      <QueryError
        title={t('folders.error', { library: library.name })}
        error={root.error}
        onRetry={() => void root.refetch()}
      />
    );
  }
  const rows = root.data ? treeRows(listing, expanded) : [];
  if (root.data && rows.length === 0) {
    return (
      <EmptyState
        icon={FolderTreeIcon}
        title={t('folders.empty.title')}
        body={t('folders.empty.body', { root: library.root })}
      />
    );
  }

  return (
    <div className="grid items-start gap-5 md:grid-cols-[minmax(240px,340px)_minmax(0,1fr)]">
      <Card className="max-h-[45vh] overflow-y-auto p-2.5 md:max-h-[70vh]">
        <div className="flex min-w-0 items-center gap-2 px-2 py-1.5 text-[13.5px] font-semibold">
          <Database className="size-[15px] shrink-0 text-muted-foreground" aria-hidden="true" />
          <span className="shrink-0">{library.name}</span>
          <span className="ml-auto min-w-0 truncate font-mono text-[11px] font-normal text-subtle-foreground">
            {library.root}
          </span>
        </div>
        {root.data ? (
          <FolderTree
            label={t('folders.tree.label', { library: library.name })}
            rows={rows}
            selected={folder}
            onSelect={onSelect}
            onToggle={toggle}
            onRetry={(p) => void byPath.get(p)?.refetch()}
          />
        ) : (
          <div className="flex flex-col gap-1.5 p-2" role="status" aria-label={t('common.loading')}>
            {[0, 1, 2, 3, 4, 5].map((i) => (
              <span key={i} className="skel h-7" />
            ))}
          </div>
        )}
      </Card>
      {folder ? (
        <FolderDetail library={library} path={folder} />
      ) : (
        <EmptyState
          icon={FolderSearch}
          title={t('folders.none.title')}
          body={t('folders.none.body')}
        />
      )}
    </div>
  );
}
