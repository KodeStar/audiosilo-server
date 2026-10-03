import { useState } from 'react';
import { ImageOff } from 'lucide-react';
import { coverUrl } from '@/api/client';
import { cn } from '@/lib/utils';

/**
 * A book's real cover art (square, the only element with a shadow). When the
 * book has no art the server 404s and this falls back to the hatched "missing"
 * cover. The procedural generated covers arrive with the Library (Phase 2b).
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
  const [failed, setFailed] = useState(false);
  if (failed) {
    return (
      <div className={cn('cover', className)} data-missing="" role="img" aria-label={title}>
        <ImageOff className="size-[30%] max-w-6" aria-hidden="true" />
      </div>
    );
  }
  return (
    <div className={cn('cover', className)}>
      <img
        src={coverUrl(libraryId, path)}
        alt={title}
        loading="lazy"
        decoding="async"
        onError={() => setFailed(true)}
      />
    </div>
  );
}
