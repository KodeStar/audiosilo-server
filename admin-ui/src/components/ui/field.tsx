import { cn } from '@/lib/utils';
import { Label } from './label';

/**
 * A form field: label, the control, then either the error (what's wrong and how
 * to fix it) or a muted description. Pass the control's id as `htmlFor`; give
 * the control `aria-describedby={describedBy(id, …)}` (lib/a11y) and `aria-invalid`.
 */
export function Field({
  htmlFor,
  label,
  description,
  error,
  children,
  className,
}: {
  htmlFor: string;
  label: React.ReactNode;
  description?: React.ReactNode;
  error?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn('flex flex-col gap-1.5', className)}>
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {error ? (
        <p
          id={`${htmlFor}-error`}
          role="alert"
          className="text-[12.5px] font-medium text-destructive"
        >
          {error}
        </p>
      ) : description ? (
        <p id={`${htmlFor}-desc`} className="text-[12.5px] text-muted-foreground">
          {description}
        </p>
      ) : null}
    </div>
  );
}

/** A form-level error (not tied to one field), announced when it appears. */
export function FormError({ children }: { children: React.ReactNode }) {
  if (!children) return null;
  return (
    <p role="alert" className="text-[13px] font-medium text-destructive">
      {children}
    </p>
  );
}
