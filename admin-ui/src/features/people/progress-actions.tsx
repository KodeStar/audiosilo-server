import { useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { CalendarDays, CheckCircle2, Ellipsis, History, RotateCcw } from 'lucide-react';
import { buttonVariants } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { ProgressDatesDialog } from './progress-dates-dialog';
import { useEditProgress, type ProgressTarget } from './use-edit-progress';

/**
 * The actions on a person's progress on a book: mark it finished (or not), edit
 * the start and finish dates, see the listening sessions.
 */
export function ProgressMenu({ target: p }: { target: ProgressTarget }) {
  const { t } = useTranslation();
  const edit = useEditProgress();
  const navigate = useNavigate();
  const [dates, setDates] = useState(false);
  const vars = { name: p.username, title: p.title };
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger
          className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
          aria-label={t('progress.actions', vars)}
        >
          <Ellipsis className="size-4" aria-hidden="true" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          {p.finished ? (
            <DropdownMenuItem
              onClick={() => void edit(p, { finished: false }, t('progress.unfinishedDone', vars))}
            >
              <RotateCcw aria-hidden="true" />
              {t('progress.markUnfinished')}
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem
              onClick={() =>
                void edit(p, { finished: true }, t('progress.finishedDone', vars), {
                  finished: false,
                  position: p.position,
                })
              }
            >
              <CheckCircle2 aria-hidden="true" />
              {t('progress.markFinished')}
            </DropdownMenuItem>
          )}
          <DropdownMenuItem onClick={() => setDates(true)}>
            <CalendarDays aria-hidden="true" />
            {t('progress.editDates')}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onClick={() =>
              void navigate({
                to: '/activity/{-$section}',
                params: { section: 'sessions' },
                search: { person: p.userId, library: p.library_id, path: p.path },
              })
            }
          >
            <History aria-hidden="true" />
            {t('progress.sessions')}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <ProgressDatesDialog open={dates} onOpenChange={setDates} target={p} />
    </>
  );
}
