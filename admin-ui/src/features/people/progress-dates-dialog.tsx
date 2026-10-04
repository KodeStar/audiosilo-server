import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { CalendarDays } from 'lucide-react';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field, FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { dateInputValue, datesEdit, datesProblem } from './people-model';
import { useEditProgress, type ProgressTarget } from './use-edit-progress';

/**
 * Sets or clears when a person started and finished a book. The finish date is
 * only for a finished book; dates can't be in the future or out of order.
 */
export function ProgressDatesDialog({
  open,
  onOpenChange,
  target,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  target: ProgressTarget;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={CalendarDays}
        title={t('progress.dates.title', { name: target.username, title: target.title })}
        description={t('progress.dates.description')}
      >
        {open ? <DatesForm target={target} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function DatesForm({ target, onDone }: { target: ProgressTarget; onDone: () => void }) {
  const { t } = useTranslation();
  const before = {
    started: dateInputValue(target.started_at),
    finished: dateInputValue(target.finished_at),
  };
  const finished = target.finished;
  const edit = useEditProgress();
  const [started, setStarted] = useState(before.started);
  const [ended, setEnded] = useState(before.finished);
  const [busy, setBusy] = useState(false);
  const today = dateInputValue(new Date().toISOString());
  const problem = datesProblem(started, finished ? ended : '', today);
  const change = datesEdit(before, { started, finished: finished ? ended : before.finished });

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (problem) return;
    if (!Object.keys(change).length) return onDone();
    setBusy(true);
    const ok = await edit(
      target,
      change,
      t('progress.dates.saved', { name: target.username, title: target.title }),
    );
    setBusy(false);
    if (ok) onDone();
  };

  return (
    <form onSubmit={(e) => void submit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <Field htmlFor="progress-started" label={t('progress.dates.started')}>
          <Input
            id="progress-started"
            type="date"
            max={today}
            value={started}
            onChange={(e) => setStarted(e.target.value)}
          />
        </Field>
        <Field
          htmlFor="progress-finished"
          label={t('progress.dates.finished')}
          description={finished ? undefined : t('progress.dates.notFinished')}
        >
          <Input
            id="progress-finished"
            type="date"
            max={today}
            min={started || undefined}
            value={finished ? ended : ''}
            disabled={!finished}
            aria-describedby={finished ? undefined : 'progress-finished-desc'}
            onChange={(e) => setEnded(e.target.value)}
          />
        </Field>
        <FormError>{problem ? t(problem) : null}</FormError>
      </DialogBody>
      <DialogFormFooter busy={busy} disabled={!!problem} submitLabel={t('progress.dates.save')} />
    </form>
  );
}
