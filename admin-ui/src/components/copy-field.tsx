import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Copy } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { copyWithToast } from '@/lib/clipboard';

/**
 * A labelled read-only value (a link, a code, an address) with a Copy button
 * beside it; focusing the field selects it all. The toast names it by `label`.
 * `children` sit under the row (a hint).
 */
export function CopyField({
  id,
  label,
  value,
  children,
}: {
  id: string;
  label: string;
  value: string;
  children?: ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <div className="flex gap-2">
        <Input
          id={id}
          readOnly
          value={value}
          className="font-mono text-[12.5px]"
          onFocus={(e) => e.currentTarget.select()}
        />
        <Button type="button" variant="outline" onClick={() => void copyWithToast(value, label)}>
          <Copy aria-hidden="true" />
          {t('common.copy')}
        </Button>
      </div>
      {children}
    </div>
  );
}
