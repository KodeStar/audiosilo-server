import { useTranslation } from 'react-i18next';
import { House } from 'lucide-react';
import type { AdminUser } from '@/api/types';
import { Badge } from '@/components/ui/badge';
import { displayName } from './household-model';

/**
 * A household member's chips: who manages them, and Kid or Grown-up. Nothing
 * for anyone else. The display name leads when it isn't the username.
 */
export function MemberChips({ user: u, owner }: { user: AdminUser; owner: string | undefined }) {
  const { t } = useTranslation();
  const name = displayName(u);
  const shownName = name.toLowerCase() !== u.username.toLowerCase() ? name : undefined;
  if (!u.managed_by && !shownName) return null;
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-1.5">
      {shownName ? (
        <span className="truncate text-[12.5px] font-semibold text-muted-foreground">
          {shownName}
        </span>
      ) : null}
      {u.managed_by ? (
        <Badge variant="outline">
          <House aria-hidden="true" />
          {t('people.badge.managedBy', { name: owner ?? u.managed_by.username })}
        </Badge>
      ) : null}
      {u.member_kind ? (
        <Badge variant="secondary">{t(`people.kind.${u.member_kind}`)}</Badge>
      ) : null}
    </div>
  );
}
