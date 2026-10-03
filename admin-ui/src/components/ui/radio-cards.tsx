import { Radio } from '@base-ui/react/radio';
import { RadioGroup } from '@base-ui/react/radio-group';
import { cn } from '@/lib/utils';

// Radio cards (STYLEGUIDE.md section 8): choices with consequences, each a title
// plus one line saying what it means. Base UI supplies the radiogroup semantics
// and arrow-key movement.

export interface RadioCardOption<V extends string> {
  value: V;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** Extra content on the right (a count, an avatar stack). */
  aside?: React.ReactNode;
  disabled?: boolean;
}

export function RadioCards<V extends string>({
  value,
  onValueChange,
  options,
  label,
  className,
}: {
  value: V | undefined;
  onValueChange: (v: V) => void;
  options: RadioCardOption<V>[];
  /** Accessible name of the group. */
  label: string;
  className?: string;
}) {
  return (
    <RadioGroup
      value={value ?? null}
      onValueChange={(v) => onValueChange(v as V)}
      aria-label={label}
      className={cn('flex flex-col gap-2', className)}
    >
      {options.map((o) => (
        <Radio.Root
          key={o.value}
          value={o.value}
          disabled={o.disabled}
          className="group flex w-full cursor-pointer items-center gap-3 rounded-[12px] border bg-card px-3.5 py-3 text-left transition-colors duration-(--dur-1) outline-none hover:border-border-strong data-checked:border-brand data-checked:bg-[color-mix(in_oklab,var(--brand)_6%,var(--card))] data-disabled:cursor-not-allowed data-disabled:opacity-50"
        >
          <span className="grid size-[18px] shrink-0 place-items-center rounded-full border-[1.5px] border-border-strong group-data-checked:border-brand">
            <Radio.Indicator className="size-2.5 rounded-full bg-brand" />
          </span>
          <span className="flex min-w-0 flex-1 flex-col">
            <b className="font-semibold">{o.title}</b>
            {o.description ? (
              <span className="text-[12.5px] text-muted-foreground">{o.description}</span>
            ) : null}
          </span>
          {o.aside}
        </Radio.Root>
      ))}
    </RadioGroup>
  );
}
