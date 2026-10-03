import { ImageOff } from 'lucide-react';
import { useCover } from '@/api/hooks';
import { cn } from '@/lib/utils';

/**
 * A book's real cover art (square, the only element with a shadow). It is
 * fetched with the session header (never a token in the URL, see fetchCover),
 * shows a skeleton while loading, and falls back to the hatched "missing" cover
 * when the book has no art. Procedural generated covers arrive with the Library
 * (Phase 2b).
 */
export function BookCover({
  libraryId,
  path,
  title,
  className,
}: {
  libraryId: number;
  path: string;
  title: string;
  className?: string;
}) {
  const cover = useCover(libraryId, path);
  if (cover.isPending) {
    return <div className={cn('cover skel', className)} role="img" aria-label={title} />;
  }
  if (!cover.data) {
    return (
      <div className={cn('cover', className)} data-missing="" role="img" aria-label={title}>
        <ImageOff className="size-[30%] max-w-6" aria-hidden="true" />
      </div>
    );
  }
  return (
    <div className={cn('cover', className)}>
      <img src={cover.data} alt={title} decoding="async" />
    </div>
  );
}
