import { cn } from '@/lib/utils';
import { segmentClass, segmentTrack } from './segment-classes';

/** A segmented choice between a few values (pressed buttons, one active). */
export function SegmentedControl<V extends string | number | boolean>({
  value,
  onChange,
  options,
  label,
  className,
}: {
  value: V;
  onChange: (v: V) => void;
  options: readonly { value: V; label: React.ReactNode }[];
  label: string;
  className?: string;
}) {
  return (
    <div role="group" aria-label={label} className={cn(segmentTrack, className)}>
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          aria-pressed={o.value === value}
          onClick={() => onChange(o.value)}
          className={segmentClass(o.value === value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
