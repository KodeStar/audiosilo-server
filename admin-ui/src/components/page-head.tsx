/** A page's display title, a muted line under it, and the page's main action. */
export function PageHead({
  title,
  description,
  action,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div className="flex min-w-0 flex-col gap-1.5">
        <h1 className="display [overflow-wrap:anywhere]">{title}</h1>
        {description ? <p className="text-[15px] text-muted-foreground">{description}</p> : null}
      </div>
      {action}
    </div>
  );
}
