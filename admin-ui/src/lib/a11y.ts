// Ids that tie a form control to the message <Field> renders under it.

/** The aria-describedby value for a control inside <Field id=…>: its error, else its description. */
export function describedBy(id: string, hasError: boolean, hasDescription: boolean) {
  if (hasError) return `${id}-error`;
  return hasDescription ? `${id}-desc` : undefined;
}
