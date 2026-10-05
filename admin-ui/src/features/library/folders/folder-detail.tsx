import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookOpen, FileAudio, FolderOpen, FolderX } from 'lucide-react';
import { setFolderMode, useBrowse } from '@/api/hooks';
import type { AdminLibrary, FsEntry } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { RadioCards } from '@/components/ui/radio-cards';
import { bookRoute } from '@/lib/book-route';
import { toastError } from '@/lib/errors';
import { counted, formatBytes, formatDuration } from '@/lib/format';
import { joinLibraryPath, relBaseName, relParent } from '@/lib/paths';
import { toast } from '@/lib/toast';
import { FOLDER_CHOICES, choiceOf, modeOf, type FolderChoice } from './folder-modes';
import { audioFilesOf, entryIn, foldersIn, joinChoices } from './folders-model';

/**
 * The selected folder: what it is, how AudioSilo reads it (each choice saved
 * at once, then the library rescans) and the audio files it holds. Its own
 * entry (override, whether it is a book) comes from its parent's listing, its
 * files from its own; the tree has usually loaded both already.
 */
export function FolderDetail({ library, path }: { library: AdminLibrary; path: string }) {
  const { t } = useTranslation();
  const parent = useBrowse(library.id, relParent(path));
  const own = useBrowse(library.id, path);
  const entry = entryIn(parent.data, path);
  const name = entry?.name ?? relBaseName(path);

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
        error={failed.error}
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
  const files = audioFilesOf(own.data);
  return (
    <Detail
      key={path}
      library={library}
      entry={entry}
      files={files}
      // A folder with no audio of its own can still be joined from its disc folders.
      join={joinChoices(files.files.length, entry, foldersIn(own.data))}
    />
  );
}

function Detail({
  library,
  entry,
  files: { files, size, duration },
  join,
}: {
  library: AdminLibrary;
  entry: FsEntry;
  files: ReturnType<typeof audioFilesOf>;
  join: ReturnType<typeof joinChoices>;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  // The choice being saved: shown at once, dropped when the request settles.
  const [saving, setSaving] = useState<FolderChoice>();
  const count = files.length;
  const fileCount = counted(count, lang);
  /** What a choice means for a folder joined from its disc folders. */
  const joinHere = (c: FolderChoice) => {
    if (c === 'auto') return t('folders.mode.autoJoinHere');
    if (c !== 'book') return undefined;
    return join.subBooks
      ? t('folders.mode.bookJoinCount', counted(join.subBooks, lang))
      : t('folders.mode.bookJoinHere');
  };

  const choose = async (choice: FolderChoice) => {
    if (choice === choiceOf(entry.override)) return;
    setSaving(choice);
    try {
      await setFolderMode(qc, library.id, entry.path, modeOf(choice));
      toast.add({
        title: t(`folders.toast.${choice}`, fileCount),
        description: t(toastBody(join.joinable, choice, entry.override), { library: library.name }),
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
              {joinLibraryPath(library.root, entry.path)}
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
            className="grid grid-cols-1 gap-2.5 xl:grid-cols-3"
            options={FOLDER_CHOICES.map((c) => ({
              value: c,
              title: t(`folders.mode.${c}`),
              // What the choice means, then what it means here: for a folder with
              // audio, its files; for one joined from its disc folders, those.
              description: [
                t(`folders.mode.${c}Body`),
                count > 0 && c !== 'book' && t(`folders.mode.${c}Here`, fileCount),
                join.joinable && joinHere(c),
              ]
                .filter(Boolean)
                .join(' '),
              disabled: !join.enabled.includes(c) || saving !== undefined,
            }))}
          />
          {count === 0 ? (
            <p className="text-[12.5px] text-muted-foreground">
              {t(join.joinable ? 'folders.detail.subfolderAudio' : 'folders.detail.noAudio')}
            </p>
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
                {t('folders.files.summary', { ...fileCount, size: formatBytes(size, lang) })}
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

/**
 * What a change does to listening progress: it moves with each file, carries over
 * to a folder joined from its disc folders, or (un-joining) stays with the joined book.
 */
function toastBody(joinable: boolean, choice: FolderChoice, override: FsEntry['override']) {
  if (!joinable) return 'folders.toast.body';
  if (choice === 'book') return 'folders.toast.joinBody';
  return override === 'book' ? 'folders.toast.unjoinBody' : 'folders.toast.body';
}
