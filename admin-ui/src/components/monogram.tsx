import { hashString, initials } from '@/lib/monogram';
import { cn } from '@/lib/utils';

/**
 * Gradient monogram avatar (STYLEGUIDE.md "Avatar"): two hues derived from the
 * name, Bricolage initials. The gradient is set through the style prop, which
 * React applies via CSSOM and the CSP allows.
 */
export function Monogram({
  name,
  size = 32,
  className,
}: {
  name: string;
  size?: number;
  className?: string;
}) {
  const h = hashString(name);
  const a = h % 360;
  const b = (a + 40 + ((h >>> 9) % 60)) % 360;
  return (
    <span
      aria-hidden="true"
      className={cn(
        'grid shrink-0 place-items-center rounded-full font-display font-bold tracking-[-0.02em] text-white',
        className,
      )}
      style={{
        width: size,
        height: size,
        fontSize: Math.round(size * 0.4),
        background: `linear-gradient(135deg, hsl(${a} 72% 58%), hsl(${b} 70% 42%))`,
      }}
    >
      {initials(name)}
    </span>
  );
}
