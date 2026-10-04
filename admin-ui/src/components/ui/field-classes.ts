// The look Input and Textarea share: border, radius, focus ring, disabled and
// invalid states (STYLEGUIDE.md "Input").
export const fieldClasses = [
  'w-full min-w-0 rounded-md border border-input bg-card text-sm transition-[border-color,box-shadow] duration-(--dur-1) outline-none placeholder:text-subtle-foreground',
  'focus-visible:border-ring focus-visible:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)] focus-visible:outline-none',
  'disabled:bg-muted disabled:text-muted-foreground',
  'aria-invalid:border-destructive aria-invalid:shadow-[0_0_0_3px_color-mix(in_oklab,var(--destructive)_15%,transparent)]',
].join(' ');
