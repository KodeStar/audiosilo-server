import { useState } from 'react';
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
 * community cover the server fetched. Art that isn't square is shown whole,
 * over a blurred copy of itself that fills the square.
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
  return <Art key={src} src={src} title={title} className={className} />;
}

/** Most art is square; only art that isn't gets the blurred fill (a filter layer per cover). */
function Art({ src, title, className }: { src: string; title: string; className?: string }) {
  const [letterbox, setLetterbox] = useState(false);
  return (
    <div className={cn('cover', className)}>
      {letterbox ? (
        <img className="cover-fill" src={src} alt="" aria-hidden="true" decoding="async" />
      ) : null}
      <img
        src={src}
        alt={title}
        decoding="async"
        onLoad={(e) => {
          const { naturalWidth: w, naturalHeight: h } = e.currentTarget;
          setLetterbox(Math.abs(w - h) > Math.max(w, h) * 0.02);
        }}
      />
    </div>
  );
}
