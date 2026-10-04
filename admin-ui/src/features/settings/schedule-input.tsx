import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import {
  formatSchedule,
  parseSchedule,
  WEEKDAYS,
  type Frequency,
  type ScheduleParts,
  type Weekday,
} from './backups-model';

/**
 * A backup schedule as three controls: how often, which day (weekly), what time.
 * value/onChange are the stored form ("", "daily:03:00", "weekly:sun:03:00").
 */
export function ScheduleInput({
  id,
  value,
  onChange,
  disabled,
  describedBy,
  invalid,
}: {
  id: string;
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
  describedBy?: string;
  invalid?: boolean;
}) {
  const { t } = useTranslation();
  const parts = parseSchedule(value);
  const set = (p: Partial<ScheduleParts>) => onChange(formatSchedule({ ...parts, ...p }));
  return (
    <div className="flex flex-wrap items-center gap-2">
      <NativeSelect
        id={id}
        className="w-full sm:w-[150px]"
        value={parts.frequency}
        disabled={disabled}
        onChange={(e) => set({ frequency: e.target.value as Frequency })}
        aria-describedby={describedBy}
        aria-invalid={invalid ? true : undefined}
      >
        {(['off', 'daily', 'weekly'] as const).map((f) => (
          <option key={f} value={f}>
            {t(`backups.frequency.${f}`)}
          </option>
        ))}
      </NativeSelect>
      {parts.frequency === 'weekly' ? (
        <NativeSelect
          className="w-[calc(50%-4px)] sm:w-[140px]"
          value={parts.day}
          disabled={disabled}
          onChange={(e) => set({ day: e.target.value as Weekday })}
          aria-label={t('backups.day')}
        >
          {WEEKDAYS.map((d) => (
            <option key={d} value={d}>
              {t(`backups.weekday.${d}`)}
            </option>
          ))}
        </NativeSelect>
      ) : null}
      {parts.frequency !== 'off' ? (
        <Input
          type="time"
          className="w-[calc(50%-4px)] font-mono text-[12.5px] sm:w-[120px]"
          value={parts.time}
          disabled={disabled}
          required
          onChange={(e) => e.target.value && set({ time: e.target.value })}
          aria-label={t('backups.time')}
        />
      ) : null}
    </div>
  );
}
