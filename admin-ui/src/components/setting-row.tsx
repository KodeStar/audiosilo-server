/**
 * A row of a settings card or danger zone: a title, a muted line saying what it
 * does, and its control on the right (wrapping under it on a phone). Pass
 * `htmlFor` when the control is a form element, so the title labels it.
 */
export function SettingRow({
  title,
  badges,
  description,
  htmlFor,
  descriptionId,
  children,
}: {
  title: React.ReactNode;
  /** Pills beside the title (a setting's lock or restart badges). */
  badges?: React.ReactNode;
  description?: React.ReactNode;
  htmlFor?: string;
  descriptionId?: string;
  children: React.ReactNode;
}) {
  const label = htmlFor ? (
    <label htmlFor={htmlFor} className="font-semibold">
      {title}
    </label>
  ) : (
    <b className="font-semibold">{title}</b>
  );
  return (
    <div className="flex flex-wrap items-center justify-between gap-4 py-4">
      <div className="flex min-w-0 flex-1 basis-60 flex-col gap-0.5">
        {badges ? (
          <div className="flex flex-wrap items-center gap-2">
            {label}
            {badges}
          </div>
        ) : (
          label
        )}
        {description ? (
          <span id={descriptionId} className="text-[12.5px] text-muted-foreground">
            {description}
          </span>
        ) : null}
      </div>
      {children}
    </div>
  );
}
