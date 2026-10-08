import { useMemo } from 'react';
import { Link } from '@tanstack/react-router';
import { booksRoute, type BooksField } from '@/lib/book-route';

/**
 * A link to the Books list filtered to one author, narrator or series
 * (booksRoute). The route is memoized per value: Link rebuilds its href
 * whenever its `search` changes identity, and booksRoute's is a function.
 */
export function BooksLink({
  field,
  value,
  className,
  onClick,
  children,
}: {
  field: BooksField;
  value: string;
  className?: string;
  onClick?: (e: React.MouseEvent) => void;
  children?: React.ReactNode;
}) {
  const route = useMemo(() => booksRoute(field, value), [field, value]);
  return (
    <Link {...route} className={className} onClick={onClick}>
      {children}
    </Link>
  );
}
