import { useMemo } from 'react';
import { encode } from 'uqr';

/**
 * A QR code drawn as one SVG path, computed in the browser (uqr): no image
 * request, no inline markup string, so it stays inside the CSP, and the invite
 * code it carries never leaves the page.
 */
export function QrCode({
  text,
  label,
  size = 168,
}: {
  text: string;
  label: string;
  size?: number;
}) {
  const { d, n } = useMemo(() => {
    const { data } = encode(text, { ecc: 'M', border: 2 });
    let path = '';
    data.forEach((row, y) =>
      row.forEach((on, x) => {
        if (on) path += `M${x} ${y}h1v1h-1z`;
      }),
    );
    return { d: path, n: data.length };
  }, [text]);
  return (
    <svg
      role="img"
      aria-label={label}
      viewBox={`0 0 ${n} ${n}`}
      width={size}
      height={size}
      shapeRendering="crispEdges"
      className="shrink-0 rounded-[10px] bg-white"
    >
      <path d={d} fill="#0a0f1e" />
    </svg>
  );
}
