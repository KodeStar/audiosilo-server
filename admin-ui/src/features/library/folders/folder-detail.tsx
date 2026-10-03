import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookOpen, FileAudio, FolderOpen, FolderX } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateBooks, noteScanStarted, useBrowse } from '@/api/hooks';
import type { AdminLibrary, FsEntry } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { RadioCards } from '@/components/ui/radio-cards';
import { bookRoute } from '@/lib/book-route';
import { errorMessage, toastError } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import { CHOICE_TITLE, FOLDER_CHOICES, choiceOf, modeOf, type FolderChoice } from './folder-modes';
import {
  audioFilesOf,
  baseName,
  entryIn,
  formatBytes,
  formatDuration,
  fullPath,
  parentOf,
} from './folders-model';

// What each choice means, then (for a folder with audio) what it means here.
const CHOICE_BODY: Record<FolderChoice, string> = {
  auto: 'folders.mode.autoBody',
  book: 'folders.mode.bookBody',
  collection: 'folders.mode.collectionBody',
};
const CHOICE_HERE: Partial<Record<FolderChoice, string>> = {
  auto: 'folders.mode.autoHere',
  collection: 'folders.mode.collectionHere',
};
const CHOICE_TOAST: Record<FolderChoice, string> = {
  auto: 'folders.toast.auto',
  book: 'folders.toast.book',
  collection: 'folders.toast.collection',
};

/**
 * The selected folder: what it is, how AudioSilo reads it (each choice saved
 * at once, then the library rescans) and the audio files it holds. Its own
 * entry (override, whether it is a book) comes from its parent's listing, its
 * files from its own; the tree has usually loaded both already.
 */
export function FolderDetail({ library, path }: { library: AdminLibrary; path: string }) {
  const { t } = useTranslation();
  const parent = useBrowse(library.id, parentOf(path));
  const own = useBrowse(library.id, path);
  const entry = entryIn(parent.data, path);
  const name = entry?.name ?? baseName(path);

  if (parent.data && !entry) {
    return (
      <Notice
        tone="info"
        icon={FolderX}
        title={t('folders.notFound.title', { name })}
        role="status"
      >
        {t('folders.notFound.body', { library: library.name })}
      </Notice>
    );
  }
  const failed = parent.isError ? parent : own.isError ? own : undefined;
  if (failed) {
    return (
      <QueryError
        title={t('folders.detail.error', { name })}
        error={new Error(errorMessage(failed.error, t))}
        onRetry={() => void failed.refetch()}
      />
    );
  }
  if (!entry || !own.data) {
    return (
      <div className="flex flex-col gap-4" role="status" aria-label={t('common.loading')}>
        <span className="skel h-[220px] rounded-xl" />
        <span className="skel h-[160px] rounded-xl" />
      </div>
    );
  }
  return <Detail key={path} library={library} entry={entry} files={audioFilesOf(own.data)} />;
}

function Detail({
  library,
  entry,
  files: { files, size, duration },
}: {
  library: AdminLibrary;
  entry: FsEntry;
  files: ReturnType<typeof audioFilesOf>;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  // The choice being saved: shown at once, dropped when the request settles.
  const [saving, setSaving] = useState<FolderChoice>();
  const count = files.length;
  const counted = { count, formatted: formatNumber(count, lang) };

  const choose = async (choice: FolderChoice) => {
    if (choice === choiceOf(entry.override)) return;
    setSaving(choice);
    try {
      await api.setFolderOverride(library.id, entry.path, modeOf(choice));
      noteScanStarted(qc, library.id); // the rescan it started
      await qc.invalidateQueries({ queryKey: ['fs', library.id] });
      invalidateBooks(qc);
      toast.add({
        title: t(CHOICE_TOAST[choice], counted),
        description: t('folders.toast.body', { library: library.name }),
        type: 'success',
      });
    } catch (err) {
      toastError(t('folders.toast.failed', { name: entry.name }), err);
    } finally {
      setSaving(undefined);
    }
  };

  return (
    <div className="flex min-w-0 flex-col gap-4">
      <Card aria-labelledby="folder-name" className="flex flex-col gap-5 p-5">
        <div className="flex flex-wrap items-start gap-4">
          {entry.is_book ? (
            <div className="w-[84px] shrink-0">
              <BookCover
                libraryId={library.id}
                path={entry.path}
                title={entry.title || entry.name}
                author={entry.author}
              />
            </div>
          ) : (
            <span
              className="grid size-14 shrink-0 place-items-center rounded-[14px] bg-muted text-muted-foreground"
              aria-hidden="true"
            >
              <FolderOpen className="size-6" />
            </span>
          )}
          <div className="flex min-w-0 flex-1 basis-56 flex-col gap-1.5">
            <span className="eyebrow">{t('folders.detail.eyebrow')}</span>
            <h2 id="folder-name" className="h2 [overflow-wrap:anywhere]">
              {entry.name}
            </h2>
            <p className="rounded-md bg-muted px-3 py-2 font-mono text-[12.5px] leading-relaxed [overflow-wrap:anywhere]">
              {fullPath(library.root, entry.path)}
            </p>
          </div>
          {entry.is_book ? (
            <Link
              {...bookRoute(library.id, entry.path)}
              className={buttonVariants({ variant: 'outline', size: 'sm' })}
            >
              <BookOpen aria-hidden="true" />
              {t('folders.detail.openBook')}
            </Link>
          ) : null}
        </div>
        <div className="flex flex-col gap-2">
          <span className="text-[13px] font-semibold">{t('folders.detail.question')}</span>
          <RadioCards
            label={t('folders.detail.question')}
            value={saving ?? choiceOf(entry.override)}
            onValueChange={(c) => void choose(c)}
            className="grid gap-2.5 xl:grid-cols-3"
            options={FOLDER_CHOICES.map((c) => ({
              value: c,
              title: t(CHOICE_TITLE[c]),
              description: [
                t(CHOICE_BODY[c]),
                count && CHOICE_HERE[c] && t(CHOICE_HERE[c], counted),
              ]
                .filter(Boolean)
                .join(' '),
              disabled: count === 0 || saving !== undefined,
            }))}
          />
          {count === 0 ? (
            <p className="text-[12.5px] text-muted-foreground">{t('folders.detail.noAudio')}</p>
          ) : null}
        </div>
      </Card>
      {count > 0 ? (
        <Card aria-labelledby="folder-files">
          <CardHeader
            titleId="folder-files"
            title={t('folders.files.title')}
            action={
              <span className="text-muted-foreground tabular-nums">
                {t('folders.files.summary', { ...counted, size: formatBytes(size, lang) })}
                {duration ? ` · ${formatDuration(duration, lang)}` : null}
              </span>
            }
          />
          <ul className="divide-y">
            {files.map((f) => (
              <li key={f.path} className="flex min-h-11 items-center gap-3 px-5 py-2.5">
                <FileAudio className="size-4 shrink-0 text-subtle-foreground" aria-hidden="true" />
                {f.is_book ? (
                  <Link
                    {...bookRoute(library.id, f.path)}
                    className="min-w-0 flex-1 truncate font-mono text-[12.5px] text-brand-ink hover:underline"
                  >
                    {f.name}
                  </Link>
                ) : (
                  <span className="min-w-0 flex-1 truncate font-mono text-[12.5px]">{f.name}</span>
                )}
                <span className="shrink-0 text-[12.5px] text-muted-foreground tabular-nums">
                  {formatBytes(f.size, lang)}
                  {f.duration ? ` · ${formatDuration(f.duration, lang)}` : null}
                </span>
              </li>
            ))}
          </ul>
        </Card>
      ) : null}
    </div>
  );
}
