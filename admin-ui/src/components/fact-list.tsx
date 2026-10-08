import { cn } from '@/lib/utils';

/**
 * Label/value pairs (a server card, About, an import's summary). `aligned`
 * (the default) puts the labels in one column beside the values; `flow` makes
 * each pair a ruled row, label left and value right, in two columns from `sm`.
 * A value not loaded yet (undefined) shows a placeholder bar.
 */
export function FactList({
  rows,
  layout = 'aligned',
}: {
  rows: [string, React.ReactNode][];
  layout?: 'aligned' | 'flow';
}) {
  const flow = layout === 'flow';
  return (
    <dl
      className={cn(
        'grid gap-y-2 text-[13px]',
        flow
          ? 'grid-cols-1 gap-x-6 sm:grid-cols-2'
          : 'grid-cols-[minmax(110px,auto)_1fr] gap-x-[18px]',
      )}
    >
      {rows.map(([label, value]) => (
        <div
          key={label}
          className={flow ? 'flex min-w-0 justify-between gap-3 border-b py-1.5' : 'contents'}
        >
          <dt className="text-muted-foreground">{label}</dt>
          <dd
            className={cn(
              'min-w-0 font-[550] [overflow-wrap:anywhere]',
              flow && 'text-right tabular-nums',
            )}
          >
            {value ?? <span className="skel inline-block h-4 w-16 align-middle" />}
          </dd>
        </div>
      ))}
    </dl>
  );
}
