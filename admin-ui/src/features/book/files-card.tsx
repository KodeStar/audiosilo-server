import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { FolderOpen, Lock, Move } from 'lucide-react';
import { useLibraries } from '@/api/hooks';
import type { AdminBookDetail } from '@/api/types';
import { PlaybackStatus } from '@/components/playback-status';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { formatBytes, formatDuration, formatNumber } from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { detectionKey, fileName, sortFiles } from './book-model';

/** Rows shown before "and N more files". */
const MAX_ROWS = 6;

/** The book's audio files: codec, bitrate, size, length and whether browsers play them directly. */
export function FilesCard({ detail }: { detail: AdminBookDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const b = detail.book;
  const files = sortFiles(detail.files ?? []);
  const rows = files.slice(0, MAX_ROWS);
  const kbps = (bitrate: number) =>
    bitrate > 0 ? t('book.files.kbps', { n: formatNumber(Math.round(bitrate / 1000), lang) }) : '';

  return (
    <Card aria-labelledby="files-title">
      <CardHeader
        titleId="files-title"
        title={t('book.files.title')}
        action={
          <span className="text-muted-foreground tabular-nums">
            {formatNumber(files.length, lang)} · {formatBytes(b.size, lang)}
          </span>
        }
      />
      <div className="overflow-x-auto">
        <table className="w-full text-[13.5px]">
          <thead className="hidden text-[12px] text-muted-foreground md:table-header-group">
            <tr className="border-b text-left">
              <th className="px-5 py-2.5 font-medium">{t('book.files.file')}</th>
              <th className="px-3 py-2.5 font-medium">{t('book.files.codec')}</th>
              <th className="px-3 py-2.5 text-right font-medium">{t('book.files.bitrate')}</th>
              <th className="px-3 py-2.5 text-right font-medium">{t('book.files.size')}</th>
              <th className="px-3 py-2.5 text-right font-medium">{t('book.files.length')}</th>
              <th className="px-5 py-2.5 font-medium">{t('book.files.playback')}</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {rows.map((f) => (
              <tr
                key={f.path}
                className="flex flex-wrap items-center gap-x-3 gap-y-1 px-5 py-3 md:table-row md:p-0"
              >
                <td className="w-full md:w-auto md:px-5 md:py-2.5">
                  <span className="font-mono text-[12.5px] [overflow-wrap:anywhere]">
                    {fileName(f.path, b.path)}
                  </span>
                  <span className="mt-0.5 block text-[12px] text-muted-foreground tabular-nums md:hidden">
                    {[
                      f.codec.toUpperCase(),
                      kbps(f.bitrate),
                      formatBytes(f.size, lang),
                      formatDuration(f.duration, lang),
                    ]
                      .filter(Boolean)
                      .join(' · ')}
                  </span>
                </td>
                <td className="hidden px-3 py-2.5 md:table-cell">
                  {f.codec ? <Badge variant="outline">{f.codec.toUpperCase()}</Badge> : null}
                </td>
                <td className="hidden px-3 py-2.5 text-right tabular-nums md:table-cell">
                  {kbps(f.bitrate)}
                </td>
                <td className="hidden px-3 py-2.5 text-right tabular-nums md:table-cell">
                  {formatBytes(f.size, lang)}
                </td>
                <td className="hidden px-3 py-2.5 text-right tabular-nums md:table-cell">
                  {formatDuration(f.duration, lang)}
                </td>
                <td className="md:px-5 md:py-2.5">
                  <PlaybackStatus direct={b.direct_playable} />
                </td>
              </tr>
            ))}
            {files.length > MAX_ROWS ? (
              <tr className="block px-5 py-3 md:table-row md:p-0">
                <td colSpan={6} className="text-muted-foreground md:px-5 md:py-2.5">
                  {t('book.files.more', { count: files.length - MAX_ROWS })}
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

/**
 * Files on disk: the one place that will ever change the actual folder. For
 * now it only shows where the book lives and how its folder is detected;
 * renaming on disk is a later, opt-in feature (off by default).
 */
export function DiskSection({ detail }: { detail: AdminBookDetail }) {
  const { t } = useTranslation();
  const b = detail.book;
  const library = useLibraries().data?.find((l) => l.id === b.library_id);
  return (
    <section
      aria-labelledby="disk-title"
      className="mt-3 border-t border-dashed border-border-strong pt-7"
    >
      <div className="mb-1.5 flex items-center gap-2.5">
        <FolderOpen className="size-[18px]" aria-hidden="true" />
        <h2 id="disk-title" className="h2">
          {t('book.disk.title')}
        </h2>
      </div>
      <p className="mb-4 max-w-[640px] text-muted-foreground">{t('book.disk.description')}</p>
      <Card className="flex flex-col gap-4 p-5">
        <div className="flex flex-col gap-1.5">
          <span className="text-[13px] font-semibold">{t('book.disk.location')}</span>
          <code className="rounded-[10px] bg-muted px-3 py-2 font-mono text-[12.5px] leading-[1.6] [overflow-wrap:anywhere]">
            {joinLibraryPath(library?.root, b.path)}
          </code>
          <span className="text-[12.5px] text-muted-foreground">{t('book.disk.identity')}</span>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex min-w-0 flex-col">
            <span className="text-[13px] font-semibold">{t('book.disk.detection')}</span>
            <span className="text-[12.5px] text-muted-foreground">
              {t(detectionKey(detail.folder.override))}
            </span>
          </div>
          <Link
            to="/library/{-$section}"
            params={{ section: 'folders' }}
            search={{ library: b.library_id, folder: detail.folder.path }}
            className={buttonVariants({ variant: 'outline', size: 'sm' })}
          >
            {t('book.disk.changeDetection')}
          </Link>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-4">
          <div className="flex min-w-0 flex-1 basis-60 flex-col">
            <span className="flex items-center gap-2 text-[13px] font-semibold">
              {t('book.disk.rename')}
              <Badge variant="outline">
                <Lock aria-hidden="true" />
                {t('book.disk.off')}
              </Badge>
            </span>
            <span id="rename-why" className="text-[12.5px] text-muted-foreground">
              {t('book.disk.renameBody')}
            </span>
          </div>
          <Button variant="outline" size="sm" disabled aria-describedby="rename-why">
            <Move aria-hidden="true" />
            {t('book.disk.preview')}
          </Button>
        </div>
      </Card>
    </section>
  );
}
