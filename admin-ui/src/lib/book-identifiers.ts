/** A normalized ASIN, including older identifiers without a B0 prefix. */
export const ASIN = /^[A-Z0-9]{10}$/;
/** An ISBN-10 or ISBN-13, upper case, without hyphens or spaces. */
export const ISBN = /^(\d{9}[\dX]|\d{13})$/;
