import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Compass } from 'lucide-react';
import { buttonVariants } from '@/components/ui/button';

export function NotFound() {
  const { t } = useTranslation();
  return (
    <div className="mx-auto flex max-w-[1440px] flex-col items-center gap-2.5 px-4 py-16 text-center">
      <Compass className="size-8 text-subtle-foreground" aria-hidden="true" />
      <h2 className="h2">{t('notFound.title')}</h2>
      <p className="max-w-md text-muted-foreground">{t('notFound.body')}</p>
      <Link to="/" className={buttonVariants({ variant: 'outline', className: 'mt-2' })}>
        {t('notFound.home')}
      </Link>
    </div>
  );
}
