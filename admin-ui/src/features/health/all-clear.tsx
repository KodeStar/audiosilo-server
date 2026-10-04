import { useTranslation } from 'react-i18next';
import { CheckCircle2 } from 'lucide-react';

/** An issue list with nothing in it: all clear, or nothing ignored. */
export function AllClear({ ignored }: { ignored: boolean }) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-col items-center gap-2 rounded-xl border bg-card px-6 py-12 text-center">
      <span
        className="mb-1 grid size-12 place-items-center rounded-[14px] bg-success-soft text-success"
        aria-hidden="true"
      >
        <CheckCircle2 className="size-6" />
      </span>
      <h3 className="h3">{ignored ? t('health.ignoredEmpty.title') : t('health.clear.title')}</h3>
      <p className="max-w-[440px] text-muted-foreground">
        {ignored ? t('health.ignoredEmpty.body') : t('health.clear.body')}
      </p>
    </div>
  );
}
