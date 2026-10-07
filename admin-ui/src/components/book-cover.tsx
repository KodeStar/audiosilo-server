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
  return (
    <CoverArt
      src={cover.data}
      pending={cover.isPending}
      title={title}
      author={author}
      className={className}
    />
  );
}

/**
 * A cover from a thumbnail data: URL: a skeleton while `pending`, the procedural
 * cover without one. BookCover feeds it a book's art; the match dialog a
 * community cover the server fetched.
 */
export function CoverArt({
  src,
  pending,
  title,
  author = '',
  className,
}: {
  src: string | null | undefined;
  pending: boolean;
  title: string;
  author?: string;
  className?: string;
}) {
  if (pending) {
    return <div className={cn('cover skel', className)} role="img" aria-label={title} />;
  }
  if (!src) return <GeneratedCover title={title} author={author} className={className} />;
  return (
    <div className={cn('cover', className)}>
      <img src={src} alt={title} decoding="async" />
    </div>
  );
}
