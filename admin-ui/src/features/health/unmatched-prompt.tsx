import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Sparkles } from 'lucide-react';
import { useIssues } from '@/api/hooks';
import { Notice } from '@/components/notice';
import { buttonVariants } from '@/components/ui/button';
import { counted } from '@/lib/format';

/**
 * "N books not matched - Match automatically" (server#370), for the pages an
 * admin looks at first (Overview, Library > Books): Match automatically was
 * reachable only from Health > Not matched. It counts what that category counts
 * (the same summary, so the two never disagree: ignored books are left out) and
 * opens it there, where the card says what a run sends before it starts. Shown
 * only while there are such books; the server lists none while community
 * metadata is off.
 */
export function UnmatchedPrompt({ className }: { className?: string }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const issues = useIssues();
  const n = issues.data?.categories.find((c) => c.kind === 'unmatched')?.count ?? 0;
  if (n === 0) return null;
  return (
    <Notice
      tone="info"
      icon={Sparkles}
      className={className}
      title={t('health.unmatchedPrompt.title', counted(n, lang))}
      actions={
        <Link
          to="/health/{-$section}"
          params={{ section: undefined }}
          search={{ issue: 'unmatched' }}
          className={buttonVariants({ variant: 'outline', size: 'sm' })}
        >
          <Sparkles aria-hidden="true" />
          {t('health.bulkMatch.title')}
        </Link>
      }
    >
      {t('health.unmatchedPrompt.body')}
    </Notice>
  );
}
