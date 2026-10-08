import { useTranslation } from 'react-i18next';
import { useImports } from '@/api/hooks';
import type { User } from '@/api/types';
import { Card, CardHeader } from '@/components/ui/card';
import { ImportRow, ImportSettingsLink } from './import-actions';

/**
 * A person's Listening tab: the histories imported into their listening, each
 * with Undo, and the way to Settings > Import. Nothing while they have none
 * (or the list can't be read: the rest of the tab doesn't depend on it).
 */
export function UserImports({ user }: { user: User }) {
  const { t } = useTranslation();
  const imports = useImports(user.id);
  if (!imports.data?.length) return null;
  return (
    <Card aria-labelledby="user-imports-title">
      <CardHeader
        titleId="user-imports-title"
        title={t('imports.user.title')}
        description={t('imports.user.description', { name: user.username })}
        action={<ImportSettingsLink variant="ghost">{t('imports.user.manage')}</ImportSettingsLink>}
      />
      <ul className="flex flex-col divide-y" aria-label={t('imports.user.title')}>
        {imports.data.map((imp) => (
          <ImportRow key={imp.id} imp={imp} showUser={false} />
        ))}
      </ul>
    </Card>
  );
}
