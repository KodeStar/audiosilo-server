import { Link } from '@tanstack/react-router';
import { Trans, useTranslation } from 'react-i18next';
import { ExternalLink } from 'lucide-react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/lib/utils';

/**
 * "Add it": the community site's Add a book page for one book (addBookUrl, in
 * src/lib/meta-site.ts), in a new tab. A row's compact button, or a text link
 * in a sentence.
 */
export function AddItLink({
  href,
  title,
  button = false,
}: {
  href: string;
  /** The book's title, for the link's accessible name. */
  title: string;
  button?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={t('book.match.addItAria', { title })}
      className={
        button
          ? buttonVariants({ variant: 'ghost', size: 'sm' })
          : 'inline-flex items-center gap-1 font-semibold text-brand-ink hover:underline'
      }
    >
      <span className={cn(button && 'max-md:hidden')}>{t('book.match.addIt')}</span>
      <ExternalLink aria-hidden="true" className={cn(!button && 'size-3.5')} />
    </a>
  );
}

/**
 * What a book the community database doesn't hold reads (server#370): that it
 * still plays as normal, and the two ways to fill the gap: add it on the
 * community site (`href`: AddItLink), and the setting that sends the database
 * the ASINs it doesn't know (Settings > Metadata). `books` words it for a list.
 */
export function NotInDatabase({
  href,
  title,
  books = false,
  className,
}: {
  /** The Add it link, for one book; none for a list (each row has its own). */
  href?: string;
  title?: string;
  books?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <div className={cn('flex flex-col gap-1.5 text-[13px] text-muted-foreground', className)}>
      <p>
        {t(books ? 'book.match.notInDatabaseBooks' : 'book.match.notInDatabase')}
        {href ? (
          <>
            {' '}
            <AddItLink href={href} title={title ?? ''} />
          </>
        ) : null}
      </p>
      <p className="text-[12.5px]">
        <Trans
          i18nKey="book.match.helpComplete"
          components={{
            settings: (
              <Link
                to="/server/{-$section}"
                params={{ section: undefined }}
                search={{ topic: 'metadata' }}
                className="font-semibold text-brand-ink hover:underline"
              >
                {null}
              </Link>
            ),
          }}
        />
      </p>
    </div>
  );
}
