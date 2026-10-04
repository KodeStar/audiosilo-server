import { useCover } from '@/api/hooks';
import type { ThumbSize } from '@/api/client';
import { cn } from '@/lib/utils';
import { GeneratedCover } from './generated-cover';

/**
 * A book's cover (square, the only element with a shadow). Real art comes as a
 * batched thumbnail fetched with the session header, never a token in the URL
 * (`size`: 160 for rows, 320 for tiles, the default, 640 for the book hero).
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
  size?: ThumbSize;
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
