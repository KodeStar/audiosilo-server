import { useMemo } from 'react';
import { coverModel, type CoverModel } from '@/lib/cover-model';
import { cn } from '@/lib/utils';

/**
 * The procedural cover a book without art gets (STYLEGUIDE.md "Cover"): React
 * SVG art plus HTML type, never an innerHTML string, so it stays CSP-clean. The
 * palette rides in CSS variables set through the style prop (CSSOM, which the
 * CSP allows); the type scales with the cover's own width (cqw), so one
 * component works from a 36px row thumbnail to the 300px hero.
 */
export function GeneratedCover({
  title,
  author,
  className,
}: {
  title: string;
  author: string;
  className?: string;
}) {
  const model = useMemo(() => coverModel(title, author), [title, author]);
  const [c1, c2, c3, ink] = model.palette;
  return (
    <div
      className={cn('cover gen-cover', `gen-${model.layout}`, className)}
      role="img"
      aria-label={author ? `${title}, ${author}` : title}
      style={
        {
          '--c1': c1,
          '--c2': c2,
          '--c3': c3,
          '--ink': ink,
          '--glow': `${c2}55`,
        } as React.CSSProperties
      }
    >
      <CoverArt model={model} />
      {model.layout === 'sigil' ? <span className="gen-frame" aria-hidden="true" /> : null}
      <div className="gen-text" aria-hidden="true">
        {author ? <span className="gen-author">{author}</span> : null}
        <span className="gen-title">{title}</span>
      </div>
    </div>
  );
}

/** The art layer: deterministic shapes per layout from the model's seeded random. */
function CoverArt({ model }: { model: CoverModel }) {
  const shapes = useMemo(() => artFor(model), [model]);
  return (
    <svg
      className="gen-art"
      viewBox="0 0 100 100"
      preserveAspectRatio="xMidYMid slice"
      aria-hidden="true"
    >
      <rect width="100" height="100" fill="var(--c1)" />
      {shapes}
    </svg>
  );
}

function artFor({ layout, rand }: CoverModel): React.ReactNode {
  const r = (a: number, b: number) => a + rand() * (b - a);
  const variant = Math.floor(rand() * 3);
  switch (layout) {
    case 'field':
      return variant === 0 ? (
        <>
          <circle cx={r(55, 70)} cy={r(28, 40)} r={r(20, 28)} fill="var(--c2)" />
          <circle cx={r(22, 38)} cy={r(18, 28)} r="4" fill="var(--c3)" />
        </>
      ) : variant === 1 ? (
        <>
          <path d="M22 66 V38 a28 28 0 0 1 56 0 V66Z" fill="var(--c2)" />
          <rect x="47" y="10" width="6" height="6" fill="var(--c3)" transform="rotate(45 50 13)" />
        </>
      ) : (
        <>
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <path
              key={i}
              d={`M-5 ${16 + i * 7} Q 50 ${4 + i * 7} 105 ${16 + i * 7}`}
              stroke="var(--c2)"
              strokeWidth={2.6 - i * 0.3}
              fill="none"
              opacity={1 - i * 0.12}
            />
          ))}
          <circle cx="78" cy="18" r="5" fill="var(--c3)" />
        </>
      );
    case 'band': {
      const angle = r(-28, -14);
      return (
        <>
          <g transform={`rotate(${angle} 50 50)`}>
            <rect x="-20" y={r(24, 34)} width="140" height={r(7, 11)} fill="var(--c3)" />
            <rect x="-20" y={r(42, 46)} width="140" height="0.6" fill="var(--c2)" opacity="0.7" />
          </g>
          <circle
            cx={r(60, 82)}
            cy={r(12, 22)}
            r={r(4, 7)}
            fill="none"
            stroke="var(--c2)"
            strokeWidth="0.6"
          />
        </>
      );
    }
    case 'grotesk':
      return variant === 0 ? (
        <>
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <circle key={i} cx={62 + i * 5.2} cy={44 - i * 3} r={0.7 + i * 0.55} fill="var(--c2)" />
          ))}
        </>
      ) : variant === 1 ? (
        <>
          <rect x="0" y="48" width="100" height="5" fill="var(--c2)" />
          <rect x="0" y="55" width="60" height="2" fill="var(--c3)" opacity="0.6" />
        </>
      ) : (
        <>
          {[0, 1, 2, 3].map((i) => (
            <path
              key={i}
              d={`M100 ${30 + i * 9} A ${30 + i * 9} ${30 + i * 9} 0 0 0 ${70 - i * 9} 0`}
              stroke="var(--c2)"
              strokeWidth="2.4"
              fill="none"
              opacity={1 - i * 0.2}
            />
          ))}
        </>
      );
    case 'sigil':
      return (
        <g stroke="var(--c3)" fill="none" strokeWidth="0.8" opacity="0.5">
          {variant === 0 ? (
            <>
              <path d="M50 30 L64 52 L50 74 L36 52Z" />
              <path d="M50 36 L59 52 L50 68 L41 52Z" opacity="0.6" />
              <line x1="50" y1="22" x2="50" y2="82" />
              <circle cx="50" cy="52" r="20" opacity="0.5" />
            </>
          ) : variant === 1 ? (
            <>
              <circle cx="50" cy="52" r="16" />
              <circle cx="50" cy="52" r="11" opacity="0.6" />
              {Array.from({ length: 12 }, (_, i) => {
                const a = (i * Math.PI) / 6;
                return (
                  <line
                    key={i}
                    x1={50 + Math.cos(a) * 18}
                    y1={52 + Math.sin(a) * 18}
                    x2={50 + Math.cos(a) * 25}
                    y2={52 + Math.sin(a) * 25}
                  />
                );
              })}
            </>
          ) : (
            <>
              <path d="M38 70 L62 34 M62 70 L38 34" />
              <path d="M34 66h8M58 66h8" />
              <circle cx="50" cy="52" r="5" fill="var(--c3)" opacity="0.7" />
              <path d="M30 44 Q50 20 70 44" opacity="0.55" />
            </>
          )}
        </g>
      );
  }
}
