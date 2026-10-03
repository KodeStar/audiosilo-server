import { useCover } from '@/api/hooks';
import type { ThumbSize } from '@/api/cover-batch';
import { cn } from '@/lib/utils';
import { GeneratedCover } from './generated-cover';

/**
 * A book's cover (square, the only element with a shadow). Real art is fetched
 * with the session header, never a token in the URL: grids, shelves and rows ask
 * for a batched thumbnail (`size`, default 320px), the book hero for the full art.
 * A skeleton holds the slot while it loads; a book without art (or whose art
 * failed to load) gets its procedural cover.
 */
export function BookCover({
  libraryId,
  path,
  title,
  author = '',
  size = 320,
  className,
}: {
  libraryId: number;
  path: string;
  title: string;
  author?: string;
  size?: ThumbSize | 'full';
  className?: string;
}) {
  const cover = useCover(libraryId, path, size);
  if (cover.isPending) {
    return <div className={cn('cover skel', className)} role="img" aria-label={title} />;
  }
  if (!cover.data) return <GeneratedCover title={title} author={author} className={className} />;
  return (
    <div className={cn('cover', className)}>
      <img src={cover.data} alt={title} decoding="async" />
    </div>
  );
}
