import { Monogram } from './monogram';
import { Ring } from './ring';

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
  return (
    <span className="relative grid shrink-0 place-items-center" style={{ width: box, height: box }}>
      <Ring
        fraction={progress}
        size={box}
        stroke={stroke}
        color="var(--brand)"
        className="absolute inset-0"
      />
      <Monogram name={name} size={size} />
    </span>
  );
}
