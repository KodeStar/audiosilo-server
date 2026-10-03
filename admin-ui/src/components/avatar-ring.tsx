import { Monogram } from './monogram';

/**
 * A monogram inside a progress ring (STYLEGUIDE.md "Avatar + progress ring"):
 * a --border track with --brand progress. The ring is decorative; the progress
 * is always also said in text nearby.
 */
export function AvatarRing({
  name,
  size,
  progress,
}: {
  name: string;
  size: number;
  /** 0..1 */
  progress: number;
}) {
  const stroke = 3;
  const box = size + stroke * 2 + 4;
  const r = (box - stroke) / 2;
  const c = 2 * Math.PI * r;
  return (
    <span className="relative grid shrink-0 place-items-center" style={{ width: box, height: box }}>
      <svg
        className="absolute inset-0 -rotate-90"
        width={box}
        height={box}
        viewBox={`0 0 ${box} ${box}`}
        aria-hidden="true"
      >
        <circle
          cx={box / 2}
          cy={box / 2}
          r={r}
          fill="none"
          stroke="var(--border)"
          strokeWidth={stroke}
        />
        <circle
          cx={box / 2}
          cy={box / 2}
          r={r}
          fill="none"
          stroke="var(--brand)"
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={`${c * Math.min(1, Math.max(0, progress))} ${c}`}
        />
      </svg>
      <Monogram name={name} size={size} />
    </span>
  );
}
