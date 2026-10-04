/**
 * A progress ring (STYLEGUIDE.md "Avatar + progress ring"): a --border track and a
 * coloured arc from the top. Decorative: the number is always said nearby.
 */
export function Ring({
  fraction,
  size,
  stroke,
  color,
  className,
}: {
  /** 0..1 */
  fraction: number;
  size: number;
  stroke: number;
  color: string;
  className?: string;
}) {
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  const f = Math.min(1, Math.max(0, fraction));
  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${size} ${size}`}
      className={className ? `-rotate-90 ${className}` : '-rotate-90'}
      aria-hidden="true"
    >
      <circle
        cx={size / 2}
        cy={size / 2}
        r={r}
        fill="none"
        stroke="var(--border)"
        strokeWidth={stroke}
      />
      {f > 0 ? (
        <circle
          cx={size / 2}
          cy={size / 2}
          r={r}
          fill="none"
          stroke={color}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={`${c * f} ${c}`}
        />
      ) : null}
    </svg>
  );
}
