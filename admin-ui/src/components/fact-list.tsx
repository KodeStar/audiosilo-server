/**
 * Label/value pairs in two columns (a server card, About). A value not loaded
 * yet (undefined) shows a placeholder bar.
 */
export function FactList({ rows }: { rows: [string, React.ReactNode][] }) {
  return (
    <dl className="grid grid-cols-[minmax(110px,auto)_1fr] gap-x-[18px] gap-y-2 text-[13px]">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-muted-foreground">{label}</dt>
          <dd className="min-w-0 font-[550] [overflow-wrap:anywhere]">
            {value ?? <span className="skel inline-block h-4 w-16 align-middle" />}
          </dd>
        </div>
      ))}
    </dl>
  );
}
