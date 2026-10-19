import { useTranslation } from 'react-i18next';
import { Circle, CircleCheck, CircleDashed, ExternalLink } from 'lucide-react';
import type { BridgeSetup } from '@/api/types';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import {
  knownSetupItem,
  setupItemLabel,
  setupItemLink,
  setupState,
  type SetupState,
} from './setup-model';

const STATE_ICON: Record<SetupState, typeof Circle> = {
  done: CircleCheck,
  todo: Circle,
  optional: CircleDashed,
};

const STATE_TONE: Record<SetupState, string> = {
  done: 'text-success',
  todo: 'text-warning',
  optional: 'text-subtle-foreground',
};

/**
 * The Bridge's setup checklist (its heartbeat's `setup`, BRIDGE-PLAN.md Phase
 * 2b, FIRST-RUN-PLAN.md F2), read-only, in the Bridge's order: each step done,
 * still to do, or optional, with the Bridge's few words about it, and (given
 * the Bridge's page, `uiUrl`) a link to the tab that fixes a step still to do.
 * Settings > Bridge's Setup card shows it, and the first-run flow's "Add a
 * Bridge" step reuses it: it takes only the setup block and the page's address,
 * and fetches nothing itself.
 */
export function BridgeSetupList({
  setup,
  uiUrl,
  className,
}: {
  setup: BridgeSetup;
  uiUrl?: string;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <ul
      className={cn('flex flex-col divide-y', className)}
      aria-label={t('bridge.setup.listLabel')}
    >
      {setup.items.map((item) => {
        const state = setupState(item);
        const Icon = STATE_ICON[state];
        const label = setupItemLabel(item.id);
        const link = state === 'done' ? undefined : setupItemLink(uiUrl, item.id);
        return (
          <li key={item.id} className="flex items-start gap-3 py-2.5">
            <Icon className={cn('mt-0.5 size-4 shrink-0', STATE_TONE[state])} aria-hidden="true" />
            <div className="flex min-w-0 flex-1 flex-col gap-0.5">
              <span className="flex flex-wrap items-center gap-2">
                <span className="font-semibold">
                  {t(label.key, { defaultValue: label.fallback })}
                </span>
                <span className="sr-only">{t(`bridge.setup.state.${state}`)}</span>
                {item.required ? null : (
                  <Badge variant="outline">{t('bridge.setup.optional')}</Badge>
                )}
              </span>
              {item.detail ? (
                <span className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
                  {item.detail}
                </span>
              ) : state !== 'done' && knownSetupItem(item.id) ? (
                <span className="text-[12.5px] text-muted-foreground">
                  {t(`bridge.setup.hint.${item.id}`)}
                </span>
              ) : null}
            </div>
            {link ? (
              <a
                href={link}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex shrink-0 items-center gap-1 text-[12.5px] font-semibold text-brand-ink hover:underline"
                aria-label={t('bridge.setup.fixAria', {
                  step: t(label.key, { defaultValue: label.fallback }),
                })}
              >
                {t('bridge.setup.fix')}
                <ExternalLink className="size-3.5" aria-hidden="true" />
              </a>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
