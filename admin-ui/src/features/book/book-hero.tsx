import { useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Trans, useTranslation } from 'react-i18next';
import {
  CalendarPlus,
  Clock,
  Copy,
  Ellipsis,
  FileAudio,
  HardDrive,
  ImageMinus,
  ImageUp,
  RefreshCw,
  Repeat,
  Share2,
  Sparkles,
  TriangleAlert,
  Unplug,
  Zap,
  type LucideIcon,
} from 'lucide-react';
import { api } from '@/api/client';
import { invalidateBookCover, rescanBook, useLibraries, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { copyText } from '@/lib/clipboard';
import { toastError } from '@/lib/errors';
import { formatBytes, formatDuration, formatRelative } from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { COVER_TYPES, audioLine, coverFileProblem } from './book-model';
import { HERO_GRID } from './layout';
import { useHeroTint } from './use-hero-tint';

/**
 * The book's hero: its cover blurred into a full-bleed backdrop and tinted
 * from the art, the title and byline, the facts that matter to an admin, and
 * the book's actions (match, cover, more).
 */
export function BookHero({
  detail,
  matchBlocked,
  onMatch,
  onAddToShare,
}: {
  detail: AdminBookDetail;
  /** Unsaved edits: matching waits until they're saved or discarded. */
  matchBlocked: boolean;
  onMatch: () => void;
  onAddToShare: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const b = detail.book;
  const server = useServerInfo();
  const caps = server.data?.capabilities;
  const library = useLibraries().data?.find((l) => l.id === b.library_id);
  const tint = useHeroTint(b.library_id, b.path, b.title, b.author);
  // The eyebrow: the series and position, else the library.
  const eyebrow = !b.series
    ? b.library_name
    : b.series_index > 0
      ? t('book.hero.seriesBook', { series: b.series, n: String(b.series_index) })
      : b.series;

  const style = tint
    ? ({ '--tint1': tint.tint1, '--tint2': tint.tint2, '--glow': tint.glow } as React.CSSProperties)
    : undefined;

  const playback = b.direct_playable
    ? { icon: Zap, label: t('book.hero.direct'), tone: 'text-success' }
    : caps?.transcode === false
      ? { icon: TriangleAlert, label: t('book.hero.noTranscode'), tone: 'text-warning' }
      : { icon: Repeat, label: t('book.hero.transcode'), tone: 'text-warning' };
  const facts: { icon: LucideIcon; label: string; tone?: string }[] = [
    { icon: Clock, label: formatDuration(b.duration, lang) },
    { icon: FileAudio, label: audioLine(b, t) },
    { icon: HardDrive, label: formatBytes(b.size, lang) },
    playback,
    { icon: CalendarPlus, label: t('book.hero.added', { time: formatRelative(b.added_at, lang) }) },
  ].filter((f) => f.label);

  return (
    <section className="hero" aria-labelledby="book-title" style={style}>
      {/* Blurred to a wash: the smallest thumbnail does (and is the one the tint samples). */}
      <div className="hero-backdrop" aria-hidden="true">
        <BookCover
          libraryId={b.library_id}
          path={b.path}
          title={b.title}
          author={b.author}
          size={160}
        />
      </div>
      <div className={HERO_GRID}>
        <div className="hero-cover w-[min(240px,66vw)] md:w-auto">
          <BookCover
            libraryId={b.library_id}
            path={b.path}
            title={b.title}
            author={b.author}
            size={640}
          />
        </div>
        <div className="flex min-w-0 flex-col">
          <div className="flex flex-wrap items-center gap-2">
            <span className="eyebrow">{eyebrow}</span>
            {library && !library.available ? (
              <Badge variant="destructive">
                <Unplug aria-hidden="true" />
                {t('book.hero.offline')}
              </Badge>
            ) : null}
          </div>
          <h1
            id="book-title"
            className="mt-2 mb-3.5 font-display text-[32px] leading-none font-[750] tracking-[-0.035em] text-balance [overflow-wrap:anywhere] md:text-[38px] lg:text-[52px]"
          >
            {b.title}
          </h1>
          <Byline author={b.author} narrator={b.narrator} />
          <ul className="mt-4 flex flex-wrap gap-x-[18px] gap-y-1.5 text-[13px] text-muted-foreground tabular-nums">
            {facts.map(({ icon: Icon, label, tone }) => (
              <li key={label} className={cn('inline-flex items-center gap-1.5', tone)}>
                <Icon className="size-[15px]" aria-hidden="true" />
                {label}
              </li>
            ))}
          </ul>
          <div className="mt-6 flex flex-wrap items-center gap-2">
            {caps?.metadata ? (
              <Button onClick={onMatch} disabled={matchBlocked}>
                <Sparkles aria-hidden="true" />
                {b.matched ? t('book.hero.compare') : t('book.hero.match')}
              </Button>
            ) : null}
            <CoverMenu detail={detail} />
            <MoreMenu detail={detail} root={library?.root} onAddToShare={onAddToShare} />
          </div>
          {caps?.metadata && matchBlocked ? (
            <p className="mt-2 text-[12.5px] text-muted-foreground">
              {t('book.hero.matchBlocked')}
            </p>
          ) : null}
        </div>
      </div>
    </section>
  );
}

function Byline({ author, narrator }: { author: string; narrator: string }) {
  const name = <b className="font-[650] text-foreground" />;
  if (!author && !narrator) return null;
  return (
    <p className="text-[15px] text-muted-foreground md:text-[17px]">
      {author ? (
        <Trans i18nKey="book.hero.by" values={{ author }} components={{ b: name }} />
      ) : null}
      {author && narrator ? ' · ' : null}
      {narrator ? (
        <Trans i18nKey="book.hero.readBy" values={{ narrator }} components={{ b: name }} />
      ) : null}
    </p>
  );
}

/** Upload a custom cover (kept in AudioSilo's database) or remove it. */
function CoverMenu({ detail }: { detail: AdminBookDetail }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const input = useRef<HTMLInputElement>(null);
  const b = detail.book;

  // The art, the book page and every list row and count (has_cover, the curating shelf).
  const refresh = () => invalidateBookCover(qc, b);

  const upload = async (file: File) => {
    const problem = coverFileProblem(file);
    if (problem) {
      toast.add({ title: t('book.cover.failed'), description: t(problem), type: 'error' });
      return;
    }
    try {
      await api.setCover(b.library_id, b.path, file);
      refresh();
      toast.add({
        title: t('book.cover.saved'),
        description: t('book.cover.savedBody'),
        type: 'success',
      });
    } catch (err) {
      toastError(t('book.cover.failed'), err);
    }
  };

  const remove = async () => {
    try {
      await api.deleteCover(b.library_id, b.path);
      refresh();
      toast.add({
        title: t('book.cover.removed'),
        description: t('book.cover.removedBody'),
        type: 'success',
      });
    } catch (err) {
      toastError(t('book.cover.removeFailed'), err);
    }
  };

  return (
    <>
      <input
        ref={input}
        type="file"
        accept={COVER_TYPES.join(',')}
        className="hidden"
        aria-label={t('book.cover.upload')}
        tabIndex={-1}
        onChange={(e) => {
          const file = e.target.files?.[0];
          e.target.value = '';
          if (file) void upload(file);
        }}
      />
      <DropdownMenu>
        <DropdownMenuTrigger className={buttonVariants({ variant: 'outline' })}>
          <ImageUp aria-hidden="true" />
          {t('book.cover.change')}
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start">
          <DropdownMenuItem onClick={() => input.current?.click()}>
            <ImageUp aria-hidden="true" />
            {t('book.cover.upload')}
          </DropdownMenuItem>
          {b.custom_cover ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onClick={() => void remove()}>
                <ImageMinus aria-hidden="true" />
                {t('book.cover.remove')}
              </DropdownMenuItem>
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
    </>
  );
}

function MoreMenu({
  detail,
  root,
  onAddToShare,
}: {
  detail: AdminBookDetail;
  root: string | undefined;
  onAddToShare: () => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const b = detail.book;
  // Reads the book's files again now (tags, chapters, cover, read problems).
  const rescan = async () => {
    try {
      await rescanBook(qc, b);
      toast.add({ title: t('book.more.rescanned'), type: 'success' });
    } catch (err) {
      toastError(t('book.more.rescanFailed'), err);
    }
  };
  const copy = async () => {
    const ok = await copyText(joinLibraryPath(root, detail.book.path));
    toast.add(
      ok
        ? { title: t('book.more.copied'), type: 'success' }
        : { title: t('book.more.copyFailed'), type: 'error' },
    );
  };
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className={buttonVariants({ variant: 'outline', size: 'icon' })}
        aria-label={t('book.more.label')}
      >
        <Ellipsis className="size-4" aria-hidden="true" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onClick={() => void copy()}>
          <Copy aria-hidden="true" />
          {t('book.more.copyPath')}
        </DropdownMenuItem>
        <DropdownMenuItem onClick={onAddToShare}>
          <Share2 aria-hidden="true" />
          {t('book.more.addToShare')}
        </DropdownMenuItem>
        <DropdownMenuItem onClick={() => void rescan()}>
          <RefreshCw aria-hidden="true" />
          {t('book.more.rescan')}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
