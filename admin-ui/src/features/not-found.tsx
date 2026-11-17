import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Compass } from 'lucide-react';
import { PageMessage } from '@/components/page-message';
import { buttonVariants } from '@/components/ui/button';

export function NotFound() {
  const { t } = useTranslation();
  return (
    <PageMessage
      icon={Compass}
      tone="muted"
      title={t('notFound.title')}
      body={t('notFound.body')}
      actions={
        <Link to="/" className={buttonVariants({ variant: 'outline' })}>
          {t('notFound.home')}
        </Link>
      }
    />
  );
}
